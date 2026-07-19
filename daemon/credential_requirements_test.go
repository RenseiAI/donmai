package daemon

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

func TestCredentialMetadata_NestedProfilePropagatesToSpecAndDetail(t *testing.T) {
	const raw = `{
		"sessionId":"nested-metadata",
		"resolvedProfile":{
			"provider":"claude",
			"harness":"native",
			"servingHost":"bedrock",
			"credentialRequirements":[
				{"anyOf":["ANTHROPIC_API_KEY","AWS_BEARER_TOKEN_BEDROCK"]},
				{"anyOf":["AWS_REGION","AWS_DEFAULT_REGION"]}
			]
		}
	}`
	var item PollWorkItem
	if err := json.Unmarshal([]byte(raw), &item); err != nil {
		t.Fatalf("unmarshal PollWorkItem: %v", err)
	}

	wantRequirements := []CredentialEnvRequirement{
		{AnyOf: []string{"ANTHROPIC_API_KEY", "AWS_BEARER_TOKEN_BEDROCK"}},
		{AnyOf: []string{"AWS_REGION", "AWS_DEFAULT_REGION"}},
	}
	spec := PollItemToSessionSpec(item, nil)
	detail := PollItemToSessionDetail(item, nil, "", "", "")

	for name, got := range map[string][]CredentialEnvRequirement{
		"poll resolved profile": item.ResolvedProfile.CredentialRequirements,
		"session spec":          spec.CredentialRequirements,
		"session detail":        detail.CredentialRequirements,
	} {
		if !reflect.DeepEqual(got, wantRequirements) {
			t.Errorf("%s requirements = %#v, want %#v", name, got, wantRequirements)
		}
	}
	if spec.Harness != "native" || detail.Harness != "native" {
		t.Errorf("Harness propagation = (%q, %q), want native", spec.Harness, detail.Harness)
	}
	if spec.ServingHost != "bedrock" || detail.ServingHost != "bedrock" {
		t.Errorf("ServingHost propagation = (%q, %q), want bedrock", spec.ServingHost, detail.ServingHost)
	}
}

func TestCredentialMetadata_TopLevelProjectionIsAuthoritative(t *testing.T) {
	explicitEmpty := make([]CredentialEnvRequirement, 0)
	item := PollWorkItem{
		SessionID:              "top-level-metadata",
		CredentialRequirements: explicitEmpty,
		Harness:                "top-level-harness",
		ServingHost:            "direct",
		ResolvedProfile: &SessionResolvedProfile{
			CredentialRequirements: []CredentialEnvRequirement{{AnyOf: []string{"NESTED_KEY"}}},
			Harness:                "nested-harness",
			ServingHost:            "vertex",
		},
	}

	spec := PollItemToSessionSpec(item, nil)
	detail := PollItemToSessionDetail(item, nil, "", "", "")
	for name, got := range map[string][]CredentialEnvRequirement{
		"session spec":   spec.CredentialRequirements,
		"session detail": detail.CredentialRequirements,
	} {
		if got == nil || len(got) != 0 {
			t.Errorf("%s requirements = %#v, want present empty top-level projection", name, got)
		}
	}
	if spec.Harness != "top-level-harness" || detail.Harness != "top-level-harness" {
		t.Errorf("top-level Harness did not win: spec=%q detail=%q", spec.Harness, detail.Harness)
	}
	if spec.ServingHost != "direct" || detail.ServingHost != "direct" {
		t.Errorf("top-level ServingHost did not win: spec=%q detail=%q", spec.ServingHost, detail.ServingHost)
	}
}

func TestCredentialMetadata_AbsentVersusEmptyDecodeCompatibility(t *testing.T) {
	tests := []struct {
		name        string
		raw         string
		wantPresent bool
	}{
		{name: "absent", raw: `{"sessionId":"absent"}`},
		{name: "top-level empty", raw: `{"sessionId":"top-empty","credentialRequirements":[]}`, wantPresent: true},
		{name: "nested empty", raw: `{"sessionId":"nested-empty","resolvedProfile":{"credentialRequirements":[]}}`, wantPresent: true},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var item PollWorkItem
			if err := json.Unmarshal([]byte(test.raw), &item); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			spec := PollItemToSessionSpec(item, nil)
			detail := PollItemToSessionDetail(item, nil, "", "", "")
			for name, got := range map[string][]CredentialEnvRequirement{
				"session spec":   spec.CredentialRequirements,
				"session detail": detail.CredentialRequirements,
			} {
				if test.wantPresent && got == nil {
					t.Errorf("%s requirements are nil, want present empty", name)
				}
				if !test.wantPresent && got != nil {
					t.Errorf("%s requirements = %#v, want nil/absent", name, got)
				}
				if len(got) != 0 {
					t.Errorf("%s requirements length = %d, want 0", name, len(got))
				}
			}

			// omitempty intentionally keeps both absent and explicit-empty slices
			// off the re-encoded wire, preserving legacy JSON byte shape.
			encoded, err := json.Marshal(spec)
			if err != nil {
				t.Fatalf("marshal SessionSpec: %v", err)
			}
			if strings.Contains(string(encoded), "credentialRequirements") {
				t.Errorf("empty requirements escaped omitempty: %s", encoded)
			}
		})
	}
}

func TestCredentialMetadata_GroupAndNameOrderPreserved(t *testing.T) {
	const raw = `{
		"sessionId":"ordered",
		"credentialRequirements":[
			{},
			{"anyOf":[]},
			{"anyOf":["SECOND_CHOICE","FIRST_CHOICE"]},
			{"anyOf":["ONLY_CHOICE"]}
		]
	}`
	var item PollWorkItem
	if err := json.Unmarshal([]byte(raw), &item); err != nil {
		t.Fatalf("unmarshal PollWorkItem: %v", err)
	}
	if got := item.CredentialRequirements; len(got) != 4 {
		t.Fatalf("requirement group count = %d, want 4", len(got))
	}
	if item.CredentialRequirements[0].AnyOf != nil {
		t.Errorf("absent anyOf = %#v, want nil", item.CredentialRequirements[0].AnyOf)
	}
	if item.CredentialRequirements[1].AnyOf == nil || len(item.CredentialRequirements[1].AnyOf) != 0 {
		t.Errorf("explicit empty anyOf = %#v, want present empty", item.CredentialRequirements[1].AnyOf)
	}
	if got, want := item.CredentialRequirements[2].AnyOf, []string{"SECOND_CHOICE", "FIRST_CHOICE"}; !reflect.DeepEqual(got, want) {
		t.Errorf("choice order = %v, want %v", got, want)
	}

	spec := PollItemToSessionSpec(item, nil)
	detail := PollItemToSessionDetail(item, nil, "", "", "")
	if !reflect.DeepEqual(spec.CredentialRequirements, item.CredentialRequirements) {
		t.Errorf("SessionSpec changed requirement groups: got %#v want %#v", spec.CredentialRequirements, item.CredentialRequirements)
	}
	if !reflect.DeepEqual(detail.CredentialRequirements, item.CredentialRequirements) {
		t.Errorf("SessionDetail changed requirement groups: got %#v want %#v", detail.CredentialRequirements, item.CredentialRequirements)
	}

	encoded, err := json.Marshal(detail)
	if err != nil {
		t.Fatalf("marshal SessionDetail: %v", err)
	}
	var roundTripped SessionDetail
	if err := json.Unmarshal(encoded, &roundTripped); err != nil {
		t.Fatalf("unmarshal SessionDetail: %v", err)
	}
	if len(roundTripped.CredentialRequirements) != 4 {
		t.Fatalf("round-tripped group count = %d, want 4; JSON=%s", len(roundTripped.CredentialRequirements), encoded)
	}
	if got, want := roundTripped.CredentialRequirements[2].AnyOf, []string{"SECOND_CHOICE", "FIRST_CHOICE"}; !reflect.DeepEqual(got, want) {
		t.Errorf("round-tripped choice order = %v, want %v", got, want)
	}
}

func TestCredentialMetadata_OmitemptyAcrossWireTypes(t *testing.T) {
	values := map[string]any{
		"PollWorkItem":           PollWorkItem{SessionID: "poll"},
		"SessionResolvedProfile": SessionResolvedProfile{Provider: "stub"},
		"SessionDetail":          SessionDetail{SessionID: "detail"},
		"SessionSpec":            SessionSpec{SessionID: "spec"},
	}
	for name, value := range values {
		t.Run(name, func(t *testing.T) {
			encoded, err := json.Marshal(value)
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			for _, field := range []string{"credentialRequirements", "harness", "servingHost"} {
				if strings.Contains(string(encoded), field) {
					t.Errorf("zero %s escaped omitempty in %s: %s", field, name, encoded)
				}
			}
		})
	}
}
