package hostwatch

import (
	"strings"
	"testing"
	"time"

	"github.com/RenseiAI/tui-components/theme"
)

func TestRenderCard_Plain(t *testing.T) {
	tm := theme.DefaultTheme()
	now := time.Date(2026, 6, 13, 14, 5, 0, 0, time.UTC)
	card := SessionCard{
		SessionID:       "sess-1",
		DaemonState:     "running",
		IssueIdentifier: "ENG-1284",
		Provider:        "claude",
		WorkType:        "development",
		StartedAtUnixMs: now.Add(-4 * time.Minute).UnixMilli(),
		ToolCalls:       37,
		CostUsd:         0.84,
		NumTurns:        5,
		LastActivity:    "Bash: pnpm test",
	}
	out := renderCard(tm, card, 0, false, true /*plain*/, now)
	for _, want := range []string{"ENG-1284", "development", "impl", "claude", "running", "37", "Bash: pnpm test"} {
		if !strings.Contains(out, want) {
			t.Errorf("card output missing %q:\n%s", want, out)
		}
	}
}

func TestRoleBadge(t *testing.T) {
	tests := []struct {
		workType, step, want string
	}{
		{"development", "", "impl"},
		{"qa", "", "review"},
		{"research", "", "planner"},
		{"kg-extraction", "", "kg"},
		{"", "spawning", "spawning"},
		{"", "", "agent"},
		{"custom", "", "custom"},
	}
	for _, tc := range tests {
		c := SessionCard{WorkType: tc.workType, CurrentStep: tc.step}
		if got := c.roleBadge(); got != tc.want {
			t.Errorf("roleBadge(wt=%q,step=%q)=%q want %q", tc.workType, tc.step, got, tc.want)
		}
	}
}

func TestAgeSeconds(t *testing.T) {
	now := time.Date(2026, 6, 13, 14, 0, 0, 0, time.UTC)
	c := SessionCard{StartedAtUnixMs: now.Add(-90 * time.Second).UnixMilli()}
	if got := c.ageSeconds(now); got != 90 {
		t.Errorf("ageSeconds: want 90, got %d", got)
	}
	// Zero start = zero age (no negative, no garbage).
	if got := (SessionCard{}).ageSeconds(now); got != 0 {
		t.Errorf("ageSeconds zero-start: want 0, got %d", got)
	}
}

func TestRenderGrid_PlainFlowsAcrossFullWidth(t *testing.T) {
	tm := theme.DefaultTheme()
	now := time.Now()
	cards := []SessionCard{
		{SessionID: "a", IssueIdentifier: "ENG-1", WorkType: "development", DaemonState: "running"},
		{SessionID: "b", IssueIdentifier: "ENG-1", WorkType: "development", DaemonState: "running"},
		{SessionID: "c", IssueIdentifier: "ENG-2", WorkType: "qa", DaemonState: "running"},
	}
	// Sessions flow in flat snapshot order regardless of issue grouping,
	// and issue context lives in the cards — no group heading rows.
	// (Plain mode stacks cards vertically; row-sharing applies to the
	// styled grid covered below.)
	out := renderGrid(tm, cards, 0, 0, 120, 0, true, now)
	if got := strings.Count(out, "ENG-1"); got < 2 {
		t.Errorf("each card should carry its own issue id (want >=2 ENG-1), got %d:\n%s", got, out)
	}
	for _, line := range strings.Split(out, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "ENG-1  development") || strings.HasPrefix(trimmed, "ENG-2  qa") {
			t.Errorf("grid should not emit group heading rows, got line %q:\n%s", line, out)
		}
	}
}

func TestRenderGrid_DifferentIssuesShareRows(t *testing.T) {
	tm := theme.DefaultTheme()
	now := time.Now()
	cards := []SessionCard{
		{SessionID: "a", IssueIdentifier: "ENG-1", WorkType: "development", DaemonState: "running"},
		{SessionID: "b", IssueIdentifier: "ENG-2", WorkType: "qa", DaemonState: "running"},
	}
	// 120 columns fit both 44-wide cards side by side: the styled grid
	// renders one shared row whose height equals a single card height,
	// holding cards from different issues.
	out := renderGrid(tm, cards, 0, 0, 120, 0, false, now)
	if !strings.Contains(out, "ENG-1") || !strings.Contains(out, "ENG-2") {
		t.Fatalf("grid missing cards:\n%s", out)
	}
	// Card 0 is selected (full border ring: taller); card 1 is not.
	// The shared row takes the tallest card's height.
	sel := renderCard(tm, cards[0], 0, true, false, now)
	if got, want := displayLines(out), displayLines(sel); got != want {
		t.Errorf("want one shared row of %d lines, got %d:\n%s", want, got, out)
	}
}

func TestRenderGrid_ProjectContextInsideCards(t *testing.T) {
	tm := theme.DefaultTheme()
	now := time.Now()
	cards := []SessionCard{
		{SessionID: "a", IssueIdentifier: "ENG-1", ProjectName: "web", WorkType: "development", DaemonState: "running"},
	}
	out := renderGrid(tm, cards, 0, 0, 120, 0, true, now)
	if !strings.Contains(out, "web") {
		t.Errorf("card should carry its project context, got:\n%s", out)
	}
}

func TestRenderGrid_Empty(t *testing.T) {
	out := renderGrid(theme.DefaultTheme(), nil, -1, 0, 80, 0, true, time.Now())
	if !strings.Contains(out, "No active sessions") {
		t.Errorf("empty grid should say so, got %q", out)
	}
}

func TestTruncateRunes(t *testing.T) {
	tests := []struct {
		in   string
		n    int
		want string
	}{
		{"hello", 10, "hello"},
		{"hello", 5, "hello"},
		{"hello", 4, "hel…"},
		{"hello", 0, ""},
		{"hello", 1, "h"},
	}
	for _, tc := range tests {
		if got := truncateRunes(tc.in, tc.n); got != tc.want {
			t.Errorf("truncateRunes(%q,%d)=%q want %q", tc.in, tc.n, got, tc.want)
		}
	}
}
