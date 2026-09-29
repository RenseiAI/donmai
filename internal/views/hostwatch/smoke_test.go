package hostwatch

import (
	"strings"
	"testing"
	"time"

	"charm.land/lipgloss/v2"
	"github.com/RenseiAI/tui-components/theme"
)

// TestSmoke_RenderWidths logs full grid renders at the acceptance widths
// (40/80/120/200) in both color modes with Unicode content, selected
// borders, mixed issue ids, and an overflowing list — the terminal evidence
// for review. It asserts width budgets; the logged output is the smoke.
func TestSmoke_RenderWidths(t *testing.T) {
	tm := theme.DefaultTheme()
	now := time.Date(2026, 9, 29, 12, 0, 0, 0, time.UTC)
	cards := []SessionCard{
		{SessionID: "sess-aaaa1111", IssueIdentifier: "ENG-1284", ProjectName: "web", Provider: "claude", WorkType: "development", DaemonState: "running", ToolCalls: 37, CostUsd: 0.84, NumTurns: 5, LastActivity: "Bash: pnpm test 日本語"},
		{SessionID: "sess-bbbb2222", IssueIdentifier: "ENG-1290", ProjectName: "web", Provider: "codex", WorkType: "qa", DaemonState: "starting", ToolCalls: 3, LastActivity: "reading αβγ diff"},
		{SessionID: "sess-cccc3333", IssueIdentifier: "ENG-日本語", ProjectName: "api", Provider: "claude", WorkType: "research", DaemonState: "failed", ToolCalls: 12, CostUsd: 1.5, NumTurns: 4, LastActivity: "error: boom ✗"},
		{SessionID: "sess-dddd4444", IssueIdentifier: "", ProjectName: "api", Provider: "", WorkType: "", DaemonState: "running", ToolCalls: 1},
		{SessionID: "sess-eeee5555", IssueIdentifier: "ENG-1300", ProjectName: "web", Provider: "claude", WorkType: "development", DaemonState: "completed", ToolCalls: 50, CostUsd: 3.25, NumTurns: 9},
		{SessionID: "sess-ffff6666", IssueIdentifier: "ENG-1301", ProjectName: "web", Provider: "codex", WorkType: "development", DaemonState: "running", ToolCalls: 7},
	}
	for _, width := range []int{40, 80, 120, 200} {
		for _, plain := range []bool{true, false} {
			mode := "styled"
			if plain {
				mode = "plain"
			}
			// Selected middle card (index 2: failed state, CJK id) plus a
			// tight 6-line window to exercise overflow markers.
			full := renderGrid(tm, cards, 2, 1, width, 0, plain, now)
			windowed := renderGrid(tm, cards, 4, 1, width, 6, plain, now)
			for name, out := range map[string]string{"full": full, "windowed": windowed} {
				for i, line := range strings.Split(out, "\n") {
					if w := lipgloss.Width(line); w > width {
						t.Errorf("width=%d mode=%s %s line %d: display width %d exceeds budget: %q",
							width, mode, name, i, w, line)
					}
				}
			}
			t.Logf("=== width=%d mode=%s selected=2 full ===\n%s", width, mode, full)
			t.Logf("=== width=%d mode=%s selected=4 windowed(6) ===\n%s", width, mode, windowed)
		}
	}
}
