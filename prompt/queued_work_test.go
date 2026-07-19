package prompt

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

// TestQueuedWork_CodeIntel_OmittedWhenNil proves the codeIntel wire field is
// omitempty: a QueuedWork with no CodeIntel block serialises byte-identically
// to today (no "codeIntel" key), so an old platform / old runner observes the
// exact payload it always has.
func TestQueuedWork_CodeIntel_OmittedWhenNil(t *testing.T) {
	t.Parallel()

	qw := QueuedWork{SessionID: "sess_1", Repository: "owner/repo"}
	b, err := json.Marshal(qw)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.Contains(string(b), "codeIntel") {
		t.Fatalf("nil CodeIntel must be omitted; got %s", b)
	}
}

// TestQueuedWork_CodeIntel_RoundTrip proves the block round-trips through JSON
// with its camelCase wire tags intact.
func TestQueuedWork_CodeIntel_RoundTrip(t *testing.T) {
	t.Parallel()

	qw := QueuedWork{
		SessionID: "sess_1",
		CodeIntel: &CodeIntelWork{
			Repo:     "owner/repo",
			Ref:      "main",
			RepoPath: "packages/linear",
			Tools:    []string{"af_code_search_symbols", "af_code_get_repo_map"},
		},
	}
	b, err := json.Marshal(qw)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	for _, want := range []string{`"codeIntel"`, `"repo"`, `"ref"`, `"repoPath"`, `"tools"`} {
		if !strings.Contains(string(b), want) {
			t.Errorf("marshaled payload missing %s: %s", want, b)
		}
	}
	var back QueuedWork
	if err := json.Unmarshal(b, &back); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !reflect.DeepEqual(qw.CodeIntel, back.CodeIntel) {
		t.Errorf("round-trip mismatch:\n got %+v\nwant %+v", back.CodeIntel, qw.CodeIntel)
	}
}

// TestQueuedWork_CodeIntel_OptionalFieldsOmitted proves the optional inner
// fields (ref, repoPath, tools) are omitempty so a minimal repo-only block
// serialises without empty keys.
func TestQueuedWork_CodeIntel_OptionalFieldsOmitted(t *testing.T) {
	t.Parallel()

	qw := QueuedWork{CodeIntel: &CodeIntelWork{Repo: "owner/repo"}}
	b, err := json.Marshal(qw)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	for _, unwanted := range []string{`"ref"`, `"repoPath"`, `"tools"`} {
		if strings.Contains(string(b), unwanted) {
			t.Errorf("empty optional field %s must be omitted; got %s", unwanted, b)
		}
	}
	if !strings.Contains(string(b), `"repo"`) {
		t.Errorf("required repo field must be present; got %s", b)
	}
}

// TestQueuedWork_InitialPromptWireSemantics locks the public wire contract:
// camelCase, omitempty for absent/explicit-empty values, exact Unicode and
// multiline round-trip when populated.
func TestQueuedWork_InitialPromptWireSemantics(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		input      string
		wantOnWire bool
	}{
		{name: "absent zero value", input: "", wantOnWire: false},
		{name: "unicode multiline", input: "こんにちは 🌱\nsecond line", wantOnWire: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			qw := QueuedWork{SessionID: "seed-wire", InitialPrompt: tt.input}
			body, err := json.Marshal(qw)
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			hasKey := strings.Contains(string(body), `"initialPrompt"`)
			if hasKey != tt.wantOnWire {
				t.Fatalf("initialPrompt key presence = %v, want %v: %s", hasKey, tt.wantOnWire, body)
			}
			var back QueuedWork
			if err := json.Unmarshal(body, &back); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			if back.InitialPrompt != tt.input {
				t.Errorf("round-trip InitialPrompt = %q, want %q", back.InitialPrompt, tt.input)
			}
		})
	}

	// An explicit empty key from a newer peer decodes safely, then omitempty
	// returns to the same field-less shape as an older peer's absent key.
	var explicitEmpty QueuedWork
	if err := json.Unmarshal([]byte(`{"sessionId":"seed-empty","initialPrompt":""}`), &explicitEmpty); err != nil {
		t.Fatalf("decode explicit empty: %v", err)
	}
	body, err := json.Marshal(explicitEmpty)
	if err != nil {
		t.Fatalf("re-marshal explicit empty: %v", err)
	}
	if strings.Contains(string(body), `"initialPrompt"`) {
		t.Fatalf("explicit empty must re-encode as omitted, got %s", body)
	}
}

// TestQueuedWork_CodeIntel_UnknownFieldTolerance proves BOTH mixed-version
// directions decode without error:
//
//  1. old runner + new platform: a payload carrying codeIntel decodes cleanly
//     on a struct WITHOUT the field (the unknown key is ignored).
//  2. new runner + old platform: a payload with NO codeIntel decodes to a nil
//     block (capability off, zero behaviour change).
func TestQueuedWork_CodeIntel_UnknownFieldTolerance(t *testing.T) {
	t.Parallel()

	// Direction 1: old struct shape (no CodeIntel field) tolerates the block.
	type oldQueuedWork struct {
		SessionID  string `json:"sessionId,omitempty"`
		Repository string `json:"repository,omitempty"`
	}
	withBlock := `{"sessionId":"s","repository":"owner/repo",` +
		`"codeIntel":{"repo":"owner/repo","tools":["af_code_search_code"]}}`
	var old oldQueuedWork
	if err := json.Unmarshal([]byte(withBlock), &old); err != nil {
		t.Fatalf("old struct must ignore the unknown codeIntel field: %v", err)
	}
	if old.SessionID != "s" || old.Repository != "owner/repo" {
		t.Errorf("known fields must survive the tolerant decode: %+v", old)
	}

	// Direction 2: new struct tolerates a payload with no block.
	noBlock := `{"sessionId":"s","repository":"owner/repo"}`
	var fresh QueuedWork
	if err := json.Unmarshal([]byte(noBlock), &fresh); err != nil {
		t.Fatalf("new struct must decode a block-less payload: %v", err)
	}
	if fresh.CodeIntel != nil {
		t.Errorf("absent block must decode to a nil pointer, got %+v", fresh.CodeIntel)
	}
}
