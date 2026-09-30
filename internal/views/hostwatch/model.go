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

	// split is the fraction of content height given to the session-card
	// grid (the stream takes the rest). Operator-adjustable via [ ] / 0.
	split float64
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
			for i := range evs {
				evs[i].source = t
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
	case "[":
		m.setSplit(m.split - splitStep)
	case "]":
		m.setSplit(m.split + splitStep)
	case "0":
		m.setSplit(defaultSplitRatio)
	}
	return m, nil
}

// setSplit clamps ratio into bounds and re-applies the layout so the next
// resize preserves the operator-chosen ratio.
func (m *Model) setSplit(ratio float64) {
	m.split = clampSplit(ratio)
	m.layout()
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
// drop tailers for sessions that vanished or finished). Folded live metrics
// (tool counts, cost/turns, activity, freshness) survive the refresh: the
// index poll carries no metrics, so a fresh card would otherwise zero them
// on every tick. Metrics keyed by session id carry over; reordered cards
// keep their own metrics and removed ids drop theirs.
func (m *Model) applySnapshot(snap Snapshot) {
	m.snapErr = snap.Err
	m.counters = snap.Counters
	if snap.Err == nil {
		kept := make(map[string]SessionCard, len(m.cards))
		for _, c := range m.cards {
			kept[c.SessionID] = c
		}
		for i := range snap.Cards {
			if prev, ok := kept[snap.Cards[i].SessionID]; ok {
				if snap.Cards[i].sameObservedRun(prev) {
					snap.Cards[i].retainFolded(prev)
				} else {
					delete(m.tailers, snap.Cards[i].SessionID)
					// The log has no per-row run identity for old status
					// observations. On a proven replacement run, attach at
					// its current end even under --replay; old bytes cannot
					// become observations attributed to the new reader.
					if path := snap.Cards[i].EventsPath(); path != "" {
						m.tailers[snap.Cards[i].SessionID] = NewTailer(snap.Cards[i].SessionID, path, true, m.now)
					}
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
		if ev.source != nil && m.tailers[ev.SessionID] != ev.source {
			continue // old asynchronous batch from a removed/replaced reader
		}
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

// retainFolded carries the tail-accumulated live metrics from a previous
// card generation onto a refreshed card for the same session. Index polls
// carry identity and header fields only; without this the refresh would
// zero tool counts, cost/turns, activity text and freshness every tick.
// sameObservedRun uses the runner's persisted start observation, not daemon
// admission time: re-adoption can change AcceptedAt while the runner survives.
// An absent start is not evidence of another run. Session removal separately
// removes both card and tailer, and a later same-ID entry starts a fresh reader.
func (c SessionCard) sameObservedRun(prev SessionCard) bool {
	return c.StartedAtUnixMs <= 0 || prev.StartedAtUnixMs <= 0 || c.StartedAtUnixMs == prev.StartedAtUnixMs
}

func (c *SessionCard) retainFolded(prev SessionCard) {
	c.ToolCalls = prev.ToolCalls
	c.LastTool = prev.LastTool
	c.LastActivity = prev.LastActivity
	c.CostUsd = prev.CostUsd
	c.NumTurns = prev.NumTurns
	c.Errored = prev.Errored
	c.Observed = prev.Observed
	c.MetricsReported = prev.MetricsReported
	c.LastWorkAt = prev.LastWorkAt
	c.LastOutputAt = prev.LastOutputAt
	c.ActualModel = prev.ActualModel
	c.ActualModelProvider = prev.ActualModelProvider
	c.ActualModelVersion = prev.ActualModelVersion
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
	c.Observed = true
	live := !ev.Replay
	switch e := ev.Event.(type) {
	case agent.ToolUseEvent:
		c.ToolCalls++
		c.LastTool = toolUseSummary(e)
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
		if txt := collapseWS(e.Text); txt != "" {
			c.LastActivity = truncateRunes(txt, cardWidth)
			if live {
				c.LastOutputAt = ev.EventAt()
			}
		}
	case agent.ResultEvent:
		// Cost/turn counts only arrive on the terminal result event.
		if e.Cost != nil {
			c.CostUsd = e.Cost.TotalCostUsd
			c.NumTurns = e.Cost.NumTurns
			c.MetricsReported = true
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
		c.LastActivity = truncateRunes(e.Message, cardWidth)
		if live {
			c.LastOutputAt = ev.EventAt()
		}
	}
}

// layout recomputes child sizes after a resize or a split change. The
// grid and the stream share the content height (everything below the
// one-line header and above the one-line help) per the operator's split
// ratio, so a resize preserves the chosen ratio.
func (m *Model) layout() {
	if m.width == 0 || m.height == 0 {
		return
	}
	_, streamH := splitPaneHeights(m.height-2, m.split)
	// -1 for the "session stream" title row above the viewer.
	bodyH := streamH - 1
	if bodyH < 0 {
		bodyH = 0
	}
	m.stream.SetSize(m.width, bodyH)
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

	gridH, _ := splitPaneHeights(m.height-2, m.split)
	grid := renderGrid(m.theme, m.cards, m.cursor, m.frame, m.width, gridH, m.opts.Plain, m.now())

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

// renderHeader renders the one-line counters bar: the host identity first,
// then the scope (only when it narrows the default "all projects" view),
// then running/queue/uptime/version counters. All values are local (daemon
// status/stats). The single line is budgeted against the terminal width:
// the counters pin right, the host-first segment truncates with an
// ellipsis, and header padding is two spaces (one per side) — the only
// horizontal chrome the style adds.
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

// headerCounterVariants keeps useful counters in priority order, removing
// optional details before the running count when the terminal narrows.
func headerCounterVariants(c Counters) []string {
	status := fmt.Sprintf("%d running", c.Running)
	queue := fmt.Sprintf("queue %d", c.QueueDepth)
	uptime := "uptime " + format.Duration(int(c.UptimeSeconds))
	full := status + "   " + queue + "   " + uptime
	if c.Version != "" {
		full += "   v" + c.Version
	}
	variants := []string{
		full,
		status + "   " + queue + "   " + uptime,
		status + "   " + queue,
		status + "   " + uptime,
		status,
		"",
	}
	unique := make([]string, 0, len(variants))
	for _, variant := range variants {
		if len(unique) == 0 || unique[len(unique)-1] != variant {
			unique = append(unique, variant)
		}
	}
	return unique
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

// truncateWidth shortens s to at most n display cells, appending an
// ellipsis when truncated. n<=0 returns "". Unlike truncateRunes it
// accounts wide (CJK) runes, so the result always fits its budget.
func truncateWidth(s string, n int) string {
	if n <= 0 {
		return ""
	}
	if lipgloss.Width(s) <= n {
		return s
	}
	w := 0
	var out []rune
	for _, r := range s {
		rw := lipgloss.Width(string(r))
		if w+rw > n-1 {
			break
		}
		out = append(out, r)
		w += rw
	}
	return string(out) + "…"
}

func (m *Model) renderHelp() string {
	help := "↑↓ select   [ ] split   0 reset   f follow/pause   g jump to tail   q quit"
	if m.opts.Plain {
		return help
	}
	return lipgloss.NewStyle().Foreground(m.theme.TextTertiary).Render(help)
}
