package stubagent

import (
	"errors"
	"strings"
	"testing"

	"github.com/RenseiAI/donmai/a2a"
)

func TestEncodeA2ALineRoundTrip(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		directive A2ADirective
		wantRole  a2a.Role
	}{
		{
			name:      "role defaults to agent",
			directive: A2ADirective{Text: "ready"},
			wantRole:  a2a.RoleAgent,
		},
		{
			name:      "explicit user role",
			directive: A2ADirective{Text: "ping", Role: string(a2a.RoleUser)},
			wantRole:  a2a.RoleUser,
		},
		{
			name:      "context and task ids ride through",
			directive: A2ADirective{Text: "ack", ContextID: "ctx-1", TaskID: "task-1"},
			wantRole:  a2a.RoleAgent,
		},
	}

	scenario := Scenario{Version: ScenarioVersion, Name: "round-trip", Seed: 42}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			line, err := EncodeA2ALine(scenario, 0, tc.directive)
			if err != nil {
				t.Fatalf("EncodeA2ALine: %v", err)
			}
			if !strings.HasPrefix(line, A2ALinePrefix) {
				t.Fatalf("line %q lacks prefix %q", line, A2ALinePrefix)
			}
			if strings.ContainsAny(line, "\n\r") {
				t.Fatalf("line %q contains a newline; the marker is line-addressable by construction", line)
			}
			got, err := ParseA2ALine(line)
			if err != nil {
				t.Fatalf("ParseA2ALine: %v", err)
			}
			if got.Role != tc.wantRole {
				t.Errorf("Role = %q, want %q", got.Role, tc.wantRole)
			}
			if got.ContextID != tc.directive.ContextID || got.TaskID != tc.directive.TaskID {
				t.Errorf("ContextID/TaskID = %q/%q, want %q/%q",
					got.ContextID, got.TaskID, tc.directive.ContextID, tc.directive.TaskID)
			}
			if len(got.Parts) != 1 {
				t.Fatalf("len(Parts) = %d, want 1", len(got.Parts))
			}
			if text, ok := got.Parts[0].Text(); !ok || text != tc.directive.Text {
				t.Errorf("Parts[0].Text() = %q/%v, want %q/true", text, ok, tc.directive.Text)
			}
			if got.MessageID == "" {
				t.Error("MessageID is empty")
			}
		})
	}
}

// TestEncodeA2ALineIsDeterministic is the property the whole harness exists
// for: the same scenario identity and step index produce the same message id,
// so two runs of one scenario can be compared byte for byte. A clock- or
// random-sourced id would pass every round-trip test above and fail this one.
func TestEncodeA2ALineIsDeterministic(t *testing.T) {
	t.Parallel()

	scenario := Scenario{Version: ScenarioVersion, Name: "fixed", Seed: 5}
	directive := A2ADirective{Text: "hello"}

	first, err := EncodeA2ALine(scenario, 2, directive)
	if err != nil {
		t.Fatalf("EncodeA2ALine: %v", err)
	}
	second, err := EncodeA2ALine(scenario, 2, directive)
	if err != nil {
		t.Fatalf("EncodeA2ALine: %v", err)
	}
	if first != second {
		t.Fatalf("two encodings of the same step differ:\n%s\n%s", first, second)
	}

	// ...and the id is a function of the identity, not a constant: a different
	// seed, name or index must move it, or "deterministic" would be satisfied
	// by hardcoding one value.
	for _, other := range []struct {
		name     string
		scenario Scenario
		index    int
	}{
		{name: "different seed", scenario: Scenario{Version: ScenarioVersion, Name: "fixed", Seed: 6}, index: 2},
		{name: "different name", scenario: Scenario{Version: ScenarioVersion, Name: "other", Seed: 5}, index: 2},
		{name: "different index", scenario: scenario, index: 3},
	} {
		t.Run(other.name, func(t *testing.T) {
			t.Parallel()
			line, err := EncodeA2ALine(other.scenario, other.index, directive)
			if err != nil {
				t.Fatalf("EncodeA2ALine: %v", err)
			}
			if line == first {
				t.Errorf("%s produced the same message id as the base case", other.name)
			}
		})
	}
}

func TestParseA2ALineRejects(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		line    string
		wantErr error
		wantSub string
	}{
		{name: "unmarked line", line: "just terminal output", wantErr: ErrNotA2ALine},
		{name: "empty", line: "", wantErr: ErrNotA2ALine},
		{name: "marked but not JSON", line: A2ALinePrefix + "{oops", wantSub: "decode a2a line"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := ParseA2ALine(tc.line)
			if tc.wantErr != nil {
				if !errors.Is(err, tc.wantErr) {
					t.Fatalf("err = %v, want %v", err, tc.wantErr)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantSub) {
				t.Fatalf("err = %v, want one containing %q", err, tc.wantSub)
			}
		})
	}
}

func TestParseA2ALineToleratesLineEndings(t *testing.T) {
	t.Parallel()

	// A PTY turns "\n" into "\r\n" on the way out, so a consumer splitting the
	// terminal stream hands this function a line that still carries the CR.
	scenario := Scenario{Version: ScenarioVersion, Name: "crlf"}
	line, err := EncodeA2ALine(scenario, 0, A2ADirective{Text: "over the wire"})
	if err != nil {
		t.Fatalf("EncodeA2ALine: %v", err)
	}
	got, err := ParseA2ALine(line + "\r\n")
	if err != nil {
		t.Fatalf("ParseA2ALine: %v", err)
	}
	if len(got.Parts) != 1 {
		t.Fatalf("len(Parts) = %d, want 1", len(got.Parts))
	}
	if text, ok := got.Parts[0].Text(); !ok || text != "over the wire" {
		t.Errorf("Parts[0].Text() = %q/%v, want \"over the wire\"/true", text, ok)
	}
}

func TestA2AMessageRejectsBadDirectives(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		directive A2ADirective
		wantSub   string
	}{
		{name: "empty text", directive: A2ADirective{Text: "   "}, wantSub: "a2a text is required"},
		{name: "unknown role", directive: A2ADirective{Text: "x", Role: "ROLE_MYSTERY"}, wantSub: "unknown a2a role"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if _, err := A2AMessage(Scenario{Version: ScenarioVersion}, 0, tc.directive); err == nil ||
				!strings.Contains(err.Error(), tc.wantSub) {
				t.Fatalf("err = %v, want one containing %q", err, tc.wantSub)
			}
		})
	}
}
