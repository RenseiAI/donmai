package hostwatch

import (
	"fmt"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/RenseiAI/donmai/agent"
	"github.com/charmbracelet/x/ansi"
)

// fixtureNow is the fixed clock of the real-shaped fixture.
var fixtureNow = time.Date(2026, 10, 8, 20, 30, 0, 0, time.UTC)

// fixtureCards is a real-shaped host: several issues and repositories,
// long and wide-character titles (CJK, emoji with modifiers and joiners),
// a session that has reported nothing yet, a held row, a failed row, an
// observed-model snapshot, and enough sessions to overflow the grid.
func fixtureCards() []SessionCard {
	now := fixtureNow
	ms := func(d time.Duration) int64 { return now.Add(-d).UnixMilli() }
	cards := []SessionCard{
		{SessionID: "0b1e6c2a-1111", IssueIdentifier: "ENG-1284", IssueTitle: "Fix flaky retry in the upload worker", ProjectName: "acme-web", Repository: "https://github.com/acme/web.git", WorkType: "development", DaemonState: "running", Harness: "claude-code", Model: "claude-sonnet-4-5", ModelProvider: "anthropic", StartedAtUnixMs: ms(102 * time.Minute), LastHeartbeatUnixMs: ms(12 * time.Second), Observed: true, ToolCalls: 149, TurnsReported: true, NumTurns: 46, CostReported: true, CostUsd: 3.12, LastActivity: "Bash: go test ./internal/upload/...", LastWorkAt: now.Add(-4 * time.Second), LastOutputAt: now.Add(-2 * time.Second)},
		{SessionID: "1c2d3e4f-2222", IssueIdentifier: "ENG-1290", IssueTitle: "Migrate the billing reconciliation job to the new ledger schema and backfill historical rows without downtime", ProjectName: "acme-billing", Repository: "https://github.com/acme/billing.git", WorkType: "qa", DaemonState: "running", Harness: "codex", Model: "gpt-5-codex", ModelAuthor: "openai", StartedAtUnixMs: ms(12 * time.Minute), LastHeartbeatUnixMs: ms(9 * time.Second), Observed: true, ToolCalls: 18, LastActivity: "Read internal/ledger/reconcile.go", LastWorkAt: now.Add(-31 * time.Second)},
		{SessionID: "2d3e4f5a-3333", IssueIdentifier: "ENG-1301", IssueTitle: "日本語のドキュメントを更新する", ProjectName: "acme-docs", Repository: "https://github.com/acme/docs.git", WorkType: "development", DaemonState: "running", Harness: "pi", Model: "meta/muse-spark-1.3-contributor", ModelAuthor: "meta", ModelProvider: "openai", EndpointOperator: "gateway", Protocol: "openai-responses", StartedAtUnixMs: ms(58 * time.Minute), LastHeartbeatUnixMs: ms(25 * time.Second), Observed: true, ToolCalls: 142, TurnsReported: true, NumTurns: 142, LastActivity: "日本語のドキュメントを更新しています", LastOutputAt: now.Add(-1 * time.Second)},
		{SessionID: "3e4f5a6b-4444", DaemonState: "running", Repository: "https://github.com/acme/web.git", ProjectName: "acme-web", Harness: "pi", Model: "meta/muse-spark-1.3-contributor", WorkType: "qa", AcceptedAt: now.Add(-40 * time.Second).Format(time.RFC3339)},
		{SessionID: "4f5a6b7c-5555", DaemonState: "unknown", ProjectName: "acme-api", Repository: "https://github.com/acme/api.git", AcceptedAt: "2026-10-08T20:21:00Z"},
		{SessionID: "5a6b7c8d-6666", IssueIdentifier: "ENG-1310", IssueTitle: "Ship 🚀 release notes ✅ for the 👩‍💻 team 👍🏽", ProjectName: "acme-web", Repository: "https://github.com/acme/web.git", WorkType: "research", DaemonState: "starting", Harness: "claude-code", Model: "claude-opus-4-1", StartedAtUnixMs: ms(20 * time.Second)},
		{SessionID: "6b7c8d9e-7777", IssueIdentifier: "ENG-1311", IssueTitle: "Tighten the session list cache", ProjectName: "acme-web", Repository: "https://github.com/acme/web.git", WorkType: "development", DaemonState: "running", Harness: "claude-code", Model: "claude-sonnet-4-5", ActualModel: "claude-sonnet-4-5-20250929", StartedAtUnixMs: ms(5 * time.Minute), Observed: true, ToolCalls: 7, LastActivity: "Edit web/src/app.tsx", LastWorkAt: now.Add(-6 * time.Second)},
		{SessionID: "7c8d9e0f-8888", IssueIdentifier: "ENG-1312", IssueTitle: "Retry the flaky checkout test", ProjectName: "acme-web", Repository: "https://github.com/acme/web.git", WorkType: "qa", DaemonState: "failed", Harness: "codex", Model: "gpt-5-codex", StartedAtUnixMs: ms(33 * time.Minute), Errored: true, Observed: true, ToolCalls: 22, LastActivity: "error: exit status 1"},
	}
	for i := 0; i < 6; i++ {
		cards = append(cards, SessionCard{
			SessionID: fmt.Sprintf("8d9e0f1a-%04d", i), IssueIdentifier: fmt.Sprintf("ENG-14%02d", i),
			IssueTitle: "Backfill audit rows for tenant shard " + fmt.Sprint(i), Repository: "https://github.com/acme/api.git",
			WorkType: "development", DaemonState: "running", Harness: "codex", Model: "gpt-5-codex",
			StartedAtUnixMs: ms(time.Duration(i+1) * 7 * time.Minute), Observed: true, ToolCalls: i * 3,
			TurnsReported: true, NumTurns: i * 2, LastActivity: "Grep: shard_id", LastWorkAt: now.Add(-time.Duration(i+1) * time.Second),
		})
	}
	return cards
}

func fixtureCounters() Counters {
	return Counters{Sessions: 14, Running: 12, HostActive: 13, MaxSessions: 16, QueueDepth: 2, UptimeSeconds: 11775, Version: "0.72.68"}
}

// newFixtureModel builds a model over the fixture at a terminal size, with
// a few stream lines, the given selection, and a fixed clock.
func newFixtureModel(t *testing.T, width, height int, plain bool, cursor int) *Model {
	t.Helper()
	m := New(Options{Plain: plain, HostLabel: "build-host-01", Now: func() time.Time { return fixtureNow }})
	mm, _ := m.Update(tea.WindowSizeMsg{Width: width, Height: height})
	m = mm.(*Model)
	m.applySnapshot(Snapshot{Cards: fixtureCards(), Counters: fixtureCounters()})
	m.applyTailBatch([]TailEvent{
		{SessionID: "0b1e6c2a-1111", At: fixtureNow.Add(-3 * time.Second), Event: agent.ToolUseEvent{ToolName: "Bash", Input: map[string]any{"command": "go test ./internal/upload/..."}}},
		{SessionID: "1c2d3e4f-2222", At: fixtureNow.Add(-2 * time.Second), Event: agent.AssistantTextEvent{Text: "Reconciliation totals match for the sampled accounts \x1b[31mred\x1b[0m."}},
		{SessionID: "2d3e4f5a-3333", At: fixtureNow.Add(-1 * time.Second), Event: agent.ToolResultEvent{ToolName: "Edit"}},
	})
	m.cursor = cursor
	return m
}

// frameColumns returns the terminal columns of the card frame characters
// in one rendered line (color stripped). Columns count cells, so wide
// characters and emoji clusters before a frame shift it by their width.
func frameColumns(line string) []int {
	line = ansi.Strip(line)
	var cols []int
	for i, r := range line {
		if strings.ContainsRune("│┃╭╮╰╯┏┓┗┛", r) {
			cols = append(cols, ansi.StringWidth(line[:i]))
		}
	}
	return cols
}

// cellIndex returns the (row, column) of the first occurrence of needle in
// a rendered view, with the column in terminal cells; (-1, -1) if absent.
func cellIndex(view, needle string) (int, int) {
	for r, line := range strings.Split(ansi.Strip(view), "\n") {
		if i := strings.Index(line, needle); i >= 0 {
			return r, ansi.StringWidth(line[:i])
		}
	}
	return -1, -1
}
