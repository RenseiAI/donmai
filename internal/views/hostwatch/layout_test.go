package hostwatch

import (
	"fmt"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/RenseiAI/tui-components/theme"
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

func TestHeaderBudget_HostFirstAndBounded(t *testing.T) {
	left, gap := headerBudget("myhost", "1 running", 40)
	line := left + strings.Repeat(" ", gap) + "1 running"
	if !strings.HasPrefix(line, "myhost") {
		t.Errorf("header must lead with the host, got %q", line)
	}
	if w := lipgloss.Width(line); w != 38 { // width minus 2 chrome cells
		t.Errorf("header content must fit width-2, got width %d for %q", w, line)
	}
	// Narrow: the left truncates with an ellipsis, counters stay intact.
	left, gap = headerBudget("a-very-long-hostname-that-cannot-fit", "1 running", 24)
	line = left + strings.Repeat(" ", gap) + "1 running"
	if !strings.HasSuffix(line, "1 running") {
		t.Errorf("counters must survive truncation, got %q", line)
	}
	if !strings.Contains(left, "…") {
		t.Errorf("truncated host should carry an ellipsis, got %q", left)
	}
	if w := lipgloss.Width(line); w > 22 {
		t.Errorf("truncated header must fit, got width %d for %q", w, line)
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
	// Narrow styled header truncates left, keeps counters.
	m.width = 40
	out = m.renderHeader()
	if w := lipgloss.Width(out); w != 40 {
		t.Errorf("narrow styled header must fill exactly 40 columns, got %d:\n%s", w, out)
	}
	if !strings.Contains(stripANSI(out), "running") {
		t.Errorf("counters must survive narrow truncation:\n%s", out)
	}
}

func TestModel_HeaderNarrowKeepsHostAndFits(t *testing.T) {
	// Exact reproduced narrow case: plain 40 columns must keep a meaningful
	// host prefix and fit the terminal (the old renderer emitted an
	// ellipsis-led 44-cell line).
	for _, plain := range []bool{true, false} {
		m := New(Options{Plain: plain, HostLabel: "studio-test", ProjectLabel: "qa"})
		m.width, m.height = 40, 40
		m.counters = Counters{Running: 1, QueueDepth: 0, UptimeSeconds: 60, Version: "0.72.55"}
		out := m.renderHeader()
		body := out
		if !plain {
			body = stripANSI(out)
			body = strings.TrimSpace(body)
		}
		body = strings.TrimSpace(body)
		if w := lipgloss.Width(out); plain && w > 40 {
			t.Errorf("plain=40: header width %d exceeds 40: %q", w, out)
		}
		if w := lipgloss.Width(out); !plain && w != 40 {
			t.Errorf("styled=40: header width %d, want 40: %q", w, out)
		}
		if !strings.HasPrefix(body, "studio-test") {
			t.Errorf("plain=%v: header must keep the host prefix, got %q", plain, body)
		}
		if !strings.Contains(body, "1 running") {
			t.Errorf("plain=%v: running counter must survive narrowing, got %q", plain, body)
		}
	}
}

func TestModel_HeaderWidthsSweepKeepsHost(t *testing.T) {
	// 40/80/120/200, styled and plain: the line fits and the host leads.
	// Wider widths preserve the full counters; narrower ones shed version
	// then queue/uptime before touching the host.
	for _, width := range []int{40, 80, 120, 200} {
		for _, plain := range []bool{true, false} {
			m := New(Options{Plain: plain, HostLabel: "studio-test", ProjectLabel: "qa"})
			m.width, m.height = width, 40
			m.counters = Counters{Running: 1, QueueDepth: 0, UptimeSeconds: 60, Version: "0.72.55"}
			out := m.renderHeader()
			body := out
			if !plain {
				body = stripANSI(out)
				body = strings.TrimSpace(body)
			}
			if w := lipgloss.Width(out); plain && w > width {
				t.Errorf("plain/%d: header width %d exceeds budget: %q", width, w, out)
			}
			if w := lipgloss.Width(out); !plain && w != width {
				t.Errorf("styled/%d: header width %d, want %d", width, w, width)
			}
			if !strings.HasPrefix(body, "studio-test") {
				t.Errorf("plain=%v/%d: host prefix lost: %q", plain, width, body)
			}
			if width >= 80 && !strings.Contains(body, "v0.72.55") {
				t.Errorf("plain=%v/%d: wide header must keep the full counters: %q", plain, width, body)
			}
		}
	}
}

func TestModel_HeaderNarrowerBoundsHostOnly(t *testing.T) {
	// Below the width where even the bare running count fits beside a
	// meaningful host prefix, the header falls back to host-only rather
	// than an ellipsis-led overflow.
	for _, width := range []int{16, 20, 24} {
		for _, plain := range []bool{true, false} {
			m := New(Options{Plain: plain, HostLabel: "studio-test", ProjectLabel: "qa"})
			m.width, m.height = width, 40
			m.counters = Counters{Running: 1, QueueDepth: 0, UptimeSeconds: 60, Version: "0.72.55"}
			out := m.renderHeader()
			body := out
			if !plain {
				body = stripANSI(out)
				body = strings.TrimSpace(body)
			}
			if w := lipgloss.Width(out); w > width {
				t.Errorf("plain=%v/%d: header width %d exceeds budget: %q", plain, width, w, out)
			}
			if !strings.HasPrefix(body, "studi") {
				t.Errorf("plain=%v/%d: meaningful host prefix lost: %q", plain, width, body)
			}
		}
	}
}

func TestModel_HeaderUnicodeWidthsBounded(t *testing.T) {
	// CJK host label: display-cell budgets hold on both render paths and
	// the host prefix survives narrowing.
	for _, width := range []int{40, 80} {
		for _, plain := range []bool{true, false} {
			m := New(Options{Plain: plain, HostLabel: "myhost-\u65e5\u672c\u8a9e", ProjectLabel: "qa"})
			m.width, m.height = width, 40
			m.counters = Counters{Running: 1, QueueDepth: 0, UptimeSeconds: 60, Version: "0.72.55"}
			out := m.renderHeader()
			body := out
			if !plain {
				body = stripANSI(out)
				body = strings.TrimSpace(body)
			}
			if w := lipgloss.Width(out); w > width {
				t.Errorf("plain=%v/%d: unicode header width %d exceeds budget: %q", plain, width, w, out)
			}
			if !strings.HasPrefix(body, "myhost") {
				t.Errorf("plain=%v/%d: unicode host prefix lost: %q", plain, width, body)
			}
		}
	}
}

func TestModel_HeaderWideBaselinePreserved(t *testing.T) {
	// Wide-header baseline: the full host/scope/counters line is unchanged
	// where it fits (pins the pre-repair wide rendering against silent
	// counter-shedding regressions).
	m := New(Options{Plain: true, HostLabel: "studio-test", ProjectLabel: "qa"})
	m.width, m.height = 120, 40
	m.counters = Counters{Running: 1, QueueDepth: 0, UptimeSeconds: 60, Version: "0.72.55"}
	left := "studio-test · qa"
	right := "1 running   queue 0   uptime 1m   v0.72.55"
	want := left + strings.Repeat(" ", 120-2-lipgloss.Width(left)-lipgloss.Width(right)) + right
	if got := m.renderHeader(); got != want {
		t.Errorf("wide plain header changed:\n got %q\nwant %q", got, want)
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
	// At minCardWidth-1 the fallback engages; at minCardWidth cards render.
	if got := renderGrid(tm, cards, 0, 0, minCardWidth-1, 0, true, now); strings.Contains(got, "elapsed ") {
		t.Errorf("below min width should be compact, got:\n%s", got)
	}
	if got := renderGrid(tm, cards, 0, 0, minCardWidth, 0, true, now); !strings.Contains(got, "elapsed unknown") {
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
	sel := renderCard(tm, c, 0, true, false, now)
	unsel := renderCard(tm, c, 0, false, false, now)
	if sel == unsel {
		t.Error("selected card must render distinctly from unselected")
	}
	// Selected ring corners present on all four sides.
	for _, want := range []string{"╭", "╮", "╰", "╯"} {
		if !strings.Contains(sel, want) {
			t.Errorf("selected card should carry a full border ring (missing %q):\n%s", want, sel)
		}
	}
	if strings.Contains(unsel, "╭") {
		t.Errorf("unselected card should keep the single left bar, got:\n%s", unsel)
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
