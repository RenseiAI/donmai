package executioncell

import (
	"encoding/json"
	"testing"
)

func TestDigestCapabilityParametersCanonicalAndStrict(t *testing.T) {
	left, err := DigestCapabilityParameters(json.RawMessage(`{"repo":"example/repo","tools":["a","b"]}`))
	if err != nil {
		t.Fatal(err)
	}
	right, err := DigestCapabilityParameters(json.RawMessage(` { "tools" : ["a","b"], "repo" : "example/repo" } `))
	if err != nil || left != right {
		t.Fatalf("equivalent object digest=%q/%q err=%v", left, right, err)
	}
	changed, err := DigestCapabilityParameters(json.RawMessage(`{"repo":"example/repo","tools":["b"]}`))
	if err != nil || changed == left {
		t.Fatalf("semantic mutation digest=%q err=%v", changed, err)
	}
	for name, raw := range map[string]json.RawMessage{
		"absent":       nil,
		"duplicate":    json.RawMessage(`{"repo":"a","repo":"b"}`),
		"trailing":     json.RawMessage(`{} {}`),
		"invalid utf8": {'"', 0xff, '"'},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := DigestCapabilityParameters(raw); err == nil {
				t.Fatal("malformed parameter value produced digest")
			}
		})
	}
	if _, err := DigestCapabilityParameters(json.RawMessage(`{}`)); err != nil {
		t.Fatalf("present-empty object rejected: %v", err)
	}
}

func TestDigestCapabilityParametersPreservesPresentEmptyAcrossTypedRemarshal(t *testing.T) {
	raw := json.RawMessage(`{"tools":[]}`)
	exact, err := DigestCapabilityParameters(raw)
	if err != nil {
		t.Fatal(err)
	}
	var typed struct {
		Tools []string `json:"tools,omitempty"`
	}
	if err := json.Unmarshal(raw, &typed); err != nil {
		t.Fatal(err)
	}
	remarshal, _ := json.Marshal(typed)
	drifted, err := DigestCapabilityParameters(remarshal)
	if err != nil {
		t.Fatal(err)
	}
	if exact == drifted {
		t.Fatal("typed remarshal erased present-empty state without changing digest")
	}
}

func TestLegacyAdapterUsesCanonicalCapabilityParameterDigest(t *testing.T) {
	raw := []byte(`{"sessionId":"parameter-digest","repository":"example/repo","codeIntel":{"tools":["a"],"repo":"example/repo"}}`)
	adapted, err := AdaptQueuedWorkJSON(raw, legacyProfile(), legacyContext())
	if err != nil {
		t.Fatal(err)
	}
	want, err := DigestCapabilityParameters(json.RawMessage(`{"tools":["a"],"repo":"example/repo"}`))
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, requirement := range adapted.Intent.RequiredCapabilities {
		if requirement.Name == "code_intelligence" {
			found = true
			if requirement.ParametersDigest != want {
				t.Fatalf("adapter digest=%q want=%q", requirement.ParametersDigest, want)
			}
		}
	}
	if !found {
		t.Fatal("adapter omitted code-intelligence capability")
	}
}
