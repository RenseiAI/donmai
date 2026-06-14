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

func TestRenderGrid_PlainGroupsByIssue(t *testing.T) {
	tm := theme.DefaultTheme()
	now := time.Now()
	cards := []SessionCard{
		{SessionID: "a", IssueIdentifier: "ENG-1", WorkType: "development", DaemonState: "running"},
		{SessionID: "b", IssueIdentifier: "ENG-1", WorkType: "development", DaemonState: "running"},
		{SessionID: "c", IssueIdentifier: "ENG-2", WorkType: "qa", DaemonState: "running"},
	}
	out := renderGrid(tm, cards, 0, 0, 120, true, now)
	if !strings.Contains(out, "ENG-1") || !strings.Contains(out, "ENG-2") {
		t.Fatalf("grid missing issue group heads:\n%s", out)
	}
	// The ENG-1 group HEADING (a line that starts with the issue id, no
	// leading status dot) should appear exactly once even though the group
	// has two cards. Card header lines start with "● ".
	headCount := 0
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "ENG-1  development") {
			headCount++
		}
	}
	if headCount != 1 {
		t.Errorf("ENG-1 group head should appear once, got %d:\n%s", headCount, out)
	}
}

func TestRenderGrid_Empty(t *testing.T) {
	out := renderGrid(theme.DefaultTheme(), nil, -1, 0, 80, true, time.Now())
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
