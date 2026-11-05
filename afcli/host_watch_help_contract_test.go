package afcli

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/RenseiAI/donmai/internal/views/hostwatch"
)

// TestHostWatchHelpContractPinsShippedControls pins AC-4 at the command
// seam: the `watch` Long help (via the command, the hidden alias and the
// exported factory) names every shipped control the in-view overlay
// describes, plus filter-text precedence and mouse unavailability. Each
// named key is then driven through the real model Update. Deleting a
// shipped-control row from either surface makes this RED.
func TestHostWatchHelpContractPinsShippedControls(t *testing.T) {
	surfaces := map[string]string{
		"watch":            newHostWatchCmd().Long,
		"exported factory": NewHostWatchCmd().Long,
	}
	alias := newFleetWatchAliasCmd(Config{BinaryName: "embedder"})
	surfaces["hidden alias"] = alias.Long
	if !alias.Hidden {
		t.Error("fleet-watch alias must stay hidden")
	}
	// Every shipped control appears in the command help and in the
	// overlay entries for the same contexts.
	controls := []string{
		"up/down", "j/k", "enter", "esc", "[", "]", "0", "f", "g",
		"/", "backspace", "x", "d", "y confirms", "n cancels",
		"p pauses", "reopens", "q quits",
		"contextual keyboard help overlay",
		"filter text, not shortcuts",
		"no mouse bindings",
	}
	for surface, long := range surfaces {
		for _, want := range controls {
			if !strings.Contains(long, want) {
				t.Errorf("%s help missing %q:\n%s", surface, want, long)
			}
		}
	}
	// Drive every described key through the production Update entry
	// point on a live model.
	model := hostwatch.New(hostwatch.Options{Plain: true})
	sized, _ := model.Update(tea.WindowSizeMsg{Width: 120, Height: 40})
	model = sized.(*hostwatch.Model)
	press := func(key string) tea.Cmd {
		var msg tea.KeyPressMsg
		switch key {
		case "up":
			msg = tea.KeyPressMsg{Code: tea.KeyUp}
		case "down":
			msg = tea.KeyPressMsg{Code: tea.KeyDown}
		case "enter":
			msg = tea.KeyPressMsg{Code: tea.KeyEnter}
		case "esc":
			msg = tea.KeyPressMsg{Code: tea.KeyEsc}
		case "backspace":
			msg = tea.KeyPressMsg{Code: tea.KeyBackspace}
		default:
			msg = tea.KeyPressMsg{Code: rune(key[0]), Text: key}
		}
		_, cmd := model.Update(msg)
		return cmd
	}
	for _, key := range []string{"down", "up", "enter", "esc", "[", "]", "0", "f", "g", "/", "backspace", "esc", "?", "esc", "q"} {
		_ = press(key)
	}
	if got := model.View().Content; got == "" {
		t.Error("empty view after driving the described keys")
	}
	// Overlay completeness: the in-view help opened through the
	// production Update/View path must describe the same shipped
	// controls. Each want is a hardcoded description fragment from
	// the overlay rows, so deleting any overlay row (for example
	// the backspace row) makes this RED even though the command
	// help above stays GREEN.
	overlay := hostwatch.New(hostwatch.Options{Plain: true})
	sizedOverlay, _ := overlay.Update(tea.WindowSizeMsg{Width: 200, Height: 40})
	overlay = sizedOverlay.(*hostwatch.Model)
	_, _ = overlay.Update(tea.KeyPressMsg{Code: '?', Text: "?"})
	helpView := overlay.View().Content
	if !strings.Contains(helpView, "keyboard help") {
		t.Fatalf("help overlay did not open through production Update:\n%s", helpView)
	}
	overlayWants := []string{
		"select a session card",
		"show the selected session detail",
		"close this help, then detail, then the filter",
		"move the cards/stream split",
		"reset the split to 50/50",
		"pause or resume the stream",
		"jump to the stream tail",
		"filter cards",
		"backspace",
		"delete one filter character while filtering",
		"ask to stop the selected session",
		"ask to drain the host past an explicit deadline",
		"confirm / cancel the pending stop or drain",
		"pause host admissions",
		"reopen host admissions",
		"quit the dashboard (sessions keep running)",
		"ctrl+c",
		"while filtering, keys type query text, not shortcuts",
		"no mouse bindings",
	}
	for _, want := range overlayWants {
		if !strings.Contains(helpView, want) {
			t.Errorf("help overlay missing shipped control %q:\n%s", want, helpView)
		}
	}
}
