package hostwatch

import (
	"strconv"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/RenseiAI/tui-components/theme"
	"github.com/charmbracelet/x/ansi"
)

func TestDetailSeparatesModelAuthorFromEndpointSurface(t *testing.T) {
	card := SessionCard{
		SessionID: "s", DaemonState: "running", Model: "muse-spark-1.3",
		ModelAuthor: "meta", ModelProvider: "openai", EndpointOperator: "gateway",
		Protocol: "openai-chat",
	}
	assertIdentityText(t, card, "Model author meta", "Endpoint operator gateway", "Endpoint surface openai",
		"Protocol openai-chat", "Actual provider unknown", "Model muse-spark-1.3")
}

func TestDetailRejectsURLShapedDisplayAxis(t *testing.T) {
	card := SessionCard{SessionID: "s", ModelAuthor: "https://example.test/token", EndpointOperator: "path/segment", Protocol: "openai-chat"}
	out := strings.Join(renderDetail(theme.DefaultTheme(), card, time.Now(), 80, 0, true), "\n")
	if strings.Contains(out, "example.test") || strings.Contains(out, "path/segment") {
		t.Fatalf("malformed display axis escaped the detail view: %q", out)
	}
	assertIdentityText(t, card, "Model author unknown", "Endpoint operator unknown")
}

func TestDetailResponseProviderStripsControlsBeforeAxisValidation(t *testing.T) {
	card := SessionCard{SessionID: "s", ActualModelProvider: "actual\x1b]0;BAD-TITLE\a-vendor"}
	for _, plain := range []bool{false, true} {
		out := strings.Join(renderDetail(theme.DefaultTheme(), card, time.Now(), 80, 0, plain), "\n")
		if strings.Contains(out, "BAD-TITLE") {
			t.Fatalf("native identity sanitation lost: %s", out)
		}
	}
	assertIdentityText(t, card, "Actual provider actual-vendor")
}

// TestDetailMissingDataIsUnknown: the detail view states each absence the
// same three ways the cards do, never as a zero.
func TestDetailMissingDataIsUnknown(t *testing.T) {
	assertIdentityText(t, SessionCard{SessionID: "bare"},
		"Issue unknown", "Title not reported", "State unknown", "Elapsed unknown", "Harness unknown",
		"Model unknown", "Turns not reported", "Cost not reported", "Tools not reported",
		"Output never", "Work never", "Heartbeat never", "PID unknown", "Agent card unknown", "Card ID unknown")
}

func TestDetailHeldRowHasNoLiveFields(t *testing.T) {
	card := SessionCard{SessionID: "held-1", DaemonState: "unknown", Repository: "o/a", ProjectName: "a", AcceptedAt: "2026-09-27T12:00:00Z", ToolCalls: 4, LastActivity: "stale"}
	fields := detailFields(card, time.Now())
	for _, absent := range []string{"Tools", "Turns", "Cost", "Activity", "Output", "Heartbeat", "PID", "Worktree"} {
		if detailValue(fields, absent) != "" {
			t.Errorf("held row shows live field %s", absent)
		}
	}
	assertIdentityText(t, card, "State held", "Repository o/a", "Project a", "Accepted 2026-09-27T12:00:00Z")
}

// TestRenderDetail_ClipsWithMarker: a pane shorter than the field list ends
// in a marker counting the hidden fields; rows fit the width.
func TestRenderDetail_ClipsWithMarker(t *testing.T) {
	card := fixtureCards()[0]
	all := len(detailFields(card, fixtureNow))
	for _, width := range []int{40, 80, 120, 200} {
		rows := renderDetail(theme.DefaultTheme(), card, fixtureNow, width, 6, false)
		if len(rows) != 6 {
			t.Fatalf("width %d: %d rows, want 6", width, len(rows))
		}
		last := ansi.Strip(rows[5])
		if !strings.HasPrefix(last, "↓ ") || !strings.Contains(last, "more fields") {
			t.Fatalf("width %d: clipped detail lacks its marker: %q", width, last)
		}
		columns := 1
		if width >= 96 {
			columns = 2
		}
		if hidden := all - 5*columns; !strings.HasPrefix(last, "↓ "+strconv.Itoa(hidden)+" more") {
			t.Errorf("width %d: marker %q, want %d hidden", width, last, hidden)
		}
		for _, row := range rows {
			if w := lipgloss.Width(row); w > width {
				t.Fatalf("width %d: row width %d: %q", width, w, ansi.Strip(row))
			}
		}
	}
}

// TestModel_DetailToggle: enter replaces the stream pane with the selected
// session's detail (every axis reachable at the default split on a wide
// terminal), the selection keys retarget it, and esc restores the stream.
func TestModel_DetailToggle(t *testing.T) {
	m := newFixtureModel(t, 200, 40, false, 2)
	m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	view := ansi.Strip(m.render())
	if !strings.Contains(view, "session detail · ENG-1301") || strings.Contains(view, "session stream") {
		t.Fatalf("enter did not open the detail pane:\n%s", view)
	}
	for _, f := range detailFields(m.cards[2], fixtureNow) {
		if !strings.Contains(view, f.label) {
			t.Errorf("default split clipped detail field %q:\n%s", f.label, view)
		}
	}
	for _, want := range []string{"meta/muse-spark-1.3-contributor", "gateway", "openai-responses"} {
		if !strings.Contains(view, want) {
			t.Errorf("detail lacks %q", want)
		}
	}
	m.Update(keyMsg("j"))
	if view := ansi.Strip(m.render()); !strings.Contains(view, "session detail · 3e4f5a6b") {
		t.Fatalf("detail did not follow the selection:\n%s", view)
	}
	m.Update(tea.KeyPressMsg{Code: tea.KeyEscape})
	if view := ansi.Strip(m.render()); !strings.Contains(view, "session stream") || strings.Contains(view, "session detail") {
		t.Fatalf("esc did not restore the stream:\n%s", view)
	}
	empty := New(Options{Plain: true})
	empty.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	if empty.detail {
		t.Fatal("detail opened with no session to show")
	}
}
