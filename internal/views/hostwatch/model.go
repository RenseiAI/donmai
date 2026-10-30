package hostwatch

import (
	"context"
	"errors"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/RenseiAI/donmai/afclient"
	"github.com/RenseiAI/donmai/agent"
	"github.com/RenseiAI/tui-components/format"
	"github.com/RenseiAI/tui-components/theme"
	"github.com/charmbracelet/x/ansi"
)

// Default cadences. The index poll is deliberately low-frequency (a
// localhost JSON GET against an in-memory daemon snapshot); the tail poll is
// the responsiveness knob for the live stream. Both are coalesced — one
// timer each, not one per session — so the watcher stays nearly free at tens
// of concurrent local projects.
const (
	defaultIndexInterval = 1500 * time.Millisecond
	defaultTailInterval  = 350 * time.Millisecond
	// maxEventsPerTailerTick bounds how many events one session's tailer
	// may contribute per tail tick. Every tailer is polled every tick, so
	// one session's long backlog can neither starve the others nor stall
	// the render loop, and a freshly attached watcher catches up on each
	// session's current run within a tick or two instead of one session
	// per tick.
	maxEventsPerTailerTick = 16 * maxEventsPerPoll
)

// HandleEnricher maps a session id to its display handle. It is an
// optional embedder-supplied hook: the handle is opaque text supplied
// through the command context, never fetched over the network. A nil or
// empty result means the session has no handle and the card falls back
// to its issue identifier, then its short session id. The hook may be
// called concurrently with background updates filling the handle map, so
// implementations must be safe for concurrent use.
type HandleEnricher interface {
	Handle(sessionID string) string
}

// handleEnricherKey is the context key carrying the optional handle hook.
type handleEnricherKey struct{}

// WithHandleEnricher returns a context carrying the handle hook the
// host-watch model reads for session display names.
func WithHandleEnricher(ctx context.Context, h HandleEnricher) context.Context {
	return context.WithValue(ctx, handleEnricherKey{}, h)
}

// HandleEnricherFrom returns the handle hook carried by ctx, or nil when
// no hook was supplied. A nil hook leaves every existing render unchanged.
func HandleEnricherFrom(ctx context.Context) HandleEnricher {
	h, _ := ctx.Value(handleEnricherKey{}).(HandleEnricher)
	return h
}

// HostActions is the narrow lifecycle-mutation surface the host-watch
// model needs. *afclient.DaemonClient satisfies it; tests inject a fake.
// Every method runs off the render loop in a Bubble Tea command with a
// bounded context. The daemon owns lifecycle truth: an accepted command
// reports the response (or the sanitized error) and returns a fresh index
// poll — it never edits cards or counters directly.
type HostActions interface {
	PauseContext(ctx context.Context) (*afclient.DaemonActionResponse, error)
	ResumeContext(ctx context.Context) (*afclient.DaemonActionResponse, error)
	DrainContext(ctx context.Context, timeoutSeconds int) (*afclient.DaemonActionResponse, error)
	StopSessionContext(ctx context.Context, sessionID string) (*afclient.DaemonActionResponse, error)
}

// DefaultDrainTimeoutSeconds is the explicit drain deadline the model
// sends when the command does not configure one. It mirrors the daemon's
// own default so the request body and the command context carry the same
// bound either way.
const DefaultDrainTimeoutSeconds = 30

// maxHostActionNoticeRunes bounds every rendered lifecycle outcome line.
const maxHostActionNoticeRunes = 160

// Options configures a hostwatch Model.
type Options struct {
	// Source is the local data layer (daemon index + state.json). Required.
	Source *Source
	// Handles optionally maps session ids to display handles. Nil allowed:
	// without it every label renders exactly as before. When set, the
	// model reads it once per card on every index snapshot (never per
	// tail event), so an embedder may fill its map in the background and
	// later polls pick the handle up.
	Handles HandleEnricher
	// Theme is the active theme; zero value uses theme.DefaultTheme().
	Theme theme.Theme
	// ProjectLabel is the header scope label (e.g. the project slug or repo).
	ProjectLabel string
	// HostLabel is the header host label (e.g. the hostname).
	HostLabel string
	// Plain renders without color/box drawing (CI / non-TTY). The model
	// still runs the full data path; only rendering changes.
	Plain bool
	// UnboundedCards retains complete cards in piped plain output.
	// Interactive callers keep viewport limits.
	UnboundedCards bool
	// Replay, when true, reads each session's events.jsonl from the top
	// (history) instead of seeking to the end. Off by default (steady-state
	// "new output only").
	Replay bool
	// IndexInterval / TailInterval override the default cadences (tests set
	// these tiny). Zero uses the defaults.
	IndexInterval time.Duration
	TailInterval  time.Duration
	// Now overrides the clock (tests). Zero uses time.Now.
	Now func() time.Time
	// Actions performs host lifecycle mutations (pause, resume,
	// deadline-bound drain, per-session stop) against the local daemon's
	// authenticated control API. Nil keeps the dashboard a pure reader:
	// action keys are ignored. Tests inject a fake; the command passes
	// the production daemon client.
	Actions HostActions
	// ActionContext bounds lifecycle commands. Nil uses
	// context.Background; the command passes its own context so
	// cancellation ends in-flight actions with the program.
	ActionContext context.Context
	// DrainTimeoutSeconds is the explicit deadline sent in the drain
	// request body and its command context. Zero uses
	// DefaultDrainTimeoutSeconds.
	DrainTimeoutSeconds int
}

// indexTickMsg drives the daemon index poll.
type indexTickMsg struct{}

// tailTickMsg drives the per-session events.jsonl tail poll.
type tailTickMsg struct{}

// snapshotMsg carries the result of one index poll (computed off the render
// path in a tea.Cmd goroutine).
type snapshotMsg struct{ snap Snapshot }

// tailBatchMsg carries the events read from all active tailers this tick.
type tailBatchMsg struct{ events []TailEvent }

// Cards/stream split. The grid and the merged stream share the content
// height 50/50 by default; the operator adjusts the ratio at runtime with
// [ ] (0 resets). The ratio lives on the model so a terminal resize
// preserves it — only an explicit keypress changes it.
const (
	// defaultSplitRatio is the initial fraction of content height given to
	// the session-card grid (the stream takes the rest).
	defaultSplitRatio = 0.5
	// minSplitRatio / maxSplitRatio bound the adjustable split so neither
	// pane can be squeezed away entirely.
	minSplitRatio = 0.2
	maxSplitRatio = 0.8
	// splitStep is the per-keypress split adjustment.
	splitStep = 0.1
	// minGridHeight is the minimum grid height in rows.
	minGridHeight = 1
	// minStreamHeight is the minimum stream height in rows: the stream
	// title row plus one body row.
	minStreamHeight = 2
)

// Model is the host-watch fleet dashboard Bubble Tea model. It owns the
// merged stream pane, the per-session tailers, and the card grid. It is a pure
// reader: every data path is local (daemon control API + on-disk files).
type Model struct {
	src   *Source
	theme theme.Theme
	opts  Options
	now   func() time.Time

	indexInterval time.Duration
	tailInterval  time.Duration

	width, height int

	cards    []SessionCard
	counters Counters
	cursor   int
	snapErr  error

	// tailers is keyed by session id. A session that disappears from the
	// index (terminal + reaped) or whose tailer reports Done is dropped.
	tailers  map[string]*Tailer
	prefixes *prefixIndex

	// labels maps session id → its stream/card label (issue id when known)
	// so the merged stream stays labeled even after a session ends.
	labels map[string]string

	stream *streamPane
	frame  int

	// tailPolling is true from a tail tick that starts a poll until that
	// poll's batch is applied, so polls never overlap.
	tailPolling bool

	// split is the fraction of content height given to the session-card
	// grid (the stream takes the rest). Operator-adjustable via [ ] / 0.
	split float64

	// detail, when true, replaces the stream pane with the selected
	// session's full detail (enter toggles, esc closes).
	detail bool

	// help, when true, renders the contextual keyboard help overlay
	// over the content area. "?" opens and closes it; esc closes it.
	// While open the model consumes action keys so they cannot leak
	// into selection, filter, split or lifecycle state, and the
	// underlying detail/filter/cursor/split/follow state is preserved
	// across dismissal. helpScroll is the first visible help row.
	help       bool
	helpScroll int

	// filterActive, when true, narrows the visible card grid to the
	// cards matching filterQuery. The source snapshot (m.cards),
	// counters, tailers, and the merged stream are never affected:
	// filtering is a derived card-grid projection only.
	filterActive bool
	// filterQuery is the live filter text typed after "/". Empty
	// matches every card; clearing restores snapshot order.
	filterQuery string
	// actions performs host lifecycle mutations (nil = pure reader).
	actions HostActions
	// actionCtx bounds lifecycle commands (never nil after New).
	actionCtx context.Context
	// drainTimeoutSecs is the explicit drain deadline in seconds.
	drainTimeoutSecs int
	// confirmKind is the lifecycle action awaiting confirmation (""
	// when idle): "stop" for a per-session stop, "drain" for a host
	// drain. The stop key sets "stop" with confirmStopID; the drain key
	// sets "drain". y confirms with one request; n/esc or a selection
	// change returns to idle with no request. Draining stops every
	// session still running past the explicit deadline, so it shares
	// the gate instead of firing on one keypress.
	confirmKind string
	// confirmStopID is the session id awaiting stop confirmation
	// (valid only when confirmKind == "stop").
	confirmStopID string
	// actionBusy is true from an accepted action's dispatch until its
	// result returns; duplicate action keys are ignored meanwhile.
	actionBusy bool
	// notice is the last bounded lifecycle outcome (response or
	// sanitized error), rendered with the help row until the next
	// action replaces it.
	notice string
}

// New constructs a host-watch Model.
func New(opts Options) *Model {
	t := opts.Theme
	if (t == theme.Theme{}) {
		t = theme.DefaultTheme()
	}
	now := opts.Now
	if now == nil {
		now = time.Now
	}
	actionCtx := opts.ActionContext
	if actionCtx == nil {
		actionCtx = context.Background()
	}
	drainSecs := opts.DrainTimeoutSeconds
	if drainSecs <= 0 {
		drainSecs = DefaultDrainTimeoutSeconds
	}
	idx := opts.IndexInterval
	if idx <= 0 {
		idx = defaultIndexInterval
	}
	tail := opts.TailInterval
	if tail <= 0 {
		tail = defaultTailInterval
	}
	return &Model{
		src:              opts.Source,
		theme:            t,
		opts:             opts,
		now:              now,
		indexInterval:    idx,
		tailInterval:     tail,
		tailers:          map[string]*Tailer{},
		prefixes:         newPrefixIndex(),
		labels:           map[string]string{},
		stream:           newStreamPane(),
		split:            defaultSplitRatio,
		actions:          opts.Actions,
		actionCtx:        actionCtx,
		drainTimeoutSecs: drainSecs,
	}
}

// Init kicks off the first index poll and starts both tick timers.
func (m *Model) Init() tea.Cmd {
	return tea.Batch(m.pollIndex(), m.indexTick(), m.tailTick())
}

func (m *Model) indexTick() tea.Cmd {
	return tea.Tick(m.indexInterval, func(time.Time) tea.Msg { return indexTickMsg{} })
}

func (m *Model) tailTick() tea.Cmd {
	return tea.Tick(m.tailInterval, func(time.Time) tea.Msg { return tailTickMsg{} })
}

// pollIndex runs Source.Snapshot off the render path.
func (m *Model) pollIndex() tea.Cmd {
	src := m.src
	return func() tea.Msg { return snapshotMsg{snap: src.Snapshot()} }
}

// pollTails drains every active tailer, each up to maxEventsPerTailerTick
// events. It runs in a tea.Cmd goroutine; the tailers are not touched
// elsewhere concurrently because all access is serialized through the
// Bubble Tea update loop (this Cmd is the only reader, and tailer
// registration happens on snapshotMsg, also on the loop). To keep that
// invariant the Cmd captures the current tailer set by value.
func (m *Model) pollTails() tea.Cmd {
	// Snapshot the tailer pointers under the loop so the goroutine reads a
	// stable set. Each *Tailer is internally locked, so concurrent Poll from
	// only this single goroutine is safe.
	tailers := make([]*Tailer, 0, len(m.tailers))
	for _, t := range m.tailers {
		tailers = append(tailers, t)
	}
	// Deterministic order so the merged stream is stable for a given tick.
	sort.Slice(tailers, func(i, j int) bool { return tailers[i].SessionID() < tailers[j].SessionID() })
	return func() tea.Msg {
		var batch []TailEvent
		for _, t := range tailers {
			for budget := maxEventsPerTailerTick; budget > 0; {
				evs, err := t.Poll()
				if err != nil {
					break // I/O hiccup — skip this tailer this tick
				}
				for i := range evs {
					evs[i].source = t
				}
				batch = append(batch, evs...)
				budget -= len(evs)
				if len(evs) < maxEventsPerPoll {
					break // drained to the end of the journal
				}
			}
		}
		// Sort merged events by ingestion time so interleaving reads across
		// tailers stay chronological in the stream.
		sort.SliceStable(batch, func(i, j int) bool { return batch[i].At.Before(batch[j].At) })
		return tailBatchMsg{events: batch}
	}
}

// Update handles all messages.
func (m *Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		// Pane heights derive from the split ratio at render time, so a
		// resize keeps the operator's ratio without any recomputation.
		m.width = msg.Width
		m.height = msg.Height
		m.clampHelpScroll()
		return m, nil

	case indexTickMsg:
		m.frame++
		return m, tea.Batch(m.pollIndex(), m.indexTick())

	case tailTickMsg:
		// One tail poll at a time. A catch-up poll can outlast the tick; a
		// second poll racing it over the same tailers would deliver one
		// session's events out of order, folding an older activity, cost or
		// turn count over a newer one. While a poll is in flight the tick
		// only reschedules itself.
		if m.tailPolling {
			return m, m.tailTick()
		}
		m.tailPolling = true
		return m, tea.Batch(m.pollTails(), m.tailTick())

	case snapshotMsg:
		m.applySnapshot(msg.snap)
		m.clampHelpScroll()
		return m, nil

	case tailBatchMsg:
		m.tailPolling = false
		m.applyTailBatch(msg.events)
		return m, nil

	case hostActionResultMsg:
		m.actionBusy = false
		m.notice = formatHostActionResult(msg)
		return m, m.pollIndex()

	case tea.KeyPressMsg:
		return m.handleKey(msg)
	case tea.MouseClickMsg, tea.MouseWheelMsg, tea.MouseMotionMsg, tea.MouseReleaseMsg:
		// The dashboard has no mouse bindings: mouse reports are
		// accepted and ignored so a stray click never mutates state.
		return m, nil
	}
	return m, nil
}

func (m *Model) handleKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	// A lifecycle confirmation is pending: y confirms with one request;
	// n/esc or a selection change returns to idle with no request. Any
	// other key is swallowed with the gate still armed, so "/" cannot
	// start filtering mid-confirm. Quit still quits. Selection moves
	// within the visible (filtered) cards so y always stops the card
	// under the cursor.
	if m.confirmKind != "" {
		key := msg.String()
		switch key {
		case "ctrl+c", "q":
			return m, tea.Quit
		case "y":
			kind := m.confirmKind
			id := m.confirmStopID
			m.confirmKind, m.confirmStopID = "", ""
			if kind == "drain" {
				return m, m.startDrain()
			}
			if m.selectedVisibleID() != id {
				return m, nil // selection moved: idle, no request
			}
			return m, m.startStopSession(id)
		case "n", "esc":
			m.confirmKind, m.confirmStopID = "", ""
			return m, nil
		case "up", "k":
			m.confirmKind, m.confirmStopID = "", "" // selection change: idle, no request
			if m.cursor > 0 {
				m.cursor--
			}
			return m, nil
		case "down", "j":
			m.confirmKind, m.confirmStopID = "", "" // selection change: idle, no request
			if m.cursor < len(m.visibleCards())-1 {
				m.cursor++
			}
			return m, nil
		default:
			return m, nil // swallow: only an explicit key resolves the gate
		}
	}
	if m.filterActive {
		// While help is open the overlay owns every key, including
		// "?": route there before the filter consumes printable input.
		if m.help {
			return m.handleHelpKey(msg)
		}
		if _, done := m.handleFilterKey(msg); done {
			return m, nil
		}
		// Printable keys (including q, f, x, d, and navigation letters) are
		// consumed as filter text while filtering and never reach the
		// global shortcuts below. Only non-printable keys fall through.
		if msg.Text != "" {
			return m, nil
		}
	}
	// The help overlay intercepts after confirmation (a pending gate
	// outranks help: "?" stays ignored while confirming) and before
	// filtering, so "?" opens help instead of becoming query text.
	// When neither confirmation nor help is active, "?" also opens
	// help from inside an active filter via the filter handler.
	if m.help {
		return m.handleHelpKey(msg)
	}
	if msg.String() == "?" {
		m.help = true
		m.helpScroll = 0
		m.clampHelpScroll()
		return m, nil
	}
	key := msg.String()
	switch key {
	case "ctrl+c", "q":
		return m, tea.Quit
	case "/":
		m.filterActive = true
		m.filterQuery = ""
		// Entering with an empty query shows every card; keep the
		// current selection by id (a no-op when unfiltered).
		m.reconcileVisibleCursor(m.selectedVisibleID())
		return m, nil
	case "up", "k":
		if m.cursor > 0 {
			m.cursor--
		}
	case "down", "j":
		if m.cursor < len(m.visibleCards())-1 {
			m.cursor++
		}
	case "enter":
		vis := m.visibleCards()
		m.detail = !m.detail && len(vis) > 0 && m.cursor >= 0 && m.cursor < len(vis)
	case "esc":
		m.detail = false
	case "f":
		m.stream.SetFollowing(!m.stream.Following())
	case "g":
		m.stream.SetFollowing(true)
	case "[":
		m.setSplit(m.split - splitStep)
	case "]":
		m.setSplit(m.split + splitStep)
	case "0":
		m.setSplit(defaultSplitRatio)
	case "x":
		return m, m.requestStopConfirm()
	case "p":
		return m, m.startPause()
	case "r":
		return m, m.startResume()
	case "d":
		return m, m.requestDrainConfirm()
	}
	return m, nil
}

// handleHelpKey routes keys while the help overlay is open. It returns
// the model with every key consumed: "?" toggles help closed, esc closes
// it, ctrl+c quits, up/down (and j/k) scroll the overlay, and all other
// keys are swallowed so selection, follow, split, filtering and lifecycle
// state stay exactly as they were. Dismissal preserves detail, filter,
// cursor, split and follow.
func (m *Model) handleHelpKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	if msg.Code == tea.KeyEscape || msg.String() == "esc" || msg.String() == "escape" {
		m.help = false
		m.helpScroll = 0
		return m, nil
	}
	// "?" toggles help closed from every context, including an active
	// filter: it must be checked before the filter handler below would
	// consume it as query text. While filtering, "?" arrives here
	// (via handleKey's help branch) and also via handleFilterKey, so
	// both paths converge on closing with state preserved.
	if msg.String() == "?" {
		m.help = false
		m.helpScroll = 0
		return m, nil
	}
	// "j"/"k" scroll the overlay (vim-style), mirroring up/down; the
	// underlying cursor never moves.
	switch msg.String() {
	case "ctrl+c", "q":
		return m, tea.Quit
	case "up":
		if m.helpScroll > 0 {
			m.helpScroll--
		}
		return m, nil
	case "down":
		m.helpScroll++
		m.clampHelpScroll()
		return m, nil
	case "k":
		if m.helpScroll > 0 {
			m.helpScroll--
		}
		return m, nil
	case "j":
		m.helpScroll++
		m.clampHelpScroll()
		return m, nil
	default:
		return m, nil
	}
}

// clampHelpScroll bounds the overlay scroll offset to the rows the
// current frame can show. Zero height or no help rows pins it at zero.
func (m *Model) clampHelpScroll() {
	limit := len(m.helpRows(m.helpContentWidth())) - m.helpBodyHeight()
	if limit < 0 {
		limit = 0
	}
	if m.helpScroll > limit {
		m.helpScroll = limit
	}
	if m.helpScroll < 0 {
		m.helpScroll = 0
	}
}

// helpContentWidth is the overlay's text width: the terminal width
// minus its two border cells, floored so tiny terminals stay non-empty.
func (m *Model) helpContentWidth() int {
	w := m.width - 2
	if w < 1 {
		w = 1
	}
	return w
}

// helpBodyHeight is the overlay rows available for help text: the
// content rows minus its title, hint and border rows, floored at zero
// so short terminals scroll instead of overflowing.
func (m *Model) helpBodyHeight() int {
	h := m.height - 2
	if h < 0 {
		h = 0
	}
	// Title row + closing hint row + top/bottom border rows.
	h -= 4
	if h < 0 {
		h = 0
	}
	return h
}

// helpEntry is one contextual help row: the key and its description.
type helpEntry struct {
	key  string
	desc string
}

// helpEntries lists the overlay rows for the current context: the
// selection, detail, split, follow, filtering, quit and lifecycle
// controls, plus filter-text precedence, the stop/drain confirmation
// keys and the no-mouse-bindings note. Detail and filter rows reflect
// the context help was opened over. Every key named here is handled by
// handleKey/handleFilterKey/handleHelpKey; the command help names the
// same set (see the contract test in afcli).
func (m *Model) helpEntries() []helpEntry {
	entries := []helpEntry{
		{"↑↓/jk", "select a session card"},
		{"enter", "show the selected session detail (esc closes detail)"},
		{"esc", "close this help, then detail, then the filter"},
		{"[ ]", "move the cards/stream split"},
		{"0", "reset the split to 50/50 (survives resizes)"},
		{"f", "pause or resume the stream"},
		{"g", "jump to the stream tail"},
		{"/", "filter cards (issue, title, repository, work type)"},
		{"backspace", "delete one filter character while filtering"},
		{"x", "ask to stop the selected session"},
		{"d", "ask to drain the host past an explicit deadline"},
		{"y / n", "confirm / cancel the pending stop or drain"},
		{"p", "pause host admissions"},
		{"r", "reopen host admissions"},
		{"q", "quit the dashboard (sessions keep running)"},
		{"ctrl+c", "quit the dashboard"},
	}
	if m.detail {
		entries = append(entries, helpEntry{"detail open", "enter toggles detail; esc closes this help first"})
	}
	if m.filterActive {
		query := m.filterQuery
		if query == "" {
			query = "(empty shows every card)"
		}
		entries = append(entries, helpEntry{"filtering: /" + query, "typed text narrows cards; shortcuts are filter text"})
	} else {
		entries = append(entries, helpEntry{"filter precedence", "while filtering, keys type query text, not shortcuts"})
	}
	entries = append(entries, helpEntry{"mouse", "no mouse bindings: mouse input is ignored"})
	return entries
}

// helpRows wraps every help entry's description to the content width so
// each row fits the terminal. Keys stay whole; long descriptions wrap
// onto continuation rows indented under the key column.
func (m *Model) helpRows(width int) []string {
	if width < 1 {
		width = 1
	}
	entries := m.helpEntries()
	keyW := 0
	for _, e := range entries {
		if w := len([]rune(e.key)); w > keyW {
			keyW = w
		}
	}
	prefix := keyW + 3 // key column + " · " separator
	var rows []string
	for _, e := range entries {
		pad := ""
		if d := keyW - len([]rune(e.key)); d > 0 {
			pad = strings.Repeat(" ", d)
		}
		first := e.key + pad + " · " + e.desc
		rows = append(rows, wrapHelpRow(first, width, strings.Repeat(" ", prefix))...)
	}
	return rows
}

// wrapHelpRow folds one row to width on cell boundaries, indenting
// continuation rows with indent. Short rows pass through unchanged.
// The fold counts terminal cells (wide CJK/emoji wrap by cells, not
// bytes) and prefers word boundaries so descriptions stay readable.
func wrapHelpRow(row string, width int, indent string) []string {
	if width < 1 {
		return []string{row}
	}
	var out []string
	rest := row
	first := true
	for {
		if ansiWidth(rest) <= width {
			out = append(out, rest)
			return out
		}
		// Longest cell-fitting byte prefix; the loop in cellPrefixLen
		// already backs off whole runes.
		cutBytes := cellPrefixLen(rest, width)
		if cutBytes < 1 {
			cutBytes = cellPrefixLen(rest, 1)
		}
		if cutBytes < 1 || cutBytes > len(rest) {
			cutBytes = len(rest)
		}
		// Prefer a word boundary so descriptions stay readable.
		if sp := lastSpaceBefore(rest, width); sp > 0 {
			cutBytes = sp
		}
		out = append(out, strings.TrimRight(rest[:cutBytes], " "))
		rest = strings.TrimLeft(rest[cutBytes:], " ")
		if rest == "" {
			return out
		}
		if first {
			rest = indent + rest
			first = false
		}
	}
}

func cellPrefixLen(s string, width int) int {
	n := 0
	for i := range s {
		if ansi.StringWidth(s[:i]) > width {
			break
		}
		n = i
	}
	if ansi.StringWidth(s) <= width {
		return len(s)
	}
	// Back off one rune so the prefix fits: i indexes byte offsets.
	for n > 0 && ansi.StringWidth(s[:n]) > width {
		_, size := utf8.DecodeLastRuneInString(s[:n])
		n -= size
	}
	return n
}

func lastSpaceBefore(s string, width int) int {
	prefix := s[:cellPrefixLen(s, width)]
	return strings.LastIndex(prefix, " ")
}

func ansiWidth(s string) int {
	return ansi.StringWidth(s)
}

// renderHelpOverlay renders the contextual help overlay bounded to the
// terminal: a bordered panel with a title, the scroll window of wrapped
// help rows, and a closing hint. It never exceeds the frame width or
// height and scrolls short terminals so every row stays reachable.
func (m *Model) renderHelpOverlay() string {
	width := m.width
	if width < 1 {
		width = 1
	}
	contentW := m.helpContentWidth()
	bodyH := m.helpBodyHeight()
	rows := m.helpRows(contentW)
	m.clampHelpScroll()
	start := m.helpScroll
	if start > len(rows) {
		start = len(rows)
	}
	end := start + bodyH
	if end > len(rows) {
		end = len(rows)
	}
	visible := rows[start:end]
	// Pad the body so the panel keeps its frame rows on tall terminals.
	for len(visible) < bodyH {
		visible = append(visible, "")
	}
	title := "keyboard help  (? closes)"
	hint := "↑↓ scroll · esc close"
	lines := make([]string, 0, len(visible)+4)
	top := "╭" + strings.Repeat("─", contentW) + "╮"
	bottom := "╰" + strings.Repeat("─", contentW) + "╯"
	lines = append(lines, truncateWidth(top, width))
	lines = append(lines, truncateWidth("│"+fitCells(title, contentW)+"│", width))
	for _, r := range visible {
		lines = append(lines, truncateWidth("│"+fitCells(r, contentW)+"│", width))
	}
	lines = append(lines, truncateWidth("│"+fitCells(hint, contentW)+"│", width))
	lines = append(lines, truncateWidth(bottom, width))
	if m.opts.Plain {
		return strings.Join(lines, "\n")
	}
	out := make([]string, 0, len(lines))
	for _, ln := range lines {
		out = append(out, lipgloss.NewStyle().Foreground(m.theme.TextTertiary).Render(ln))
	}
	return strings.Join(out, "\n")
}

// handleFilterKey routes keys while the card filter is active. It returns
// true when the key was consumed as filter input (or filter navigation)
// and no global shortcut should run. Printable text appends to the query;
// backspace deletes one rune; escape clears the query and exits filtering.
// Arrow navigation and enter operate on the visible slice; ctrl+c still
// quits and is left for the global handler (false).
func (m *Model) handleFilterKey(msg tea.KeyPressMsg) (tea.Model, bool) {
	// "?" opens the help overlay instead of becoming query text. It is
	// intercepted here (filter state stays intact underneath) because
	// the global help check runs after this handler consumes printable
	// input; without this branch "?" would type into the query.
	if msg.String() == "?" {
		m.help = true
		m.helpScroll = 0
		m.clampHelpScroll()
		return m, true
	}
	// Match by name and by code: String() is the documented matcher, Code
	// is the foolproof fallback for special keys.
	if msg.Code == tea.KeyEscape || msg.String() == "esc" || msg.String() == "escape" {
		keep := m.selectedVisibleID()
		m.filterQuery = ""
		m.filterActive = false
		m.detail = false
		m.reconcileVisibleCursor(keep)
		return m, true
	}
	if msg.Code == tea.KeyBackspace || msg.String() == "backspace" {
		keep := m.selectedVisibleID()
		m.filterQuery = deleteLastRune(m.filterQuery)
		m.reconcileVisibleCursor(keep)
		return m, true
	}
	switch msg.String() {
	case "up":
		if m.cursor > 0 {
			m.cursor--
		}
		return m, true
	case "down":
		if m.cursor < len(m.visibleCards())-1 {
			m.cursor++
		}
		return m, true
	case "enter":
		vis := m.visibleCards()
		m.detail = !m.detail && len(vis) > 0 && m.cursor >= 0 && m.cursor < len(vis)
		return m, true
	case "ctrl+c":
		return m, false
	}
	if msg.Text != "" {
		keep := m.selectedVisibleID()
		m.filterQuery += msg.Text
		m.reconcileVisibleCursor(keep)
		return m, true
	}
	return m, false
}

// visibleCards returns the cards matching the active filter in snapshot
// order. With no active filter (or an empty query) it returns the full
// source slice; otherwise it returns a derived projection. The source
// slice (m.cards) is never mutated by filtering.
func (m *Model) visibleCards() []SessionCard {
	if !m.filterActive || m.filterQuery == "" {
		return m.cards
	}
	lower := strings.ToLower(m.filterQuery)
	out := make([]SessionCard, 0, len(m.cards))
	for _, c := range m.cards {
		if cardMatchesFilter(c, lower) {
			out = append(out, c)
		}
	}
	return out
}

// cardMatchesFilter reports whether a card matches the lowercased query
// over the sanitized display surfaces only: display identity, title,
// repository name, and work type. All four are cleaned before comparison
// so ANSI escapes and control characters never reach search.
func cardMatchesFilter(c SessionCard, lowerQuery string) bool {
	if lowerQuery == "" {
		return true
	}
	if strings.Contains(strings.ToLower(c.displayID()), lowerQuery) {
		return true
	}
	if strings.Contains(strings.ToLower(c.title()), lowerQuery) {
		return true
	}
	if strings.Contains(strings.ToLower(c.repoName()), lowerQuery) {
		return true
	}
	if strings.Contains(strings.ToLower(cleanText(c.WorkType)), lowerQuery) {
		return true
	}
	return false
}

// selectedVisibleID returns the SessionID under the cursor in the current
// visible slice, or "" when nothing is selected.
func (m *Model) selectedVisibleID() string {
	vis := m.visibleCards()
	if m.cursor >= 0 && m.cursor < len(vis) {
		return vis[m.cursor].SessionID
	}
	return ""
}

// reconcileVisibleCursor repoints the cursor at the visible slice,
// preserving keepID when it remains visible. Exclusion selects the first
// match; no matches (or no cards) selects nothing without panicking.
func (m *Model) reconcileVisibleCursor(keepID string) {
	vis := m.visibleCards()
	if len(vis) == 0 {
		m.cursor = 0
		return
	}
	if keepID != "" {
		for i := range vis {
			if vis[i].SessionID == keepID {
				m.cursor = i
				return
			}
		}
	}
	m.cursor = 0
}

// deleteLastRune removes the final Unicode rune (rune-safe backspace).
func deleteLastRune(s string) string {
	r := []rune(s)
	if len(r) == 0 {
		return s
	}
	return string(r[:len(r)-1])
}

// selectedCardID reports the cursor's session id in the visible
// (filtered) cards, or "" when no card is selected. Filtering is a
// projection: stop/drain target selection must follow the visible card
// under the cursor, not m.cards[m.cursor].
func (m *Model) selectedCardID() string {
	return m.selectedVisibleID()
}

// requestStopConfirm enters the stop gate for the selected session. It
// sends no request; confirmation arrives on a later key.
func (m *Model) requestStopConfirm() tea.Cmd {
	if m.actions == nil || m.actionBusy || len(m.visibleCards()) == 0 {
		return nil
	}
	if id := m.selectedCardID(); id != "" {
		m.confirmKind = "stop"
		m.confirmStopID = id
	}
	return nil
}

// requestDrainConfirm enters the drain gate. It sends no request;
// confirmation arrives on a later key. The prompt names the effect:
// sessions still running past the explicit deadline are stopped.
func (m *Model) requestDrainConfirm() tea.Cmd {
	if m.actions == nil || m.actionBusy {
		return nil
	}
	m.confirmKind = "drain"
	m.confirmStopID = ""
	return nil
}

// startPause dispatches one authenticated pause request off the render
// loop. Visible admission state changes only through the next snapshot.
func (m *Model) startPause() tea.Cmd {
	if m.actions == nil || m.actionBusy {
		return nil
	}
	m.actionBusy = true
	ctx, actions := m.actionCtx, m.actions
	return func() tea.Msg {
		resp, err := actions.PauseContext(ctx)
		return hostActionResultMsg{kind: "pause", ok: err == nil && resp != nil && resp.OK, message: actionResponseMessage(resp), err: err}
	}
}

// startResume dispatches one authenticated resume request off the render
// loop. Visible admission state changes only through the next snapshot.
func (m *Model) startResume() tea.Cmd {
	if m.actions == nil || m.actionBusy {
		return nil
	}
	m.actionBusy = true
	ctx, actions := m.actionCtx, m.actions
	return func() tea.Msg {
		resp, err := actions.ResumeContext(ctx)
		return hostActionResultMsg{kind: "resume", ok: err == nil && resp != nil && resp.OK, message: actionResponseMessage(resp), err: err}
	}
}

// startDrain dispatches one deadline-bound drain request off the render
// loop. The explicit timeout travels in both the request body and the
// command context; the daemon owns drain truth and the next snapshot
// carries the visible state.
func (m *Model) startDrain() tea.Cmd {
	if m.actions == nil || m.actionBusy {
		return nil
	}
	secs := m.drainTimeoutSecs
	if secs <= 0 {
		secs = DefaultDrainTimeoutSeconds
	}
	m.actionBusy = true
	actions := m.actions
	ctx, cancel := context.WithTimeout(m.actionCtx, time.Duration(secs)*time.Second)
	return func() tea.Msg {
		defer cancel()
		resp, err := actions.DrainContext(ctx, secs)
		return hostActionResultMsg{kind: "drain", timeoutSecs: secs, ok: err == nil && resp != nil && resp.OK, message: actionResponseMessage(resp), err: err}
	}
}

// startStopSession dispatches the single authenticated stop request for
// the confirmed session id off the render loop. The acknowledged card
// stays visible until a later snapshot omits it (terminal reap).
func (m *Model) startStopSession(id string) tea.Cmd {
	if m.actions == nil || m.actionBusy || id == "" {
		return nil
	}
	m.actionBusy = true
	ctx, actions := m.actionCtx, m.actions
	return func() tea.Msg {
		resp, err := actions.StopSessionContext(ctx, id)
		return hostActionResultMsg{kind: "stop", sessionID: id, ok: err == nil && resp != nil && resp.OK, message: actionResponseMessage(resp), err: err}
	}
}

// hostActionResultMsg carries one lifecycle command's outcome back to
// the render loop. Handling reports it, releases the in-flight guard,
// and returns a fresh index poll — it never edits cards or counters.
type hostActionResultMsg struct {
	kind        string
	sessionID   string
	timeoutSecs int
	ok          bool
	message     string
	err         error
}

func actionResponseMessage(resp *afclient.DaemonActionResponse) string {
	if resp == nil {
		return ""
	}
	return resp.Message
}

// formatHostActionResult renders one lifecycle outcome as a single
// bounded line. Failures and refusals never claim success and never
// carry credential or header material.
func formatHostActionResult(msg hostActionResultMsg) string {
	target := msg.kind + ": "
	switch msg.kind {
	case "stop":
		target = "stop " + shortID(sanitizeHostActionText(msg.sessionID)) + ": "
	case "drain":
		secs := msg.timeoutSecs
		if secs <= 0 {
			secs = DefaultDrainTimeoutSeconds
		}
		target = fmt.Sprintf("drain (%ds): ", secs)
	}
	if msg.err != nil {
		return target + hostActionErrorDetail(msg.err)
	}
	if !msg.ok {
		if detail := sanitizeHostActionText(msg.message); detail != "" {
			return target + "refused: " + detail
		}
		return target + "refused"
	}
	if detail := sanitizeHostActionText(msg.message); detail != "" {
		return target + detail
	}
	return target + "accepted"
}

// hostActionErrorDetail classifies a lifecycle failure for display. The
// daemon's reason travels only through the sanitizer: bounded, with
// bearer material redacted, so headers and credentials never render.
func hostActionErrorDetail(err error) string {
	detail := sanitizeHostActionText(err.Error())
	switch {
	case errors.Is(err, afclient.ErrNotAuthenticated) || errors.Is(err, afclient.ErrUnauthorized):
		return "unauthorized: " + detail
	case errors.Is(err, afclient.ErrUnavailable):
		return "unavailable: " + detail
	case errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled):
		return "timed out: " + detail
	default:
		return "failed: " + detail
	}
}

// sanitizeHostActionText strips terminal escapes and controls, redacts
// bearer credential material, and bounds the result to one notice line.
func sanitizeHostActionText(s string) string {
	return truncateRunes(redactBearerMaterial(cleanText(s)), maxHostActionNoticeRunes)
}

// redactBearerMaterial replaces every "Bearer <token>" span with a
// fixed placeholder so a credential echoed in an error or detail can
// never reach the display.
func redactBearerMaterial(s string) string {
	var b strings.Builder
	lower := strings.ToLower(s)
	for {
		idx := strings.Index(lower, "bearer ")
		if idx < 0 {
			b.WriteString(s)
			break
		}
		rest := s[idx+len("bearer "):]
		if rest == "" {
			// Trailing "Bearer " with no token: nothing to redact.
			b.WriteString(s)
			break
		}
		b.WriteString(s[:idx])
		b.WriteString("Bearer [redacted]")
		if i := strings.IndexAny(rest, " \t\n\r\"'"); i >= 0 {
			s, lower = rest[i:], lower[idx+len("bearer ")+i:]
		} else {
			break
		}
	}
	return b.String()
}

// setSplit clamps ratio into bounds. Only an explicit keypress changes the
// ratio; resizes keep it.
func (m *Model) setSplit(ratio float64) {
	m.split = clampSplit(ratio)
}

// clampSplit bounds a split ratio so neither pane can vanish.
func clampSplit(ratio float64) float64 {
	if ratio < minSplitRatio {
		return minSplitRatio
	}
	if ratio > maxSplitRatio {
		return maxSplitRatio
	}
	return ratio
}

// splitPaneHeights divides contentH rows between the grid and the stream
// (which includes its title row) per ratio. Minimums hold whenever they
// fit; on a tiny terminal the grid keeps one row and the stream takes the
// rest, so selection and the latest output stay reachable.
func splitPaneHeights(contentH int, ratio float64) (gridH, streamH int) {
	if contentH <= 0 {
		return 0, 0
	}
	r := clampSplit(ratio)
	gridH = int(math.Round(float64(contentH) * r))
	if minGridHeight+minStreamHeight <= contentH {
		if gridH < minGridHeight {
			gridH = minGridHeight
		}
		if contentH-gridH < minStreamHeight {
			gridH = contentH - minStreamHeight
		}
	} else if gridH < minGridHeight {
		gridH = minGridHeight
		if gridH > contentH {
			gridH = contentH
		}
	}
	return gridH, contentH - gridH
}

// applySnapshot folds a new index poll into the model: refreshes the cards +
// counters, then reconciles the tailer set (start tailers for new sessions,
// drop tailers for sessions that vanished). Folded live metrics
// (tool counts, cost/turns, activity, freshness) survive the refresh: the
// index poll carries no metrics, so a fresh card would otherwise zero them
// on every tick. Metrics carry over only for the same fenced run;
// reordered cards keep their own metrics and removed ids drop theirs.
func (m *Model) applySnapshot(snap Snapshot) {
	keepID := m.selectedVisibleID()
	m.snapErr = snap.Err
	m.counters = snap.Counters
	if snap.Err == nil {
		enrichHandles(m.opts.Handles, snap.Cards)
		kept := make(map[string]SessionCard, len(m.cards))
		for _, c := range m.cards {
			kept[c.SessionID] = c
		}
		for i := range snap.Cards {
			if snap.Cards[i].isHeld() {
				delete(m.tailers, snap.Cards[i].SessionID)
				continue // unresolved ownership cannot inherit earlier live observations
			}
			if prev, ok := kept[snap.Cards[i].SessionID]; ok {
				if snap.Cards[i].sameLegacyObservedRun(prev) {
					snap.Cards[i].retainLegacyFolded(prev)
					if snap.Cards[i].sameMetricObservedRun(prev) {
						snap.Cards[i].retainMetricFolded(prev)
					}
				} else {
					delete(m.tailers, snap.Cards[i].SessionID)
					m.attachTailer(&snap.Cards[i])
				}
			}
		}
		m.cards = snap.Cards
	}
	m.reconcileVisibleCursor(keepID)
	m.reconcileTailers()
}

// enrichHandles fills each card's display handle from the optional hook
// once per index snapshot. It never runs on the tail path, so the call
// count stays flat across tail ticks. Empty or whitespace-only results
// clear the handle and fall back to the issue identifier.
func enrichHandles(h HandleEnricher, cards []SessionCard) {
	if h == nil {
		for i := range cards {
			cards[i].Handle = ""
		}
		return
	}
	for i := range cards {
		if handle := h.Handle(cards[i].SessionID); strings.TrimSpace(handle) != "" {
			cards[i].Handle = handle
		} else {
			cards[i].Handle = ""
		}
	}
}

// reconcileTailers starts a tailer for every scoped session that has a known
// worktree path and no tailer yet, and drops tailers whose session is no
// longer present. A provider turn result need not end a continuing session.
// Labels are remembered so the merged stream stays attributed after exit.
func (m *Model) reconcileTailers() {
	present := map[string]struct{}{}
	for i := range m.cards {
		c := &m.cards[i]
		present[c.SessionID] = struct{}{}
		// Remember the label for the stream prefix.
		m.labels[c.SessionID] = c.displayID()

		if c.isHeld() || c.EventsPath() == "" {
			delete(m.tailers, c.SessionID)
			continue // held or pathless sessions have no session event stream
		}
		if _, ok := m.tailers[c.SessionID]; !ok {
			m.attachTailer(c)
		}
	}
	for id, t := range m.tailers {
		_, stillPresent := present[id]
		if !stillPresent || t.Done() {
			delete(m.tailers, id)
		}
	}
}

// attachTailer starts the card's metric tailer. A valid per-run offset
// replays only this run's bytes; absent or invalid offsets attach at EOF,
// fail closed. When the run already wrote events before the watcher
// attached, the journal's modification time is the run's true last output
// time, so a quiet session reads "4m ago" rather than "never" until its
// next event — replayed events themselves still never advance freshness.
func (m *Model) attachTailer(c *SessionCard) {
	path := c.EventsPath()
	if path == "" {
		return
	}
	t := NewMetricTailer(c.SessionID, path, c.EventLogStartOffset, m.now)
	m.tailers[c.SessionID] = t
	if c.LastOutputAt.IsZero() {
		c.LastOutputAt = t.AttachModTime()
	}
}

// applyTailBatch appends the tick's merged events to the stream pane and folds
// per-session live metrics (tool count, last tool, cost) onto the matching
// card.
func (m *Model) applyTailBatch(events []TailEvent) {
	if len(events) == 0 {
		return
	}
	var lines []string
	for _, ev := range events {
		if m.isHeldSession(ev.SessionID) {
			continue
		}
		if ev.source != nil && m.tailers[ev.SessionID] != ev.source {
			continue // old asynchronous batch from a removed/replaced reader
		}
		label := m.labels[ev.SessionID]
		idx := m.prefixes.get(ev.SessionID)
		if !ev.Replay || m.opts.Replay {
			if line := formatStreamLine(m.theme, ev, label, idx, m.opts.Plain); line != "" {
				lines = append(lines, line)
			}
		}
		m.foldMetrics(ev)
	}
	if len(lines) > 0 {
		m.stream.Append(lines...)
	}
	// Drop tailers that hit their terminal event this batch so a finished
	// session stops being polled immediately (rather than waiting for the
	// next index reconcile). The card persists until the daemon reaps the
	// session from the index.
	for id, t := range m.tailers {
		if t.Done() {
			delete(m.tailers, id)
		}
	}
}

// Legacy live observations survive index refreshes in mixed-version runs.
// A known change of run start or journal boundary is proof of replacement;
// an absent field alone is not. Session removal still drops all folds.
func (c SessionCard) sameLegacyObservedRun(prev SessionCard) bool {
	if c.SessionID != prev.SessionID || c.WorktreePath != prev.WorktreePath {
		return false
	}
	if c.StartedAtUnixMs > 0 && prev.StartedAtUnixMs > 0 && c.StartedAtUnixMs != prev.StartedAtUnixMs {
		return false
	}
	if c.EventLogStartOffset != nil && prev.EventLogStartOffset != nil && *c.EventLogStartOffset != *prev.EventLogStartOffset {
		return false
	}
	return true
}

// New cumulative cost/turn observations need a positive exact run start and
// journal boundary. Without either, they are not carried into a fresh card.
func (c SessionCard) sameMetricObservedRun(prev SessionCard) bool {
	return c.sameLegacyObservedRun(prev) && c.StartedAtUnixMs > 0 &&
		prev.StartedAtUnixMs > 0 && c.StartedAtUnixMs == prev.StartedAtUnixMs &&
		c.EventLogStartOffset != nil && prev.EventLogStartOffset != nil &&
		*c.EventLogStartOffset == *prev.EventLogStartOffset
}

func (c *SessionCard) retainLegacyFolded(prev SessionCard) {
	c.ToolCalls = prev.ToolCalls
	c.LastTool = prev.LastTool
	c.LastActivity = prev.LastActivity
	c.Errored = prev.Errored
	c.Observed = prev.Observed
	c.LastWorkAt = prev.LastWorkAt
	c.LastOutputAt = prev.LastOutputAt
	c.streamTicks = prev.streamTicks
	c.streamingAt = prev.streamingAt
	c.ActualModel = prev.ActualModel
	c.ActualModelProvider = prev.ActualModelProvider
	c.ActualModelVersion = prev.ActualModelVersion
}

func (c *SessionCard) retainMetricFolded(prev SessionCard) {
	c.CostUsd = prev.CostUsd
	c.NumTurns = prev.NumTurns
	c.MetricsReported = prev.MetricsReported
	c.CostReported = prev.CostReported
	c.TurnsReported = prev.TurnsReported
	c.seenCallSpanIDs = prev.seenCallSpanIDs
	c.seenTerminalOffsets = prev.seenTerminalOffsets
}

func (m *Model) isHeldSession(sessionID string) bool {
	for i := range m.cards {
		if m.cards[i].SessionID == sessionID {
			return m.cards[i].isHeld()
		}
	}
	return false
}

// foldMetrics updates the live per-card metrics from one event. Cards are
// matched by session id; an event for a card not currently in view is
// ignored (its card will pick up state.json header data on the next index
// poll). Cumulative metrics (tool counts, cost/turns, activity text) fold
// from every event including replayed history; freshness timestamps advance
// only on live events, so replaying history never makes an idle session
// look active now. Heartbeats are liveness only: they advance the
// heartbeat freshness the runner persists, never tool counts or output.
func (m *Model) foldMetrics(ev TailEvent) {
	idx := -1
	for i := range m.cards {
		if m.cards[i].SessionID == ev.SessionID {
			idx = i
			break
		}
	}
	if idx < 0 || ev.Event == nil {
		return
	}
	c := &m.cards[idx]
	if c.isHeld() {
		return
	}
	// Observed means the journal carries the agent event stream, which is
	// what makes a zero tool count a measurement. System notices alone (an
	// interactive terminal session's journal holds nothing else) do not.
	if _, notice := ev.Event.(agent.SystemEvent); !notice {
		c.Observed = true
	}
	live := !ev.Replay
	switch e := ev.Event.(type) {
	case agent.SystemEvent:
		if live && e.Subtype == agent.SystemSubtypeStreamProgress {
			c.streamTicks++
			c.streamingAt = ev.EventAt()
		}
	case agent.AssistantTextEvent, agent.ToolUseEvent, agent.ToolResultEvent,
		agent.ResultEvent, agent.ErrorEvent:
		c.streamTicks, c.streamingAt = 0, time.Time{} // content ends the stream
	}
	switch e := ev.Event.(type) {
	case agent.ToolUseEvent:
		c.ToolCalls++
		c.LastTool = truncateRunes(cleanText(toolUseSummary(e)), maxActivityRunes)
		c.LastActivity = c.LastTool
		if live {
			c.LastWorkAt = ev.EventAt()
		}
	case agent.LlmCallEvent:
		// Only explicit native response metadata identifies the serving model.
		// Replace the whole observation, including absent components, so a
		// fallback model cannot inherit another response's provider/version.
		if !e.Synthetic && (e.ResponseModel != "" || e.ResponseModelProvider != "" || e.ModelSnapshotID != "") {
			c.ActualModel = e.ResponseModel
			c.ActualModelProvider = e.ResponseModelProvider
			c.ActualModelVersion = e.ModelSnapshotID
		}
		key := observedUsageKey(e, ev.Offset)
		if !e.Synthetic && e.UsageSource == agent.LlmUsageProvider && key != "" {
			if c.seenCallSpanIDs == nil {
				c.seenCallSpanIDs = make(map[string]struct{})
			}
			if _, seen := c.seenCallSpanIDs[key]; !seen {
				c.seenCallSpanIDs[key] = struct{}{}
				if e.ObservedCostUsd != nil && finiteNonnegative(*e.ObservedCostUsd) {
					next := c.CostUsd + *e.ObservedCostUsd
					if finiteNonnegative(next) {
						c.CostUsd = next
						c.CostReported = true
					}
				}
				if e.TurnCompleted {
					c.NumTurns++
					c.TurnsReported = true
				}
				c.MetricsReported = c.CostReported || c.TurnsReported
			}
		}
		// A model call with usage is meaningful work even without a tool
		// invocation; a call without usage is an observation only.
		if e.InputTokens > 0 || e.OutputTokens > 0 {
			if live {
				c.LastWorkAt = ev.EventAt()
			}
		} else if live {
			c.LastOutputAt = ev.EventAt()
		}
	case agent.SystemEvent:
		if e.Subtype == agent.SystemSubtypeModelIdentity && e.ObservedModel != nil {
			c.ActualModel = e.ObservedModel.Model
			c.ActualModelProvider = e.ObservedModel.Provider
			c.ActualModelVersion = e.ObservedModel.Version
		}
		// An interactive terminal session's journal holds only system
		// notices; without folding them into activity text the card's
		// activity row falls back to the runner's CurrentStep ("spawning"
		// until the runner advances it) for the whole session. Fold the
		// lifecycle notices into human-readable activity, mirroring the
		// running → stopped step transitions, so the card reflects the
		// current state even when no agent event stream exists.
		if text := interactiveLifecycleActivity(e); text != "" {
			c.LastActivity = truncateRunes(text, maxActivityRunes)
		}
		if live {
			c.LastOutputAt = ev.EventAt()
		}
	case agent.ToolResultEvent, agent.ToolProgressEvent:
		if live {
			c.LastOutputAt = ev.EventAt()
		}
	case agent.AssistantTextEvent:
		if txt := cleanText(e.Text); txt != "" {
			c.LastActivity = truncateRunes(txt, maxActivityRunes)
			if live {
				c.LastOutputAt = ev.EventAt()
			}
		}
	case agent.ResultEvent:
		// A native cumulative observation replaces the provisional sum. A
		// missing field leaves the independently known field untouched.
		terminalSeen := false
		if ev.Offset != nil {
			if c.seenTerminalOffsets == nil {
				c.seenTerminalOffsets = make(map[int64]struct{})
			}
			_, terminalSeen = c.seenTerminalOffsets[*ev.Offset]
			c.seenTerminalOffsets[*ev.Offset] = struct{}{}
		}
		if !terminalSeen {
			if e.ObservedCostUsd != nil && finiteNonnegative(*e.ObservedCostUsd) {
				c.CostUsd = *e.ObservedCostUsd
				c.CostReported = true
			}
			if e.ObservedTurns != nil && *e.ObservedTurns >= 0 {
				c.NumTurns = *e.ObservedTurns
				c.TurnsReported = true
			}
			c.MetricsReported = c.CostReported || c.TurnsReported
		}
		if !e.Success {
			c.Errored = true
		}
		c.LastActivity = streamTickerText(ev)
		if live {
			c.LastOutputAt = ev.EventAt()
		}
	case agent.ErrorEvent:
		c.Errored = true
		c.LastActivity = truncateRunes(cleanText(e.Message), maxActivityRunes)
		if live {
			c.LastOutputAt = ev.EventAt()
		}
	}
}

// interactiveLifecycleActivity renders an interactive session's lifecycle
// SystemEvent notice as card activity text. Empty for every other notice:
// only the lifecycle transitions name the session state, so only they can
// stand in for activity when the journal carries no agent event stream.
func interactiveLifecycleActivity(e agent.SystemEvent) string {
	switch e.Subtype {
	case "interactive-session-started":
		return "interactive session running"
	case "interactive-initial-prompt-delivered":
		return "initial prompt delivered"
	case "interactive-session-ended":
		if msg := cleanText(e.Message); msg != "" {
			return truncateRunes(msg, maxActivityRunes)
		}
		return "interactive session ended"
	default:
		return ""
	}
}

func finiteNonnegative(value float64) bool {
	return !math.IsNaN(value) && !math.IsInf(value, 0) && value >= 0
}

// View renders the full dashboard: host header, card grid, then the
// merged session stream (or the selected session's detail).
func (m *Model) View() tea.View {
	v := tea.NewView(m.render())
	v.AltScreen = !m.opts.Plain
	return v
}

// render composes the frame row by row. Each pane occupies exactly the
// rows the split allocates it — a sparse grid is padded, never collapsed —
// so the stream boundary and the help row stay at fixed positions for a
// given terminal size and split. Every row fits the terminal width and
// the frame never exceeds the terminal height.
func (m *Model) render() string {
	if m.width == 0 {
		return ""
	}
	unbounded := m.opts.Plain && m.opts.UnboundedCards
	header := m.renderHeader()
	help := m.renderHelp()
	if m.help {
		if m.snapErr != nil {
			warn := truncateWidth(fmt.Sprintf("daemon unreachable: %v", cleanText(m.snapErr.Error())), m.width)
			if !m.opts.Plain {
				warn = theme.ErrorText().Render(warn)
			}
			return m.clipHeight([]string{header, m.renderHelpOverlay(), warn, help}, unbounded)
		}
		return m.clipHeight([]string{header, m.renderHelpOverlay(), help}, unbounded)
	}
	if m.snapErr != nil {
		warn := truncateWidth(fmt.Sprintf("daemon unreachable: %v", cleanText(m.snapErr.Error())), m.width)
		if !m.opts.Plain {
			warn = theme.ErrorText().Render(warn)
		}
		return m.clipHeight([]string{header, "", warn, "", help}, unbounded)
	}

	gridH, streamH := splitPaneHeights(m.height-2, m.split)
	rows := []string{header}
	vis := m.visibleCards()
	if unbounded {
		rows = append(rows, renderGrid(m.theme, vis, m.cursor, m.frame, m.width, 0, true, m.now()))
	} else {
		grid := renderGrid(m.theme, vis, m.cursor, m.frame, m.width, gridH, m.opts.Plain, m.now())
		rows = append(rows, fitLines(grid, gridH)...)
	}
	if streamH > 0 || unbounded {
		bodyH := max(streamH-1, 0)
		if m.detail && m.cursor >= 0 && m.cursor < len(vis) {
			card := vis[m.cursor]
			rows = append(rows, detailTitle(card, m.opts.Plain, m.theme, m.width))
			rows = append(rows, fitLines(strings.Join(renderDetail(m.theme, card, m.now(), m.width, bodyH, m.opts.Plain), "\n"), bodyH)...)
		} else {
			rows = append(rows, m.renderStreamTitle())
			body := m.stream.View(m.width, bodyH)
			if unbounded {
				body = trimTrailingBlank(body)
			}
			rows = append(rows, body...)
		}
	}
	rows = append(rows, help)
	return m.clipHeight(rows, unbounded)
}

// clipHeight joins rows, dropping any beyond the terminal height so the
// header stays on screen even on a terminal shorter than the chrome.
func (m *Model) clipHeight(rows []string, unbounded bool) string {
	out := strings.Join(rows, "\n")
	if unbounded || m.height <= 0 {
		return out
	}
	lines := strings.Split(out, "\n")
	if len(lines) > m.height {
		lines = lines[:m.height]
	}
	return strings.Join(lines, "\n")
}

func trimTrailingBlank(lines []string) []string {
	for len(lines) > 0 && strings.TrimSpace(lines[len(lines)-1]) == "" {
		lines = lines[:len(lines)-1]
	}
	return lines
}

// renderStreamTitle is the stream pane's title row with its follow state.
func (m *Model) renderStreamTitle() string {
	state := "follow"
	if !m.stream.Following() {
		state = "paused"
	}
	if m.opts.Plain {
		return truncateWidth("session stream  ["+state+"]", m.width)
	}
	return truncateWidth(theme.SectionTitle().Render("session stream")+
		lipgloss.NewStyle().Foreground(m.theme.TextTertiary).Render("  ["+state+"]"), m.width)
}

// renderHeader renders the one-line host header: the host identity first,
// then the scope (only when it narrows the default "all projects" view),
// then the counters — sessions in scope, queue depth, the compact
// capacity strip (host active/max), uptime and version. All values are local (daemon
// status/stats). The single line is budgeted against the terminal width:
// lower-priority counters drop first, the host is kept whole whenever it
// fits, and the only horizontal chrome is the style's one padding cell on
// each side.
func (m *Model) renderHeader() string {
	scope := m.opts.ProjectLabel
	if scope == "" {
		scope = "all projects"
	}
	host := m.opts.HostLabel
	c := m.counters

	contentWidth := m.width
	style := theme.Header()
	if !m.opts.Plain && m.width > 0 {
		chrome := 2 // Header style padding: one cell on each side.
		if m.width < 4 {
			chrome = 0
			style = style.Padding(0, 0)
		}
		contentWidth -= chrome
	}
	left, gap, right := headerBudgetVariants(host, scope, headerCounterVariants(c), contentWidth)
	content := left + strings.Repeat(" ", gap) + right
	if m.opts.Plain {
		return content
	}
	leftStyled := lipgloss.NewStyle().Bold(true).Foreground(m.theme.Accent).Render(left)
	rightStyled := lipgloss.NewStyle().Foreground(m.theme.TextSecondary).Render(right)
	content = leftStyled + strings.Repeat(" ", gap) + rightStyled
	return style.Width(m.width).Render(content)
}

// headerCounterVariants lists the counter strings from richest to barest.
// Counters drop lowest priority first — version, uptime, the capacity
// strip, then queue — before the in-scope session count itself, so a
// narrow header keeps the work waiting before the host's capacity.
func headerCounterVariants(c Counters) []string {
	status := fmt.Sprintf("%d running", c.Running)
	if c.Sessions > c.Running {
		status = fmt.Sprintf("%d sessions", c.Sessions)
	}
	parts := []string{status, fmt.Sprintf("queue %d", c.QueueDepth)}
	if c.MaxSessions > 0 {
		// Known capacity renders as a compact strip through the shared
		// capacity formatter. Zero MaxSessions means an older daemon with
		// unknown capacity, so the strip omits it and the queue keeps its
		// higher drop priority.
		capMax := c.MaxSessions
		parts = append(parts, "cap "+format.CapacityRatio(c.HostActive, &capMax))
	}
	parts = append(parts, "uptime "+format.Duration(int(c.UptimeSeconds)))
	if c.Version != "" {
		parts = append(parts, "v"+c.Version)
	}
	variants := make([]string, 0, len(parts)+1)
	for n := len(parts); n > 0; n-- {
		variants = append(variants, strings.Join(parts[:n], "   "))
	}
	return append(variants, "")
}

// headerBudgetVariants composes a one-line header within the content width left
// after any style padding. It preserves the host and scope before shortening
// the host, then selects the richest counters that fit. Width <= 0 means
// unbounded and retains the full header with its normal two-cell gap.
func headerBudgetVariants(host, scope string, rightVariants []string, width int) (string, int, string) {
	if len(rightVariants) == 0 {
		rightVariants = []string{""}
	}
	if host == "" {
		host = "host"
	}
	if width <= 0 {
		left := host
		if scope != "" && scope != "all projects" {
			left += " · " + scope
		}
		return left, 2, rightVariants[0]
	}
	leftWidthFor := func(right string) int {
		gapWidth := 0
		if right != "" {
			gapWidth = 1
		}
		return width - lipgloss.Width(right) - gapWidth
	}
	scopedHost := host
	if scope != "" && scope != "all projects" {
		scopedHost += " · " + scope
	}
	for _, right := range rightVariants {
		leftWidth := leftWidthFor(right)
		if lipgloss.Width(scopedHost) <= leftWidth {
			gap := 0
			if right != "" {
				gap = width - lipgloss.Width(scopedHost) - lipgloss.Width(right)
			}
			return scopedHost, gap, right
		}
	}
	for _, right := range rightVariants {
		leftWidth := leftWidthFor(right)
		if leftWidth < lipgloss.Width(host) {
			continue
		}
		gap := 0
		if right != "" {
			gap = width - lipgloss.Width(host) - lipgloss.Width(right)
		}
		return host, gap, right
	}
	// No counter variant fits beside the complete host. Keep only the primary
	// running count if at least one host cell remains; otherwise let the host
	// use the full line by itself.
	primary := ""
	if len(rightVariants) > 1 {
		primary = rightVariants[len(rightVariants)-2]
	}
	primaryWidth := lipgloss.Width(primary)
	if primary != "" && width-primaryWidth-1 >= 1 {
		leftWidth := width - primaryWidth - 1
		return truncateHeaderHost(host, leftWidth), 1, primary
	}
	return truncateHeaderHost(host, width), 0, ""
}

func truncateHeaderHost(host string, width int) string {
	runes := []rune(host)
	if width == 1 && len(runes) > 0 {
		if lipgloss.Width(string(runes[0])) == 1 {
			return string(runes[0])
		}
		return "…"
	}
	if width == 2 && len(runes) > 0 {
		if lipgloss.Width(string(runes[0])) == 2 {
			return string(runes[0])
		}
		return truncateWidth(host, width)
	}
	return truncateWidth(host, width)
}

// helpText is the compact footer row. "?" opens the full contextual
// help overlay; the footer stays one discoverable line.
const helpText = "? help   ↑↓/jk select   enter detail   x stop   p pause   r resume   d drain   [ ] split   0 reset   f follow/pause   g tail   q quit"

func (m *Model) renderHelp() string {
	help := helpText
	switch {
	case m.confirmKind == "stop":
		help = "stop " + shortID(sanitizeHostActionText(m.confirmStopID)) + "? y confirm · n cancel"
	case m.confirmKind == "drain":
		secs := m.drainTimeoutSecs
		if secs <= 0 {
			secs = DefaultDrainTimeoutSeconds
		}
		help = fmt.Sprintf("drain host (%ds): sessions still running are stopped? y confirm · n cancel", secs)
	case m.notice != "":
		help = m.notice + "   " + helpText
	}
	// The "?" opener stays visible at narrow widths even beside a long
	// filter or notice: it is the footer's discoverability anchor. When
	// the filter suffix alone overflows, the suffix keeps its tail (the
	// live query text) while the anchor still leads.
	anchor := "? help"
	if m.filterActive {
		suffix := "   filter: /" + m.filterQuery
		if m.width > 0 {
			avail := m.width - lipgloss.Width(suffix)
			if avail < 0 {
				// Suffix overflows on its own: anchor + the suffix tail.
				room := m.width - lipgloss.Width(anchor) - 3
				if room < 0 {
					room = 0
				}
				help = anchor + "   " + truncateWidth(filterSuffixTail(suffix, room), room)
				help = truncateWidth(help, m.width)
			} else {
				help = truncateWidth(help, avail) + suffix
				help = reserveHelpAnchor(truncateWidth(help, m.width), anchor, m.width)
			}
		} else {
			help += suffix
		}
	} else {
		help = truncateWidth(help, m.width)
		help = reserveHelpAnchor(help, anchor, m.width)
	}
	if m.opts.Plain {
		return help
	}
	return lipgloss.NewStyle().Foreground(m.theme.TextTertiary).Render(help)
}

// filterSuffixTail keeps the live end of an overflowing filter suffix:
// the "filter: /" marker when it fits, else the query tail alone,
// truncated to room cells.
func filterSuffixTail(suffix string, room int) string {
	if room <= 0 {
		return ""
	}
	if lipgloss.Width(suffix) <= room {
		return suffix
	}
	const marker = "filter: /"
	if idx := strings.Index(suffix, marker); idx >= 0 {
		tail := suffix[idx:]
		for lipgloss.Width(tail) > room && len(tail) > 0 {
			tail = string([]rune(tail)[1:])
		}
		return tail
	}
	return truncateWidth(suffix, room)
}

// reserveHelpAnchor keeps the "? help" opener visible at narrow widths:
// when it was truncated away, the row is rebuilt from the anchor plus as
// much of the remainder as fits. Wide rows pass through unchanged.
func reserveHelpAnchor(help, anchor string, width int) string {
	if width <= 0 || lipgloss.Width(anchor) >= width {
		return truncateWidth(anchor, width)
	}
	if strings.Contains(help, anchor) {
		return help
	}
	// Rebuild: anchor + separator + the tail of the row that still fits.
	room := width - lipgloss.Width(anchor) - 3
	tail := help
	for lipgloss.Width(tail) > room && len(tail) > 0 {
		tail = string([]rune(tail)[1:])
	}
	tail = strings.TrimLeft(tail, " ")
	return anchor + "   " + truncateWidth(tail, room)
}

// A journal position identifies a local observation when optional span upload
// is disabled. Both namespaces are stable on replay and reset with the run.
func observedUsageKey(call agent.LlmCallEvent, offset *int64) string {
	if call.SpanID != "" {
		return "span:" + call.SpanID
	}
	if offset != nil && *offset >= 0 {
		return fmt.Sprintf("journal:%d", *offset)
	}
	return ""
}
