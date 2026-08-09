package worker

import (
	"strings"
	"testing"
)

// TestMergeCapabilities verifies a registration path can always guarantee the
// tags for the lanes it runs are advertised — appended after the operator's own
// tags, order-preserving, deduped, and with empty tags dropped.
func TestMergeCapabilities(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name     string
		operator []string
		required []string
		want     []string
	}{
		{"empty_operator_gets_required", nil, []string{"code-survival-scan"}, []string{"code-survival-scan"}},
		{"operator_preserved_required_appended", []string{"gpu"}, []string{"code-survival-scan"}, []string{"gpu", "code-survival-scan"}},
		{"dedupe_when_operator_already_has_it", []string{"code-survival-scan"}, []string{"code-survival-scan"}, []string{"code-survival-scan"}},
		{"drops_empty_tags", []string{"", "gpu", ""}, []string{"code-survival-scan"}, []string{"gpu", "code-survival-scan"}},
		{"no_required_is_a_passthrough", []string{"local", "sandbox"}, nil, []string{"local", "sandbox"}},
		{"multiple_required_keep_order", nil, []string{"a", "b"}, []string{"a", "b"}},
		{"nil_everything_is_empty", nil, nil, []string{}},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got := MergeCapabilities(tc.operator, tc.required...)
			if strings.Join(got, ",") != strings.Join(tc.want, ",") {
				t.Fatalf("MergeCapabilities(%v, %v) = %v, want %v", tc.operator, tc.required, got, tc.want)
			}
		})
	}
}

// TestMergeCapabilities_DoesNotAliasInput guards against the merged slice
// sharing backing memory with the operator's slice: a caller that keeps its own
// list must not see it mutated by a later append.
func TestMergeCapabilities_DoesNotAliasInput(t *testing.T) {
	t.Parallel()

	operator := []string{"local", "sandbox"}
	got := MergeCapabilities(operator, "kg-extraction")
	got[0] = "mutated"
	if operator[0] != "local" {
		t.Fatalf("operator slice was aliased and mutated: %v", operator)
	}
}
