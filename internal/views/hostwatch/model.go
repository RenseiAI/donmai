package hostwatch

import (
	"fmt"
	"sort"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/RenseiAI/donmai/agent"
	"github.com/RenseiAI/tui-components/format"
	"github.com/RenseiAI/tui-components/theme"
	"github.com/RenseiAI/tui-components/widget"
)

// Default cadences. The index poll is deliberately low-frequency (a
// localhost JSON GET against an in-memory daemon snapshot); the tail poll is
// the responsiveness knob for the live stream. Both are coalesced — one
// timer each, not one per session — so the watcher stays nearly free at tens
// of concurrent local projects.
const (
	defaultIndexInterval = 1500 * time.Millisecond
	defaultTailInterval  = 350 * time.Millisecond
	// maxLinesPerTick caps how many merged-stream lines we append per tail
	// tick so a chatty tool burst cannot stall the render loop (backpressure
	// onto the LogViewer's ring buffer).
	maxLinesPerTick = 200
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

// Model is the host-watch fleet dashboard Bubble Tea model. It owns the
// merged LogViewer, the per-session tailers, and the card grid. It is a pure
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

	stream *widget.LogViewer
	frame  int
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
	lv := widget.NewLogViewer(widget.WithFollow(true), widget.WithWrap(false))
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
		stream:        lv,
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

// pollTails reads every active tailer once. It runs in a tea.Cmd goroutine;
// the tailers are not touched elsewhere concurrently because all access is
// serialized through the Bubble Tea update loop (this Cmd is the only reader,
// and tailer registration happens on snapshotMsg, also on the loop). To keep
// that invariant the Cmd captures the current tailer set by value.
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
			evs, err := t.Poll()
			if err != nil {
				continue // I/O hiccup — skip this tailer this tick
			}
			batch = append(batch, evs...)
			if len(batch) >= maxLinesPerTick {
				break
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
		m.width = msg.Width
		m.height = msg.Height
		m.layout()
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
	case "f":
		m.stream.SetFollowing(!m.stream.Following())
	case "g":
		m.stream.SetFollowing(true)
	}
	return m, nil
}

// applySnapshot folds a new index poll into the model: refreshes the cards +
// counters, then reconciles the tailer set (start tailers for new sessions,
// drop tailers for sessions that vanished or finished).
func (m *Model) applySnapshot(snap Snapshot) {
	m.snapErr = snap.Err
	m.counters = snap.Counters
	if snap.Err == nil {
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
// longer present OR whose tailer is Done (terminal event consumed). Labels
// are remembered so the merged stream stays attributed after a session ends.
func (m *Model) reconcileTailers() {
	present := map[string]struct{}{}
	for i := range m.cards {
		c := m.cards[i]
		present[c.SessionID] = struct{}{}
		// Remember the label for the stream prefix.
		label := c.IssueIdentifier
		if label == "" {
			label = shortID(c.SessionID)
		}
		m.labels[c.SessionID] = label

		evPath := c.EventsPath()
		if evPath == "" {
			continue // worktree path unknown — cannot tail (daemon pre-enrichment)
		}
		if _, ok := m.tailers[c.SessionID]; !ok {
			m.tailers[c.SessionID] = NewTailer(c.SessionID, evPath, !m.opts.Replay, m.now)
		}
	}
	for id, t := range m.tailers {
		_, stillPresent := present[id]
		if !stillPresent || t.Done() {
			delete(m.tailers, id)
		}
	}
}

// applyTailBatch appends the tick's merged events to the LogViewer and folds
// per-session live metrics (tool count, last tool, cost) onto the matching
// card.
func (m *Model) applyTailBatch(events []TailEvent) {
	if len(events) == 0 {
		return
	}
	var lines []string
	for _, ev := range events {
		label := m.labels[ev.SessionID]
		idx := m.prefixes.get(ev.SessionID)
		if line := formatStreamLine(m.theme, ev, label, idx, m.opts.Plain); line != "" {
			lines = append(lines, line)
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

// foldMetrics updates the live per-card metrics from one event. Cards are
// matched by session id; an event for a card not currently in view is
// ignored (its card will pick up state.json header data on the next index
// poll).
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
	switch e := ev.Event.(type) {
	case agent.ToolUseEvent:
		c.ToolCalls++
		c.LastTool = toolUseSummary(e)
		c.LastActivity = c.LastTool
	case agent.AssistantTextEvent:
		if txt := collapseWS(e.Text); txt != "" {
			c.LastActivity = truncateRunes(txt, cardWidth)
		}
	case agent.ResultEvent:
		if e.Cost != nil {
			c.CostUsd = e.Cost.TotalCostUsd
			c.NumTurns = e.Cost.NumTurns
		}
		if !e.Success {
			c.Errored = true
		}
		c.LastActivity = streamTickerText(ev)
	case agent.ErrorEvent:
		c.Errored = true
		c.LastActivity = truncateRunes(e.Message, cardWidth)
	}
}

// layout recomputes child sizes after a resize. The stream gets the bottom
// third (min 6 rows); the grid + header take the rest.
func (m *Model) layout() {
	if m.width == 0 || m.height == 0 {
		return
	}
	streamH := m.height / 3
	if streamH < 6 {
		streamH = 6
	}
	if streamH > m.height-4 {
		streamH = m.height - 4
	}
	if streamH < 1 {
		streamH = 1
	}
	// -1 for the "session stream" title row above the viewer.
	m.stream.SetSize(m.width, streamH-1)
}

// View renders the full dashboard: counters header, card grid, then the
// merged session stream.
func (m *Model) View() tea.View {
	v := tea.NewView(m.render())
	v.AltScreen = !m.opts.Plain
	return v
}

func (m *Model) render() string {
	if m.width == 0 {
		return ""
	}
	header := m.renderHeader()
	if m.snapErr != nil {
		warn := fmt.Sprintf("daemon unreachable: %v", m.snapErr)
		if !m.opts.Plain {
			warn = theme.ErrorText().Render(warn)
		}
		return lipgloss.JoinVertical(lipgloss.Left, header, "", warn, "", m.renderHelp())
	}

	streamH := m.height / 3
	if streamH < 6 {
		streamH = 6
	}
	gridH := m.height - streamH - lineCount(header) - 2 // -2 for stream title + help
	if gridH < 1 {
		gridH = 1
	}
	grid := renderGrid(m.theme, m.cards, m.cursor, m.frame, m.width, m.opts.Plain, m.now())
	grid = clampHeight(grid, gridH)

	streamTitle := "session stream"
	if !m.opts.Plain {
		follow := "follow"
		if !m.stream.Following() {
			follow = "paused"
		}
		streamTitle = theme.SectionTitle().Render("session stream") +
			lipgloss.NewStyle().Foreground(m.theme.TextTertiary).Render("  ["+follow+"]")
	}
	streamBody := m.stream.View().Content

	return lipgloss.JoinVertical(lipgloss.Left,
		header,
		grid,
		streamTitle,
		streamBody,
		m.renderHelp(),
	)
}

// renderHeader renders the counters bar: scope label + running/queue/cost,
// uptime, version. All values are local (daemon status/stats).
func (m *Model) renderHeader() string {
	scope := m.opts.ProjectLabel
	if scope == "" {
		scope = "all projects"
	}
	host := m.opts.HostLabel
	c := m.counters

	left := fmt.Sprintf("donmai · project: %s", scope)
	if host != "" {
		left += " · host: " + host
	}
	right := fmt.Sprintf("%d running   queue %d   uptime %s",
		c.Running, c.QueueDepth, format.Duration(int(c.UptimeSeconds)))
	if c.Version != "" {
		right += "   v" + c.Version
	}

	if m.opts.Plain {
		return left + "  —  " + right
	}
	leftStyled := lipgloss.NewStyle().Bold(true).Foreground(m.theme.Accent).Render(left)
	rightStyled := lipgloss.NewStyle().Foreground(m.theme.TextSecondary).Render(right)
	gap := m.width - lipgloss.Width(leftStyled) - lipgloss.Width(rightStyled)
	if gap < 1 {
		gap = 1
	}
	bar := leftStyled + strings.Repeat(" ", gap) + rightStyled
	return theme.Header().Width(m.width).Render(bar)
}

func (m *Model) renderHelp() string {
	help := "↑↓ select   f follow/pause   g jump to tail   q quit"
	if m.opts.Plain {
		return help
	}
	return lipgloss.NewStyle().Foreground(m.theme.TextTertiary).Render(help)
}

// lineCount returns the number of display lines in s (1 + newline count).
func lineCount(s string) int {
	if s == "" {
		return 0
	}
	return strings.Count(s, "\n") + 1
}

// clampHeight truncates s to at most n lines.
func clampHeight(s string, n int) string {
	if n <= 0 {
		return ""
	}
	lines := strings.Split(s, "\n")
	if len(lines) <= n {
		return s
	}
	return strings.Join(lines[:n], "\n")
}
