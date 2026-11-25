package pi

// provider_request_fit_test.go — scripted conformance fixtures for two
// provider-request safeguards in the policy extension, run against the REAL
// production extensions/donmai-policy.ts under node (no `pi` binary needed;
// the harness is testdata/tool-call-bounds-harness.mjs):
//
//   - Output limit fitted to the context window. An OpenAI-compatible gateway
//     rejected a request because its input plus the requested output exceeded
//     the model's context window, and rejected every identical retry. pi sizes
//     the output limit from a characters/4 input estimate and a fixed
//     4096-token margin; with the model's maxTokens equal to its window, any
//     input of a few tens of thousands of tokens asks for more output than
//     the window has left. The extension's before_provider_request hook
//     re-derives the limit from the serialized payload.
//
//   - Context-overflow wording pi does not recognize. The same gateway words
//     the failure "exceeds the model's context window", which is outside pi's
//     own overflow patterns, so pi neither compacted nor stopped retrying the
//     identical request. The extension's message_end hook marks such an error
//     with a wording pi does recognize.

import (
	"encoding/json"
	"regexp"
	"testing"
)

// piOverflowMarker is the pinned pi's generic context-overflow fallback
// pattern; an error text it matches is compacted, never auto-retried as is.
var piOverflowMarker = regexp.MustCompile(`(?i)context[_ ]length[_ ]exceeded`)

type providerFitReport struct {
	Window                 int            `json:"window"`
	IncidentRealInputToken int            `json:"incidentRealInputTokens"`
	PiRequestedOutput      int            `json:"piRequestedOutput"`
	Cases                  map[string]any `json:"cases"`
	Hook                   struct {
		Registered bool   `json:"registered"`
		Hooked     *int   `json:"hooked"`
		Foreign    string `json:"foreign"`
	} `json:"hook"`
}

type overflowCase struct {
	Text       string  `json:"text"`
	Classified bool    `json:"classified"`
	Marked     *string `json:"marked"`
}

type contextOverflowReport struct {
	Cases        map[string]overflowCase `json:"cases"`
	NotAnError   any                     `json:"notAnError"`
	NotAssistant any                     `json:"notAssistant"`
	Hook         struct {
		Registered bool    `json:"registered"`
		Hooked     *string `json:"hooked"`
	} `json:"hook"`
}

func decodeSection[T any](t *testing.T, out map[string]any, key string) T {
	t.Helper()
	var section T
	raw, ok := out[key]
	if !ok || raw == nil {
		t.Fatalf("harness reported no %q section: the extension does not export its helpers", key)
	}
	encoded, err := json.Marshal(raw)
	if err != nil {
		t.Fatalf("re-encode %q section: %v", key, err)
	}
	if err := json.Unmarshal(encoded, &section); err != nil {
		t.Fatalf("decode %q section %s: %v", key, encoded, err)
	}
	return section
}

func TestProviderRequestFitsContextWindow(t *testing.T) {
	t.Parallel()
	report := decodeSection[providerFitReport](t, toolCallBoundsFixture(t), "providerFit")

	window := report.Window
	realInput := report.IncidentRealInputToken
	if report.PiRequestedOutput+realInput <= window {
		t.Fatalf("fixture no longer reproduces the incident: pi's %d + real input %d fits the %d window", report.PiRequestedOutput, realInput, window)
	}

	intCase := func(name string) (int, bool) {
		v, ok := report.Cases[name].(float64)
		return int(v), ok
	}
	for _, name := range []string{"maxTokens", "maxCompletionTokens", "maxOutputTokens", "thinkingMax"} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			fitted, ok := intCase(name)
			if !ok {
				t.Fatalf("no fitted output limit: %v", report.Cases[name])
			}
			if fitted+realInput > window {
				t.Errorf("input %d + fitted output %d = %d exceeds the %d window", realInput, fitted, fitted+realInput, window)
			}
			// Conservative, not crippling: the turn keeps most of the room.
			if fitted < window/2 {
				t.Errorf("fitted output %d is less than half the %d window for a %d-token input", fitted, window, realInput)
			}
		})
	}

	tests := []struct {
		name string
		want any
	}{
		// The registered ceiling still caps the fitted limit.
		{"registeredCeiling", float64(200000)},
		// A limit that already fits is left alone (no payload replacement).
		{"alreadyFits", nil},
		// Too little room for even the minimum output: left to the overflow path.
		{"noRoom", nil},
		// No output limit requested: none is invented.
		{"noOutputField", nil},
		// No context window registered: nothing to fit against.
		{"noWindow", nil},
		// The input payload is never mutated in place.
		{"originalUntouched", true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := report.Cases[tc.name]; got != tc.want {
				t.Errorf("case %s = %v, want %v", tc.name, got, tc.want)
			}
		})
	}

	// Headroom beyond a bytes/3 bound over the whole serialized payload: at
	// least the window-scaled margin (3% of the window), for a large input,
	// a small one, and one whose bytes sit mostly in the tool definitions.
	reserved, _ := report.Cases["reserved"].(map[string]any)
	wantReserved := (window*3 + 99) / 100
	for _, name := range []string{"large", "small", "toolHeavy"} {
		t.Run("reserved/"+name, func(t *testing.T) {
			t.Parallel()
			got, ok := reserved[name].(float64)
			if !ok {
				t.Fatalf("no fitted limit for the %s payload: %v", name, reserved[name])
			}
			if int(got) < wantReserved {
				t.Errorf("%s payload: fit leaves %d tokens beyond the input bound, want at least %d", name, int(got), wantReserved)
			}
		})
	}

	t.Run("thinking budget that cannot fit is left to the overflow path", func(t *testing.T) {
		t.Parallel()
		if got, ok := intCase("tightNoThinking"); !ok || got != 1500 {
			t.Fatalf("control: tight payload without thinking fitted to %v, want 1500", report.Cases["tightNoThinking"])
		}
		if got := report.Cases["tightThinking"]; got != nil {
			t.Errorf("tight payload with a thinking budget = %v, want left as built (nil)", got)
		}
	})

	t.Run("thinking budget stays below the fitted limit", func(t *testing.T) {
		t.Parallel()
		maxTokens, okMax := intCase("thinkingMax")
		budget, okBudget := intCase("thinkingBudget")
		if !okMax || !okBudget {
			t.Fatalf("thinking case: max %v budget %v", report.Cases["thinkingMax"], report.Cases["thinkingBudget"])
		}
		if budget >= maxTokens || budget < 1024 {
			t.Errorf("thinking budget %d must be in [1024, %d)", budget, maxTokens)
		}
	})

	t.Run("hook on the registered provider", func(t *testing.T) {
		t.Parallel()
		if !report.Hook.Registered {
			t.Fatal("no before_provider_request handler registered for the donmai provider")
		}
		if report.Hook.Hooked == nil || *report.Hook.Hooked+realInput > window {
			t.Errorf("hooked donmai payload output limit = %v, want input+output <= %d", report.Hook.Hooked, window)
		}
		if report.Hook.Foreign != "unchanged" {
			t.Errorf("another provider's payload was %s, want unchanged", report.Hook.Foreign)
		}
	})
}

func TestContextOverflowWordingIsMarkedForPi(t *testing.T) {
	t.Parallel()
	report := decodeSection[contextOverflowReport](t, toolCallBoundsFixture(t), "contextOverflow")

	tests := []struct {
		name       string
		classified bool
		marked     bool
	}{
		// The gateway's wording: an overflow pi's own patterns miss.
		{"gateway", true, true},
		// The common OpenAI-compatible wording.
		{"openAICompatible", true, true},
		// Already carries pi's marker: left as is.
		{"alreadyMarked", true, false},
		// A rate limit is never an overflow to pi, whatever else it says.
		{"rateLimited", true, false},
		{"unrelated", false, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			c, ok := report.Cases[tc.name]
			if !ok {
				t.Fatalf("harness reported no %q case", tc.name)
			}
			if c.Classified != tc.classified {
				t.Errorf("extension classified %q as overflow = %v, want %v", c.Text, c.Classified, tc.classified)
			}
			// One policy on both sides: the extension agrees with the Go
			// mapper, which never marks an overflow not-retryable, even on a
			// message whose own flag says it is (providerErrorDetail, which
			// applies providerretry.ContextOverflow).
			goSide := providerErrorDetail(map[string]any{"isRetryable": false}, c.Text) == c.Text
			if goSide != c.Classified {
				t.Errorf("Go mapper reads %q as overflow = %v, extension says %v", c.Text, goSide, c.Classified)
			}
			if got := c.Marked != nil; got != tc.marked {
				t.Fatalf("marked = %v (%v), want %v", got, c.Marked, tc.marked)
			}
			if c.Marked == nil {
				return
			}
			if !piOverflowMarker.MatchString(*c.Marked) {
				t.Errorf("marked text %q does not match pi's overflow pattern", *c.Marked)
			}
			if len(*c.Marked) < len(c.Text) || (*c.Marked)[:len(c.Text)] != c.Text {
				t.Errorf("marked text %q does not keep the provider's text %q first", *c.Marked, c.Text)
			}
		})
	}

	t.Run("only failed assistant messages are marked", func(t *testing.T) {
		t.Parallel()
		if report.NotAnError != nil {
			t.Errorf("a non-error message was marked: %v", report.NotAnError)
		}
		if report.NotAssistant != nil {
			t.Errorf("a non-assistant message was marked: %v", report.NotAssistant)
		}
	})

	t.Run("message_end hook", func(t *testing.T) {
		t.Parallel()
		if !report.Hook.Registered {
			t.Fatal("no message_end handler registered")
		}
		if report.Hook.Hooked == nil || !piOverflowMarker.MatchString(*report.Hook.Hooked) {
			t.Errorf("message_end replacement errorMessage = %v, want pi's overflow marker", report.Hook.Hooked)
		}
	})
}
