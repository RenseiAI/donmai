package stagewire

import (
	"encoding/json"
	"reflect"
	"testing"
)

// fullSkillJSON is a work item carrying a 1.0 skill and the repository
// policy, in the shape a control plane emits.
const fullSkillJSON = `{
	"skills": [{
		"id": "review",
		"name": "review",
		"description": "Review a change.",
		"version": "1.2.0",
		"digest": "sha256:0000000000000000000000000000000000000000000000000000000000000000",
		"files": [
			{"path": "SKILL.md", "contentBase64": "IyBSZXZpZXcK", "mode": 420, "sha256": "aa"},
			{"path": "scripts/run.sh", "contentBase64": "IyEvYmluL3NoCg==", "mode": 493, "sha256": "bb"}
		],
		"invocation": "both",
		"delivery": "native_required",
		"body": "fallback body",
		"disallowedTools": ["Bash(rm:*)"]
	}],
	"repoSkills": {"policy": "allowlist", "allow": ["conventions"]}
}`

type skillWorkItem struct {
	Skills []Skill `json:"skills,omitempty"`
	Fields
}

func TestSkillsDecodeAndRoundTrip(t *testing.T) {
	t.Parallel()
	var got skillWorkItem
	if err := json.Unmarshal([]byte(fullSkillJSON), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	want := skillWorkItem{
		Skills: []Skill{{
			ID: "review", Name: "review", Description: "Review a change.", Version: "1.2.0",
			Digest: "sha256:0000000000000000000000000000000000000000000000000000000000000000",
			Files: []SkillFile{
				{Path: "SKILL.md", ContentBase64: "IyBSZXZpZXcK", Mode: 0o644, SHA256: "aa"},
				{Path: "scripts/run.sh", ContentBase64: "IyEvYmluL3NoCg==", Mode: 0o755, SHA256: "bb"},
			},
			Invocation: SkillInvocationBoth, Delivery: SkillDeliveryNativeRequired,
			Body: "fallback body", DisallowedTools: []string{"Bash(rm:*)"},
		}},
		Fields: Fields{RepoSkills: &RepoSkills{Policy: RepoSkillsAllowlist, Allow: []string{"conventions"}}},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("decoded =\n%+v\nwant\n%+v", got, want)
	}
	// Re-encoding yields the same document the control plane sent.
	raw, err := json.Marshal(got)
	if err != nil {
		t.Fatal(err)
	}
	var again, source any
	if err := json.Unmarshal(raw, &again); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal([]byte(fullSkillJSON), &source); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(again, source) {
		t.Fatalf("round trip =\n%s\nwant the source document", raw)
	}
}

// TestSkillsLegacyShapeUnchanged pins that a pre-1.0 skill (id, body,
// disallowedTools) decodes and re-encodes byte-identically.
func TestSkillsLegacyShapeUnchanged(t *testing.T) {
	t.Parallel()
	const legacy = `{"id":"x","body":"b","disallowedTools":["Edit"]}`
	var s Skill
	if err := json.Unmarshal([]byte(legacy), &s); err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != legacy {
		t.Fatalf("re-encoded %s, want %s", raw, legacy)
	}
}

// TestSkillsDropUnknownVocabulary pins the closed vocabularies: unknown
// invocation, delivery and policy values decode to their defaults, and a
// structurally wrong mode is a decode error like any other wrong type.
func TestSkillsDropUnknownVocabulary(t *testing.T) {
	t.Parallel()
	var s Skill
	if err := json.Unmarshal([]byte(`{"id":"x","invocation":"sometimes","delivery":"teleport"}`), &s); err != nil {
		t.Fatal(err)
	}
	if s.Invocation != "" || s.Delivery != "" || s.Delivery.Effective() != SkillDeliveryNativePreferred {
		t.Fatalf("decoded %+v, want unknown values dropped to defaults", s)
	}
	var r RepoSkills
	if err := json.Unmarshal([]byte(`{"policy":"some"}`), &r); err != nil {
		t.Fatal(err)
	}
	if r.Policy != "" || r.Policy.Effective() != RepoSkillsAll {
		t.Fatalf("decoded %+v, want the default policy", r)
	}
	if err := json.Unmarshal([]byte(`{"files":[{"path":"a","mode":"0755"}]}`), &s); err == nil {
		t.Fatal("a string mode decoded; want a type error")
	}
}

// TestSkillsCloneIsDeep proves a projection never aliases the work item.
func TestSkillsCloneIsDeep(t *testing.T) {
	t.Parallel()
	in := []Skill{{ID: "x", Files: []SkillFile{{Path: "SKILL.md"}}, DisallowedTools: []string{"Edit"}}}
	out := CloneSkills(in)
	out[0].Files[0].Path, out[0].DisallowedTools[0] = "changed", "changed"
	if in[0].Files[0].Path != "SKILL.md" || in[0].DisallowedTools[0] != "Edit" {
		t.Fatal("CloneSkills aliases the source")
	}
	fields := Fields{RepoSkills: &RepoSkills{Policy: RepoSkillsAllowlist, Allow: []string{"a"}}}
	clone := fields.CloneFields()
	clone.RepoSkills.Allow[0] = "changed"
	if fields.RepoSkills.Allow[0] != "a" {
		t.Fatal("CloneFields aliases RepoSkills")
	}
	if CloneSkills(nil) != nil {
		t.Fatal("CloneSkills(nil) must stay nil")
	}
}
