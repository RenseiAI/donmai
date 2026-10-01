package localqueue

import "testing"

func TestBaseBranchEvidenceRequiresClosedTransport(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, payload, mode string
		valid               bool
	}{
		{"legacy absent", `{}`, "local", true},
		{"new branch", `{"baseRef":"release/next"}`, "local/v2", true},
		{"old transport", `{"baseRef":"release/next"}`, "local", false},
		{"unknown transport", `{"baseRef":"release/next"}`, "local/v3", false},
		{"null", `{"baseRef":null}`, "local/v2", false},
		{"tag ref", `{"baseRef":"refs/tags/v1"}`, "local/v2", false},
		{"amend conflict", `{"baseRef":"main","ref":"work/existing"}`, "local/v2", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := validateBaseBranchEvidence([]byte(tc.payload), tc.mode)
			if (err == nil) != tc.valid {
				t.Fatalf("valid=%v err=%v", tc.valid, err)
			}
		})
	}
}
