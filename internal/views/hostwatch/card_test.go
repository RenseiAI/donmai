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
		Harness:         "claude-code",
		Model:           "claude-sonnet-4-5",
		ModelProvider:   "anthropic",
		Provider:        "claude",
		WorkType:        "development",
		StartedAtUnixMs: now.Add(-4 * time.Minute).UnixMilli(),
		ToolCalls:       37,
		Observed:        true,
		CostUsd:         0.84,
		NumTurns:        5,
		MetricsReported: true,
		LastActivity:    "Bash: pnpm test",
		LastWorkAt:      now.Add(-30 * time.Second),
		LastOutputAt:    now.Add(-10 * time.Second),
	}
	out := renderCard(tm, card, 0, false, true /*plain*/, now)
	for _, want := range []string{
		"ENG-1284", "development",
		"harness claude-code", "model claude-sonnet-4-5", "provider anthropic", "state running",
		"tools 37", "Bash: pnpm test",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("card output missing %q:\n%s", want, out)
		}
	}
}

func TestRenderCard_MissingDataIsUnknown(t *testing.T) {
	tm := theme.DefaultTheme()
	now := time.Date(2026, 6, 13, 14, 5, 0, 0, time.UTC)
	// A card straight from an old daemon with no state.json: no invented
	// running state, no zero metrics — every absent field is labeled.
	card := SessionCard{SessionID: "sess-9"}
	out := renderCard(tm, card, 0, false, true /*plain*/, now)
	for _, want := range []string{
		"harness unknown", "model unknown", "provider unknown", "state unknown",
		"elapsed unknown", "tools not reported", "cost not reported", "turns not reported",
		"heartbeat never", "output never", "work never",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("card output missing %q:\n%s", want, out)
		}
	}
	for _, bad := range []string{"· agent · running", "unknown · unknown · running"} {
		if strings.Contains(out, bad) {
			t.Errorf("card invents defaults in %q:\n%s", bad, out)
		}
	}
}

func TestRenderCard_MeasuredZeroAfterObserved(t *testing.T) {
	tm := theme.DefaultTheme()
	now := time.Date(2026, 6, 13, 14, 5, 0, 0, time.UTC)
	// Once the tail has been observed, a zero count is measured — it must
	// render as 0, not "not reported".
	card := SessionCard{SessionID: "s", Observed: true, MetricsReported: true}
	out := renderCard(tm, card, 0, false, true /*plain*/, now)
	for _, want := range []string{"tools 0", "turns 0"} {
		if !strings.Contains(out, want) {
			t.Errorf("card output missing %q:\n%s", want, out)
		}
	}
}

func TestIsLiveState_EmptyIsNotLive(t *testing.T) {
	if isLiveState("") {
		t.Error("empty daemon state must not animate as running")
	}
	if !isLiveState("running") {
		t.Error("running must stay live")
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
