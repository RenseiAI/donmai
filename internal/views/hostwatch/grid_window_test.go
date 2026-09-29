package hostwatch

import (
	"strings"
	"testing"
	"time"

	"github.com/RenseiAI/tui-components/theme"
)

func TestRenderGrid_PlainSelectionWithinHeight(t *testing.T) {
	cards := []SessionCard{
		{SessionID: "first", IssueIdentifier: "TEST-1"},
		{SessionID: "second", IssueIdentifier: "TEST-2"},
		{SessionID: "third", IssueIdentifier: "TEST-3"},
		{SessionID: "selected", IssueIdentifier: "TEST-4"},
	}
	for _, budget := range []int{1, 2, 8} {
		got := renderGrid(theme.DefaultTheme(), cards, 3, 0, 200, budget, true, time.Now())
		if !strings.Contains(got, "TEST-4") {
			t.Errorf("height %d: selected fourth card hidden:\n%s", budget, got)
		}
		if lines := displayLines(got); lines > budget {
			t.Errorf("height %d: output has %d lines:\n%s", budget, lines, got)
		}
	}
	full := renderGrid(theme.DefaultTheme(), cards, 3, 0, 200, 0, true, time.Now())
	if !strings.Contains(full, "TEST-1") || !strings.Contains(full, "TEST-4") {
		t.Errorf("zero budget means unbounded output:\n%s", full)
	}
}

func TestRenderGrid_StyledSelectionWithinTinyHeight(t *testing.T) {
	cards := []SessionCard{
		{SessionID: "first", IssueIdentifier: "TEST-1"},
		{SessionID: "selected", IssueIdentifier: "TEST-2"},
		{SessionID: "third", IssueIdentifier: "TEST-3"},
	}
	for _, budget := range []int{1, 2, 3, 8} {
		got := renderGrid(theme.DefaultTheme(), cards, 1, 0, 80, budget, false, time.Now())
		if !strings.Contains(got, "TEST-2") {
			t.Errorf("height %d: styled selected card identity hidden:\n%s", budget, got)
		}
		if lines := displayLines(got); lines > budget {
			t.Errorf("height %d: output has %d lines:\n%s", budget, lines, got)
		}
	}
}
