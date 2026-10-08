package hostwatch

import (
	"strings"
	"testing"
	"time"

	"github.com/RenseiAI/tui-components/theme"
	"github.com/charmbracelet/x/ansi"
)

// cardText renders a card and returns its rows with color stripped. Styled
// rows keep their frame characters.
func cardText(card SessionCard, selected, plain bool, now time.Time, width int) []string {
	out := renderCard(theme.DefaultTheme(), card, 0, selected, plain, now, width)
	return strings.Split(ansi.Strip(out), "\n")
}

// fullCard is a card with every essential field reported.
func fullCard(now time.Time) SessionCard {
	return SessionCard{
		SessionID: "sess-1", DaemonState: "running", IssueIdentifier: "ENG-1284",
		IssueTitle: "Fix flaky retry", Repository: "https://github.com/acme/web.git",
		ProjectName: "acme-web", Harness: "claude-code", Model: "claude-sonnet-4-5",
		WorkType: "development", StartedAtUnixMs: now.Add(-4 * time.Minute).UnixMilli(),
		Observed: true, ToolCalls: 37, CostReported: true, CostUsd: 0.84,
		TurnsReported: true, NumTurns: 5, LastActivity: "Bash: pnpm test",
		LastWorkAt: now.Add(-30 * time.Second), LastOutputAt: now.Add(-10 * time.Second),
	}
}

// TestRenderCard_EssentialRowsInFixedOrder pins the card spec: five rows in
// one order — identity, project context, model, status, activity — with
// bare values and none of the verbose identity axes.
func TestRenderCard_EssentialRowsInFixedOrder(t *testing.T) {
	now := time.Date(2026, 6, 13, 14, 5, 0, 0, time.UTC)
	want := []string{
		"ENG-1284  Fix flaky retry",
		"web · development",
		"claude-sonnet-4-5 · claude-code",
		"running 4m · 5 turns · $0.84",
		"10s ago · Bash: pnpm test",
	}
	for _, plain := range []bool{true, false} {
		rows := cardText(fullCard(now), false, plain, now, 60)
		if !plain {
			rows = rows[1 : len(rows)-1] // drop the frame rows
		}
		if len(rows) != cardBodyRows {
			t.Fatalf("plain=%v: want %d rows, got %d:\n%s", plain, cardBodyRows, len(rows), strings.Join(rows, "\n"))
		}
		for i, w := range want {
			if !strings.Contains(rows[i], w) {
				t.Errorf("plain=%v row %d = %q, want it to contain %q", plain, i+1, rows[i], w)
			}
		}
		joined := strings.Join(rows, "\n")
		for _, verbose := range []string{"Model author", "Endpoint", "Agent card", "Card ID", "Protocol", "heartbeat", "tools", "harness ", "model ", "issue ENG-1284", "project "} {
			if strings.Contains(joined, verbose) {
				t.Errorf("plain=%v: card carries verbose field %q:\n%s", plain, verbose, joined)
			}
		}
	}
}

// TestRenderCard_MissingDataIsUnknown pins "unknown is never zero": a card
// with nothing reported says what is missing and never shows a zero count,
// a zero cost, a zero duration, or an invented running state.
func TestRenderCard_MissingDataIsUnknown(t *testing.T) {
	now := time.Date(2026, 6, 13, 14, 5, 0, 0, time.UTC)
	card := SessionCard{SessionID: "sess-9"}
	for _, plain := range []bool{true, false} {
		out := strings.Join(cardText(card, false, plain, now, 60), "\n")
		for _, want := range []string{"sess-9", "project unknown · work type unknown", "model unknown · harness unknown", "state unknown elapsed unknown", "activity not reported"} {
			if !strings.Contains(out, want) {
				t.Errorf("plain=%v: card missing %q:\n%s", plain, want, out)
			}
		}
		for _, invented := range []string{"0 turns", "$0.00", " 0s", "running", "never"} {
			if strings.Contains(out, invented) {
				t.Errorf("plain=%v: unreported data rendered as %q:\n%s", plain, invented, out)
			}
		}
	}
}

// TestRenderCard_MeasuredZeroAfterObserved: once reported, a zero is a
// measurement and renders as such.
func TestRenderCard_MeasuredZeroAfterObserved(t *testing.T) {
	now := time.Date(2026, 6, 13, 14, 5, 0, 0, time.UTC)
	card := SessionCard{SessionID: "s", DaemonState: "running", CostReported: true, TurnsReported: true}
	out := strings.Join(cardText(card, false, true, now, 60), "\n")
	for _, want := range []string{"0 turns", "$0.00"} {
		if !strings.Contains(out, want) {
			t.Errorf("card output missing %q:\n%s", want, out)
		}
	}
}

// TestRenderCard_ElapsedFallsBackToAdmission: before the runner writes its
// start time, the daemon's admission time is the session's true age.
func TestRenderCard_ElapsedFallsBackToAdmission(t *testing.T) {
	now := time.Date(2026, 10, 8, 20, 30, 0, 0, time.UTC)
	card := SessionCard{SessionID: "s", DaemonState: "running", AcceptedAt: "2026-10-08T20:29:20Z"}
	if got := elapsedStr(card, now); got != "40s" {
		t.Errorf("elapsed from admission: got %q, want 40s", got)
	}
	card.StartedAtUnixMs = now.Add(-10 * time.Second).UnixMilli()
	if got := elapsedStr(card, now); got != "10s" {
		t.Errorf("runner start must win over admission: got %q, want 10s", got)
	}
	if got := elapsedStr(SessionCard{AcceptedAt: "not-a-time"}, now); got != "unknown" {
		t.Errorf("unparseable admission must stay unknown, got %q", got)
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
	now := time.Date(2026, 9, 27, 12, 5, 0, 0, time.UTC)
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
	out := strings.Join(cardText(card, false, true, now, 60), "\n")
	for _, want := range []string{"held-123", "project", "accepted 2026-09-27T12:00:00Z", "held 5m", "awaiting local execution"} {
		if !strings.Contains(out, want) {
			t.Errorf("held card missing %q:\n%s", want, out)
		}
	}
	if !strings.HasPrefix(out, "  ○ held-123") {
		t.Errorf("held state should have a static neutral glyph:\n%s", out)
	}
	for _, absent := range []string{"running", "must not appear as live activity", "$1.25", "turns"} {
		if strings.Contains(out, absent) {
			t.Errorf("held card should not show %q:\n%s", absent, out)
		}
	}
}

// TestRenderCard_ObservedModel shows what actually served: a dated snapshot
// of the requested alias replaces it; a different (fallback) model shows
// both.
func TestRenderCard_ObservedModel(t *testing.T) {
	now := time.Now()
	base := SessionCard{SessionID: "s", DaemonState: "running", Harness: "claude-code", Model: "claude-sonnet-4-5"}
	tests := []struct {
		actual, want string
	}{
		{"", "claude-sonnet-4-5 · claude-code"},
		{"claude-sonnet-4-5-20250929", "claude-sonnet-4-5-20250929 · claude-code"},
		{"claude-haiku-4-5", "claude-sonnet-4-5 → claude-haiku-4-5 · claude-code"},
	}
	for _, tc := range tests {
		c := base
		c.ActualModel = tc.actual
		rows := cardText(c, false, true, now, 80)
		if !strings.Contains(rows[2], tc.want) {
			t.Errorf("actual=%q: model row %q, want %q", tc.actual, rows[2], tc.want)
		}
	}
	// A long model shortens before the harness does.
	c := base
	c.Model = "meta/muse-spark-1.3-contributor-extended-context"
	rows := cardText(c, false, false, now, 40)
	if !strings.Contains(rows[3], "… · claude-code") {
		t.Errorf("harness must survive model truncation: %q", rows[3])
	}
}

func TestRepoShortName(t *testing.T) {
	tests := map[string]string{
		"https://github.com/acme/web.git": "web",
		"git@github.com:acme/api.git":     "api",
		"acme/docs":                       "docs",
		"https://github.com/acme/web/":    "web",
		"":                                "",
	}
	for in, want := range tests {
		if got := repoShortName(in); got != want {
			t.Errorf("repoShortName(%q)=%q want %q", in, got, want)
		}
	}
	if got := (SessionCard{ProjectName: "acme-web"}).repoName(); got != "acme-web" {
		t.Errorf("project name is the fallback context, got %q", got)
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
	out := renderGrid(tm, cards, 0, 0, 120, 0, true, now)
	if got := strings.Count(out, "ENG-1"); got < 2 {
		t.Errorf("each card should carry its own issue id (want >=2 ENG-1), got %d:\n%s", got, out)
	}
	for _, line := range strings.Split(out, "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "ENG-1" || trimmed == "ENG-2" {
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
		{SessionID: "c", IssueIdentifier: "ENG-3", WorkType: "qa", DaemonState: "running"},
	}
	// 120 columns hold three cards: one shared row of exactly one card
	// height, whatever the issues and whichever card is selected.
	out := renderGrid(tm, cards, 0, 0, 120, 0, false, now)
	for _, id := range []string{"ENG-1", "ENG-2", "ENG-3"} {
		if !strings.Contains(out, id) {
			t.Fatalf("grid missing %s:\n%s", id, out)
		}
	}
	if got := displayLines(out); got != cardHeight(false) {
		t.Errorf("want one shared row of %d lines, got %d:\n%s", cardHeight(false), got, out)
	}
}

func TestRenderGrid_ProjectContextInsideCards(t *testing.T) {
	tm := theme.DefaultTheme()
	now := time.Now()
	cards := []SessionCard{
		{SessionID: "a", IssueIdentifier: "ENG-1", Repository: "https://github.com/acme/web.git", WorkType: "development", DaemonState: "running"},
		{SessionID: "b", IssueIdentifier: "ENG-2", ProjectName: "billing", WorkType: "qa", DaemonState: "running"},
	}
	out := renderGrid(tm, cards, 0, 0, 120, 0, true, now)
	for _, want := range []string{"web · development", "billing · qa"} {
		if !strings.Contains(out, want) {
			t.Errorf("card should carry its project context %q, got:\n%s", want, out)
		}
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

// TestCleanText keeps printable Unicode and drops terminal instructions.
func TestCleanText(t *testing.T) {
	hostile := "東京\x1b]52;c;secret\x07\x1b[2J\n\r\tok\u009b31m"
	got := cleanText(hostile)
	if strings.ContainsAny(got, "\x1b\x07\n\r\t\u009b") || strings.Contains(got, "secret") {
		t.Fatalf("control or escape survived: %q", got)
	}
	if !strings.HasPrefix(got, "東京") || !strings.Contains(got, "ok") {
		t.Fatalf("printable text lost: %q", got)
	}
}
