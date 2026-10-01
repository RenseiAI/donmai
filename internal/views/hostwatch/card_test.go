package hostwatch

import (
	"strings"
	"testing"
	"time"

	"charm.land/lipgloss/v2"
	"github.com/RenseiAI/tui-components/theme"
)

func TestRenderCard_CombinedIdentityAndScope(t *testing.T) {
	now := time.Date(2026, 9, 29, 12, 0, 0, 0, time.UTC)
	card := SessionCard{
		SessionID: "session-1", IssueIdentifier: "ENG-1284", ProjectName: "web",
		Harness: "driver", Model: "model-id", ModelProvider: "vendor",
		DaemonState: "running", WorkType: "development",
	}
	for _, plain := range []bool{true, false} {
		out := renderCard(theme.DefaultTheme(), card, 0, true, plain, now)
		for _, want := range []string{
			"project web · issue ENG-1284", "harness driver", "model model-id",
			"provider vendor", "state running", "elapsed unknown", "tools not reported",
			"cost not reported", "turns not reported", "heartbeat never", "output never", "work never",
		} {
			if !strings.Contains(out, want) {
				t.Errorf("plain=%v: card missing %q:\n%s", plain, want, out)
			}
		}
		if !plain {
			for _, line := range strings.Split(out, "\n") {
				if width := lipgloss.Width(line); width > cardWidth {
					t.Errorf("selected card line width %d exceeds %d: %q", width, cardWidth, line)
				}
			}
		}
	}
}

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

func TestRenderCard_HeldSessionDoesNotImplyLiveWork(t *testing.T) {
	card := SessionCard{
		SessionID:    "held-1234",
		DaemonState:  "unknown",
		AcceptedAt:   "2026-09-27T12:00:00Z",
		ProjectName:  "local-project",
		Repository:   "https://github.com/example/project",
		ToolCalls:    4,
		CostUsd:      1.25,
		NumTurns:     2,
		LastActivity: "must not appear as live activity",
	}
	out := renderCard(theme.DefaultTheme(), card, 3, false, true, time.Now())
	for _, want := range []string{"held-123", "held", "project local-project", "repo https://github.com/example/project", "accepted 2026-09-27T12:00:00Z"} {
		if !strings.Contains(out, want) {
			t.Errorf("held card missing %q:\n%s", want, out)
		}
	}
	if !strings.HasPrefix(out, "○ held-123") {
		t.Errorf("held state should have a static neutral glyph:\n%s", out)
	}
	for _, absent := range []string{"running", "⏱", "must not appear as live activity", "$1.25"} {
		if strings.Contains(out, absent) {
			t.Errorf("held card should not show %q:\n%s", absent, out)
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
