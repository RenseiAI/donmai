package hostwatch

import (
	"fmt"
	"math"
	"sort"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/RenseiAI/donmai/agent"
	"github.com/RenseiAI/tui-components/format"
	"github.com/RenseiAI/tui-components/theme"
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

// Options configures a hostwatch Model.
type Options struct {
	// Source is the local data layer (daemon index + state.json). Required.
	Source *Source
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

	// split is the fraction of content height given to the session-card
	// grid (the stream takes the rest). Operator-adjustable via [ ] / 0.
	split float64

	// detail, when true, replaces the stream pane with the selected
	// session's full detail (enter toggles, esc closes).
	detail bool
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
	idx := opts.IndexInterval
	if idx <= 0 {
		idx = defaultIndexInterval
	}
	tail := opts.TailInterval
	if tail <= 0 {
		tail = defaultTailInterval
	}
	return &Model{
		src:           opts.Source,
		theme:         t,
		opts:          opts,
		now:           now,
		indexInterval: idx,
		tailInterval:  tail,
		tailers:       map[string]*Tailer{},
		prefixes:      newPrefixIndex(),
		labels:        map[string]string{},
		stream:        newStreamPane(),
		split:         defaultSplitRatio,
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
		return m, nil

	case indexTickMsg:
		m.frame++
		return m, tea.Batch(m.pollIndex(), m.indexTick())

	case tailTickMsg:
		return m, tea.Batch(m.pollTails(), m.tailTick())

	case snapshotMsg:
		m.applySnapshot(msg.snap)
		return m, nil

	case tailBatchMsg:
		m.applyTailBatch(msg.events)
		return m, nil

	case tea.KeyPressMsg:
		return m.handleKey(msg)
	}
	return m, nil
}

func (m *Model) handleKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "ctrl+c", "q":
		return m, tea.Quit
	case "up", "k":
		if m.cursor > 0 {
			m.cursor--
		}
	case "down", "j":
		if m.cursor < len(m.cards)-1 {
			m.cursor++
		}
	case "enter":
		m.detail = !m.detail && len(m.cards) > 0
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
	}
	return m, nil
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
	m.snapErr = snap.Err
	m.counters = snap.Counters
	if snap.Err == nil {
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
	if m.cursor >= len(m.cards) {
		m.cursor = len(m.cards) - 1
	}
	if m.cursor < 0 {
		m.cursor = 0
	}
	m.reconcileTailers()
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
	if m.snapErr != nil {
		warn := truncateWidth(fmt.Sprintf("daemon unreachable: %v", cleanText(m.snapErr.Error())), m.width)
		if !m.opts.Plain {
			warn = theme.ErrorText().Render(warn)
		}
		return m.clipHeight([]string{header, "", warn, "", help}, unbounded)
	}

	gridH, streamH := splitPaneHeights(m.height-2, m.split)
	rows := []string{header}
	if unbounded {
		rows = append(rows, renderGrid(m.theme, m.cards, m.cursor, m.frame, m.width, 0, true, m.now()))
	} else {
		grid := renderGrid(m.theme, m.cards, m.cursor, m.frame, m.width, gridH, m.opts.Plain, m.now())
		rows = append(rows, fitLines(grid, gridH)...)
	}
	if streamH > 0 || unbounded {
		bodyH := max(streamH-1, 0)
		if m.detail && m.cursor < len(m.cards) {
			card := m.cards[m.cursor]
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
// then the counters — sessions in scope, queue depth, the host's
// occupied/total session slots, uptime and version. All values are local (daemon
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
// Counters drop lowest priority first — version, uptime, host slots, then
// queue — before the in-scope session count itself, so a narrow header
// keeps the work waiting before the host's capacity.
func headerCounterVariants(c Counters) []string {
	status := fmt.Sprintf("%d running", c.Running)
	if c.Sessions > c.Running {
		status = fmt.Sprintf("%d sessions", c.Sessions)
	}
	parts := []string{status, fmt.Sprintf("queue %d", c.QueueDepth)}
	if c.MaxSessions > 0 {
		parts = append(parts, fmt.Sprintf("slots %d/%d", c.HostActive, c.MaxSessions))
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

// helpText documents every key. It is truncated to the terminal width.
const helpText = "↑↓/jk select   enter detail   [ ] split   0 reset   f follow/pause   g tail   q quit"

func (m *Model) renderHelp() string {
	help := truncateWidth(helpText, m.width)
	if m.opts.Plain {
		return help
	}
	return lipgloss.NewStyle().Foreground(m.theme.TextTertiary).Render(help)
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
