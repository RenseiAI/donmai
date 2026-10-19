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
			Company:   CompanyAnthropic,
			Model:     "claude-sonnet-4-5",
			Protocol:  ProtoAnthropicMessages,
			Host:      HostDirect,
			Mechanism: AuthAPIKey,
			Auth:      AuthBYOK,
		},
	}
	b, err := json.Marshal(spec)
	if err != nil {
		t.Fatalf("json.Marshal: %v", err)
	}
	if !strings.Contains(string(b), "\"endpoint\"") {
		t.Errorf("set Endpoint was not serialized:\n%s", b)
	}
	if !strings.Contains(string(b), `"mechanism":"api_key"`) {
		t.Errorf("set Endpoint mechanism was not serialized:\n%s", b)
	}

	if spec.Endpoint.IsZero() {
		t.Errorf("a populated EndpointBinding reported IsZero")
	}
	var zero EndpointBinding
	if !zero.IsZero() {
		t.Errorf("a zero EndpointBinding did not report IsZero")
	}
	if (EndpointBinding{Mechanism: AuthNone}).IsZero() {
		t.Errorf("a mechanism-only EndpointBinding reported IsZero")
	}
}

func TestEndpointBinding_LegacyAuthRetainedWithoutMechanism(t *testing.T) {
	b, err := json.Marshal(EndpointBinding{Auth: AuthBYOK})
	if err != nil {
		t.Fatalf("json.Marshal: %v", err)
	}
	if strings.Contains(string(b), `"mechanism"`) {
		t.Errorf("zero mechanism was not omitted: %s", b)
	}
	if !strings.Contains(string(b), `"auth":"byok"`) {
		t.Errorf("legacy auth was not retained: %s", b)
	}
}

// TestEndpointBinding_RoutingPreferenceRoundTrips verifies the additive
// RoutingPreference field survives marshal/unmarshal under its camelCase
// wire tag, and is omitted when unset so pre-preference dispatches
// round-trip byte-identically.
func TestEndpointBinding_RoutingPreferenceRoundTrips(t *testing.T) {
	t.Parallel()
	in := EndpointBinding{
		Company:  CompanyOpenAI,
		Model:    "served-model",
		Protocol: ProtoOpenAIChat,
		Host:     HostGateway,
		RoutingPreference: &RoutingPreference{
			Order: []string{"anthropic", "openai"},
			Only:  []string{"anthropic"},
		},
	}
	buf, err := json.Marshal(in)
	if err != nil {
		t.Fatalf("marshal EndpointBinding: %v", err)
	}
	for _, want := range []string{`"routingPreference":{"order":["anthropic","openai"],"only":["anthropic"]}`} {
		if !strings.Contains(string(buf), want) {
			t.Fatalf("marshalled JSON missing %s: %s", want, buf)
		}
	}
	var out EndpointBinding
	if err := json.Unmarshal(buf, &out); err != nil {
		t.Fatalf("unmarshal EndpointBinding: %v", err)
	}
	if out.RoutingPreference == nil {
		t.Fatal("RoutingPreference missing after round-trip")
	}
	if len(out.RoutingPreference.Order) != 2 || len(out.RoutingPreference.Only) != 1 {
		t.Errorf("RoutingPreference round-trip = %+v, want order x2 + only x1", out.RoutingPreference)
	}

	plain, err := json.Marshal(EndpointBinding{Company: CompanyOpenAI, Model: "m"})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.Contains(string(plain), "routingPreference") {
		t.Errorf("empty RoutingPreference leaked into wire shape: %s", plain)
	}
	var back EndpointBinding
	if err := json.Unmarshal(plain, &back); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !back.RoutingPreference.IsZero() {
		t.Errorf("RoutingPreference = %+v, want unset on a pre-preference payload", back.RoutingPreference)
	}
}
