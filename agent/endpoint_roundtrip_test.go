package agent

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestSpec_EndpointOmittedForPreP1Producers is the §6 wire round-trip proof: a
// pre-P1-shaped Spec (one that never sets the new Endpoint field) must emit NO
// "endpoint" key, so existing serialized QueuedWork.resolvedProfile payloads
// round-trip byte-for-byte unchanged. This is why Spec.Endpoint is a POINTER
// (json:"endpoint,omitempty") — Go's encoding/json never treats a non-pointer
// struct as empty, so a value field would always serialize "endpoint":{...}.
func TestSpec_EndpointOmittedForPreP1Producers(t *testing.T) {
	tests := []struct {
		name string
		spec Spec
	}{
		{
			name: "empty spec",
			spec: Spec{},
		},
		{
			name: "typical pre-P1 spec",
			spec: Spec{
				Prompt: "do the thing",
				Cwd:    "/tmp/wt",
				Model:  "claude-sonnet-4-5",
				Env:    map[string]string{"FOO": "bar"},
			},
		},
		{
			name: "spec with tools but no endpoint",
			spec: Spec{
				Prompt:       "x",
				AllowedTools: []string{"Bash(pnpm:*)"},
				MCPServers:   []MCPServerConfig{{Name: "af_linear"}},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			b, err := json.Marshal(tt.spec)
			if err != nil {
				t.Fatalf("json.Marshal: %v", err)
			}
			if strings.Contains(string(b), "\"endpoint\"") {
				t.Errorf("pre-P1 spec emitted an \"endpoint\" key (breaks wire round-trip):\n%s", b)
			}

			// And it must round-trip back to a nil Endpoint.
			var back Spec
			if err := json.Unmarshal(b, &back); err != nil {
				t.Fatalf("json.Unmarshal: %v", err)
			}
			if back.Endpoint != nil {
				t.Errorf("round-trip produced non-nil Endpoint: %+v", back.Endpoint)
			}
		})
	}
}

// TestSpec_EndpointEmittedWhenSet confirms the field DOES serialize once a P1+
// producer sets it (so the additive field is actually wire-reachable for
// Phase 3), and that IsZero discriminates the unset case.
func TestSpec_EndpointEmittedWhenSet(t *testing.T) {
	spec := Spec{
		Prompt: "x",
		Endpoint: &EndpointBinding{
			Company:  CompanyAnthropic,
			Model:    "claude-sonnet-4-5",
			Protocol: ProtoAnthropicMessages,
			Host:     HostDirect,
			Auth:     AuthBYOK,
		},
	}
	b, err := json.Marshal(spec)
	if err != nil {
		t.Fatalf("json.Marshal: %v", err)
	}
	if !strings.Contains(string(b), "\"endpoint\"") {
		t.Errorf("set Endpoint was not serialized:\n%s", b)
	}

	if spec.Endpoint.IsZero() {
		t.Errorf("a populated EndpointBinding reported IsZero")
	}
	var zero EndpointBinding
	if !zero.IsZero() {
		t.Errorf("a zero EndpointBinding did not report IsZero")
	}
}
