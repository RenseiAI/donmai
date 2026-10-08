package hostwatch

import (
	"fmt"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/RenseiAI/tui-components/theme"
	"github.com/charmbracelet/x/ansi"
)

func TestSplitPaneHeights_DefaultFiftyFifty(t *testing.T) {
	gridH, streamH := splitPaneHeights(20, defaultSplitRatio)
	if gridH != 10 || streamH != 10 {
		t.Errorf("50/50 of 20: want (10,10), got (%d,%d)", gridH, streamH)
	}
}

func TestSplitPaneHeights_ClampAndMinimums(t *testing.T) {
	tests := []struct {
		name       string
		contentH   int
		ratio      float64
		wantGrid   int
		wantStream int
	}{
		{"clamp low", 20, 0.0, 4, 16},
		{"clamp high", 20, 1.5, 16, 4},
		{"odd content rounds", 9, 0.5, 5, 4}, // round(4.5)=4? math.Round(4.5)=5
		{"tiny terminal keeps 1 grid row", 2, 0.5, 1, 1},
		{"single row all grid", 1, 0.5, 1, 0},
		{"zero content", 0, 0.5, 0, 0},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			gridH, streamH := splitPaneHeights(tc.contentH, tc.ratio)
			if gridH != tc.wantGrid || streamH != tc.wantStream {
				t.Errorf("splitPaneHeights(%d,%v)=(%d,%d) want (%d,%d)",
					tc.contentH, tc.ratio, gridH, streamH, tc.wantGrid, tc.wantStream)
			}
			if gridH+streamH != tc.contentH {
				t.Errorf("panes must sum to content: %d+%d != %d", gridH, streamH, tc.contentH)
			}
		})
	}
}

func TestClampSplit(t *testing.T) {
	if got := clampSplit(0.5); got != 0.5 {
		t.Errorf("mid ratio must pass through, got %v", got)
	}
	if got := clampSplit(-1); got != minSplitRatio {
		t.Errorf("low must clamp to %v, got %v", minSplitRatio, got)
	}
	if got := clampSplit(99); got != maxSplitRatio {
		t.Errorf("high must clamp to %v, got %v", maxSplitRatio, got)
	}
}

func TestModel_SplitKeysAdjustResetAndPersist(t *testing.T) {
	m := New(Options{Plain: true})
	m.width, m.height = 120, 40
	if m.split != defaultSplitRatio {
		t.Fatalf("initial split: want %v, got %v", defaultSplitRatio, m.split)
	}
	m.handleKey(keyMsg("["))
	m.handleKey(keyMsg("["))
	if m.split >= defaultSplitRatio {
		t.Errorf("[ should shrink the grid share, got %v", m.split)
	}
	afterResize := m.split
	mm, _ := m.Update(newWindowSizeMsg(100, 30))
	m = mm.(*Model)
	if m.split != afterResize {
		t.Errorf("resize must preserve the ratio: want %v, got %v", afterResize, m.split)
	}
	m.handleKey(keyMsg("]"))
	if m.split != afterResize+splitStep {
		t.Errorf("] should grow one step, got %v", m.split)
	}
	m.handleKey(keyMsg("0"))
	if m.split != defaultSplitRatio {
		t.Errorf("0 should reset to %v, got %v", defaultSplitRatio, m.split)
	}
	// Bounds hold under repeated presses.
	for i := 0; i < 20; i++ {
		m.handleKey(keyMsg("["))
	}
	if m.split != minSplitRatio {
		t.Errorf("repeated [ must clamp at %v, got %v", minSplitRatio, m.split)
	}
	for i := 0; i < 20; i++ {
		m.handleKey(keyMsg("]"))
	}
	if m.split != maxSplitRatio {
		t.Errorf("repeated ] must clamp at %v, got %v", maxSplitRatio, m.split)
	}
}

func TestTruncateWidth_CJK(t *testing.T) {
	if got := truncateWidth("日本語test", 7); lipgloss.Width(got) != 7 {
		t.Errorf("CJK truncation must hit its budget, got %q (width %d)", got, lipgloss.Width(got))
	}
	if got := truncateWidth("abc", 10); got != "abc" {
		t.Errorf("short strings pass through, got %q", got)
	}
	if got := truncateWidth("abcdef", 0); got != "" {
		t.Errorf("zero budget returns empty, got %q", got)
	}
}

// TestTruncateWidth_Graphemes: emoji with modifiers and joiners are one
// cluster each — truncation never splits one or misjudges its width.
func TestTruncateWidth_Graphemes(t *testing.T) {
	tests := []struct {
		in   string
		n    int
		want string
	}{
		{"ab👍🏽cd", 5, "ab👍🏽…"},
		{"x👩‍💻yz", 4, "x👩‍💻…"},
		{"ok", 2, "ok"},
	}
	for _, tc := range tests {
		got := truncateWidth(tc.in, tc.n)
		if got != tc.want || ansi.StringWidth(got) > tc.n {
			t.Errorf("truncateWidth(%q,%d)=%q (%d cells), want %q", tc.in, tc.n, got, ansi.StringWidth(got), tc.want)
		}
	}
}

// TestModel_HeaderShowsHostSlots: the header carries the host's occupied
// and total session slots after the queue, and drops them before the queue
// (but after uptime and version) when narrowing.
func TestModel_HeaderShowsHostSlots(t *testing.T) {
	m := New(Options{Plain: true, HostLabel: "studio-test"})
	m.counters = Counters{Running: 3, HostActive: 9, MaxSessions: 64, QueueDepth: 1, UptimeSeconds: 60, Version: "1.2.3"}
	m.width = 120
	if out := m.renderHeader(); !strings.Contains(out, "3 running   queue 1   slots 9/64   uptime 1m   v1.2.3") {
		t.Errorf("wide header lacks host slots: %q", out)
	}
	m.width = 48
	if out := m.renderHeader(); !strings.Contains(out, "3 running   queue 1   slots 9/64") || strings.Contains(out, "uptime") {
		t.Errorf("slots must outlast uptime/version: %q", out)
	}
	m.width = 36
	if out := m.renderHeader(); !strings.Contains(out, "3 running   queue 1") || strings.Contains(out, "slots") {
		t.Errorf("narrow header keeps the queue before slots: %q", out)
	}
	m.counters.MaxSessions = 0 // an older daemon without capacity
	m.width = 120
	if out := m.renderHeader(); strings.Contains(out, "slots") {
		t.Errorf("unknown capacity must not render as slots: %q", out)
	}
}

func TestModel_HeaderHostFirst(t *testing.T) {
	m := New(Options{Plain: true, ProjectLabel: "owner/repo", HostLabel: "myhost"})
	m.width, m.height = 120, 40
	m.counters = Counters{Running: 2, QueueDepth: 1, UptimeSeconds: 60, Version: "1.0"}
	out := m.renderHeader()
	if !strings.HasPrefix(out, "myhost") {
		t.Errorf("header must lead with the host, got %q", out)
	}
	if strings.Contains(out, "donmai · project:") {
		t.Errorf("header must not carry the old product/project filler, got %q", out)
	}
	if !strings.Contains(out, "owner/repo") {
		t.Errorf("non-default scope should follow the host, got %q", out)
	}
	// Default scope stays out: host-first, nothing else on the left.
	m.opts.ProjectLabel = ""
	out = m.renderHeader()
	if strings.Contains(out, "all projects") {
		t.Errorf("default scope should not pad the header, got %q", out)
	}
}

func TestModel_HeaderStyledWidthBounded(t *testing.T) {
	m := New(Options{HostLabel: "myhost-日本語", ProjectLabel: "owner/repo"})
	m.width, m.height = 80, 40
	m.counters = Counters{Running: 2}
	out := m.renderHeader()
	if w := lipgloss.Width(out); w != 80 {
		t.Errorf("styled header must fill exactly 80 columns, got %d:\n%s", w, out)
	}
	// Narrow styled header keeps the complete host and primary counter while
	// reducing secondary counters.
	m.width = 40
	out = m.renderHeader()
	if w := lipgloss.Width(out); w != 40 {
		t.Errorf("narrow styled header must fill exactly 40 columns, got %d:\n%s", w, out)
	}
	if !strings.Contains(stripANSI(out), "running") {
		t.Errorf("primary counter must survive narrow truncation:\n%s", out)
	}
	if !strings.Contains(stripANSI(out), "myhost-日本語") {
		t.Errorf("narrow header must preserve the host prefix:\n%s", out)
	}
}

func TestModel_HeaderWidthPriority(t *testing.T) {
	for _, plain := range []bool{true, false} {
		mode := "styled"
		if plain {
			mode = "plain"
		}
		for _, width := range []int{40, 80, 120, 200} {
			t.Run(fmt.Sprintf("%s/%d", mode, width), func(t *testing.T) {
				m := New(Options{Plain: plain, HostLabel: "studio-test", ProjectLabel: "qa"})
				m.width, m.height = width, 36
				m.counters = Counters{Running: 1, QueueDepth: 0, UptimeSeconds: 60, Version: "0.72.55"}
				out := m.renderHeader()
				if lipgloss.Height(out) != 1 {
					t.Fatalf("header must remain one line, got %d: %q", lipgloss.Height(out), out)
				}
				gotWidth := lipgloss.Width(out)
				if (!plain && gotWidth != width) || (plain && gotWidth > width) {
					t.Errorf("header physical width=%d, terminal width=%d, plain=%v: %q", gotWidth, width, plain, out)
				}
				visible := strings.TrimSpace(stripANSI(out))
				if !strings.HasPrefix(visible, "studio-test") {
					t.Errorf("header lost the meaningful host prefix: %q", visible)
				}
				if !strings.Contains(visible, "· qa") {
					t.Errorf("header dropped a scope that fits beside the host: %q", visible)
				}
				if !strings.Contains(visible, "1 running") {
					t.Errorf("header lost its primary counter: %q", visible)
				}
				if width == 40 {
					if !strings.Contains(visible, "queue 0") || strings.Contains(visible, "uptime") || strings.Contains(visible, "v0.72.55") {
						t.Errorf("40-column header should keep queue and drop lower-priority uptime/version: %q", visible)
					}
				} else if !strings.Contains(visible, "uptime 1m") || !strings.Contains(visible, "v0.72.55") {
					t.Errorf("wide header should preserve complete counters: %q", visible)
				}
			})
		}
	}
}

func TestModel_HeaderTinyWidthsAndUnicode(t *testing.T) {
	for _, plain := range []bool{true, false} {
		for _, width := range []int{1, 2, 3, 4, 5, 8, 12, 20, 40} {
			t.Run(fmt.Sprintf("plain=%t/width=%d", plain, width), func(t *testing.T) {
				m := New(Options{Plain: plain, HostLabel: "東京-node", ProjectLabel: "team/qa"})
				m.width, m.height = width, 10
				m.counters = Counters{Running: 1, QueueDepth: 0, UptimeSeconds: 60, Version: "0.72.55"}
				out := m.renderHeader()
				gotWidth := lipgloss.Width(out)
				if (!plain && gotWidth != width) || (plain && gotWidth > width) {
					t.Errorf("tiny/Unicode header width=%d exceeds terminal width=%d: %q", gotWidth, width, out)
				}
				if lipgloss.Height(out) != 1 {
					t.Errorf("tiny/Unicode header must remain one line, got %q", out)
				}
				visible := strings.TrimSpace(stripANSI(out))
				if width >= 12 && !strings.HasPrefix(visible, "東京-node") {
					t.Errorf("header must preserve the full Unicode host before optional scope/counters: %q", visible)
				}
				if width < 12 && width > 2 && !strings.HasPrefix(visible, "東") {
					t.Errorf("truncated Unicode host must retain its first glyph: %q", visible)
				}
			})
		}
	}
}

func TestRenderGrid_SelectedStaysVisibleOnOverflow(t *testing.T) {
	tm := theme.DefaultTheme()
	now := time.Now()
	var cards []SessionCard
	for i := 0; i < 10; i++ {
		cards = append(cards, SessionCard{
			SessionID:       string(rune('a'+i)) + "-sess",
			IssueIdentifier: fmt.Sprintf("ENG-%d", i),
			WorkType:        "development",
			DaemonState:     "running",
		})
	}
	// A one-line pane prioritizes the selected card's identity; no marker
	// can fit beside it within the height budget.
	out := renderGrid(tm, cards, 9, 0, 120, 1, true, now)
	if !strings.Contains(out, "ENG-9") || displayLines(out) != 1 {
		t.Errorf("one-line pane must show the selected card only, got:\n%s", out)
	}
	// Roomier budget: selection visible, both markers present.
	out = renderGrid(tm, cards, 5, 0, 120, 3, true, now)
	if !strings.Contains(out, "↑") || !strings.Contains(out, "↓") {
		t.Errorf("windowed grid should mark both sides, got:\n%s", out)
	}
}

func TestRenderGrid_TinyTerminalCompactList(t *testing.T) {
	tm := theme.DefaultTheme()
	now := time.Now()
	cards := []SessionCard{
		{SessionID: "abcdef0123", IssueIdentifier: "ENG-1", WorkType: "development", DaemonState: "running"},
		{SessionID: "1234567890", IssueIdentifier: "ENG-2", WorkType: "qa", DaemonState: "running"},
	}
	out := renderGrid(tm, cards, 1, 0, 30, 0, true, now)
	for _, line := range strings.Split(out, "\n") {
		if w := lipgloss.Width(line); w > 30 {
			t.Errorf("compact line exceeds width: %d > 30: %q", w, line)
		}
	}
	if !strings.Contains(out, ">") {
		t.Errorf("compact list must mark the selection, got:\n%s", out)
	}
	if !strings.Contains(out, "ENG-1") || !strings.Contains(out, "ENG-2") {
		t.Errorf("compact list must keep issue ids, got:\n%s", out)
	}
	// At minCardWidth-1 the fallback engages (one line per session); at
	// minCardWidth full cards render (their activity row appears).
	if got := renderGrid(tm, cards, 0, 0, minCardWidth-1, 0, true, now); strings.Contains(got, "activity not reported") || displayLines(got) != len(cards) {
		t.Errorf("below min width should be compact, got:\n%s", got)
	}
	if got := renderGrid(tm, cards, 0, 0, minCardWidth, 0, true, now); !strings.Contains(got, "activity not reported") {
		t.Errorf("at min width full cards should render, got:\n%s", got)
	}
}

func TestRenderGrid_WidthsBounded(t *testing.T) {
	tm := theme.DefaultTheme()
	now := time.Now()
	cards := []SessionCard{
		{SessionID: "sess-αβγ-1", IssueIdentifier: "ENG-日本語", WorkType: "development", DaemonState: "running", Provider: "x", LastActivity: "doing things αβγ"},
		{SessionID: "sess-2", IssueIdentifier: "ENG-2", WorkType: "qa", DaemonState: "running", Provider: "y"},
		{SessionID: "sess-3", IssueIdentifier: "ENG-3", WorkType: "research", DaemonState: "failed", Provider: "z"},
	}
	for _, width := range []int{40, 80, 120, 200} {
		for _, plain := range []bool{true, false} {
			out := renderGrid(tm, cards, 1, 0, width, 0, plain, now)
			for _, line := range strings.Split(out, "\n") {
				if w := lipgloss.Width(line); w > width {
					t.Errorf("width=%d plain=%v: line width %d exceeds budget: %q", width, plain, w, line)
				}
			}
		}
	}
}

func TestRenderCard_SelectedBorder(t *testing.T) {
	tm := theme.DefaultTheme()
	now := time.Now()
	c := SessionCard{SessionID: "s1", IssueIdentifier: "ENG-1", WorkType: "development", DaemonState: "running"}
	sel := ansi.Strip(renderCard(tm, c, 0, true, false, now, 40))
	unsel := ansi.Strip(renderCard(tm, c, 0, false, false, now, 40))
	// Selection reads without color: a heavy frame and a ▸ marker.
	for _, want := range []string{"┏", "┓", "┗", "┛", "▸"} {
		if !strings.Contains(sel, want) {
			t.Errorf("selected card missing %q:\n%s", want, sel)
		}
	}
	for _, want := range []string{"╭", "╮", "╰", "╯"} {
		if !strings.Contains(unsel, want) {
			t.Errorf("unselected card missing %q:\n%s", want, unsel)
		}
	}
	if strings.Contains(unsel, "▸") {
		t.Errorf("unselected card must not carry the selection marker:\n%s", unsel)
	}
}

// stripANSI drops SGR escape sequences for content assertions on styled output.
func stripANSI(s string) string {
	var out strings.Builder
	inEsc := false
	for i := 0; i < len(s); i++ {
		if !inEsc {
			if s[i] == 0x1b && i+1 < len(s) && s[i+1] == '[' {
				inEsc = true
				i++
				continue
			}
			out.WriteByte(s[i])
			continue
		}
		if (s[i] >= 'a' && s[i] <= 'z') || (s[i] >= 'A' && s[i] <= 'Z') {
			inEsc = false
		}
	}
	return out.String()
}

func newWindowSizeMsg(w, h int) tea.WindowSizeMsg { return tea.WindowSizeMsg{Width: w, Height: h} }
