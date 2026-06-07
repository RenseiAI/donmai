package access

import (
	"errors"
	"strings"
	"testing"

	"github.com/RenseiAI/donmai/agent"
)

// TestIsCellAllowed locks the isCellAllowed mirror (access-policy.ts:206-209):
// nil cell (absent rule) ⇒ allow/inherit; explicit allowed:false ⇒ deny;
// allowed:true ⇒ allow.
func TestIsCellAllowed(t *testing.T) {
	tests := []struct {
		name string
		cell *AccessCostCell
		want bool
	}{
		{"nil-inherits-allow", nil, true},
		{"explicit-false-denies", &AccessCostCell{Allowed: false}, false},
		{"explicit-true-allows", &AccessCostCell{Allowed: true}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isCellAllowed(tt.cell); got != tt.want {
				t.Errorf("isCellAllowed(%+v) = %v, want %v", tt.cell, got, tt.want)
			}
		})
	}
}

// TestResolveAccessCell_Precedence locks the model → provider(company) → '*' →
// nil resolution order (access-policy.ts:233-266).
func TestResolveAccessCell_Precedence(t *testing.T) {
	matrix := map[string]map[string]AccessCostCell{
		"claude-sonnet": {"byok": {Allowed: false, Host: "model-row"}},
		"anthropic":     {"byok": {Allowed: true, Host: "provider-row"}, "metered": {Allowed: true, Host: "provider-metered"}},
		"*":             {"byok": {Allowed: true, Host: "wildcard-row"}, "local": {Allowed: false, Host: "wildcard-local"}},
	}

	tests := []struct {
		name      string
		model     string
		company   string
		mode      AuthMode
		wantNil   bool
		wantHost  string
		wantAllow bool
	}{
		{"model-row-wins", "claude-sonnet", "anthropic", agent.AuthBYOK, false, "model-row", false},
		{"provider-row-when-no-model-cell", "claude-sonnet", "anthropic", agent.AuthMetered, false, "provider-metered", true},
		{"provider-row-when-model-absent", "unknown-model", "anthropic", agent.AuthBYOK, false, "provider-row", true},
		{"wildcard-when-no-provider-cell", "unknown-model", "anthropic", agent.AuthLocal, false, "wildcard-local", false},
		{"empty-model-skips-model-tier", "", "anthropic", agent.AuthBYOK, false, "provider-row", true},
		{"absent-everywhere-returns-nil", "x", "unknown-company", agent.AuthShared, true, "", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := resolveAccessCell(matrix, tt.model, tt.company, tt.mode)
			if tt.wantNil {
				if got != nil {
					t.Fatalf("expected nil, got %+v", got)
				}
				return
			}
			if got == nil {
				t.Fatal("expected a cell, got nil")
			}
			if got.Host != tt.wantHost {
				t.Errorf("host: got %q, want %q", got.Host, tt.wantHost)
			}
			if got.Allowed != tt.wantAllow {
				t.Errorf("allowed: got %v, want %v", got.Allowed, tt.wantAllow)
			}
		})
	}
}

// TestResolveAccessCell_NilMatrix verifies a nil matrix resolves to nil (inherit)
// rather than panicking.
func TestResolveAccessCell_NilMatrix(t *testing.T) {
	if got := resolveAccessCell(nil, "m", "c", agent.AuthBYOK); got != nil {
		t.Errorf("nil matrix: got %+v, want nil", got)
	}
}

// TestSelectPolicy verifies workload-block selection + scope labeling, with
// fall-back to Default for empty/unknown workloads.
func TestSelectPolicy(t *testing.T) {
	cfg := &ModelAccessConfig{
		Default:   AccessPolicy{Matrix: map[string]map[string]AccessCostCell{"d": nil}},
		Workloads: map[string]AccessPolicy{"kg-extraction": {Matrix: map[string]map[string]AccessCostCell{"w": nil}}},
	}
	tests := []struct {
		name      string
		workload  string
		wantScope string
		wantKey   string
	}{
		{"empty-uses-default", "", "machine", "d"},
		{"known-workload", "kg-extraction", "machine-workload", "w"},
		{"unknown-falls-to-default", "interview", "machine", "d"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			pol, scope := selectPolicy(cfg, tt.workload)
			if scope != tt.wantScope {
				t.Errorf("scope: got %q, want %q", scope, tt.wantScope)
			}
			if _, ok := pol.Matrix[tt.wantKey]; !ok {
				t.Errorf("policy matrix missing expected key %q (got keys %v)", tt.wantKey, keysOfMatrix(pol.Matrix))
			}
		})
	}
}

// TestAccessDeniedError_NoSecrets is a defensive check that the error string
// references only cell identity (company/model/authMode/workload/scope) and
// carries no key-shaped material. Confidentiality-by-construction.
func TestAccessDeniedError_NoSecrets(t *testing.T) {
	e := &AccessDeniedError{
		Company: "anthropic", Model: "claude-sonnet", AuthMode: agent.AuthBYOK,
		Workload: "kg-extraction", Scope: "machine-workload",
	}
	msg := e.Error()
	for _, want := range []string{"anthropic", "claude-sonnet", "byok", "kg-extraction", "machine-workload"} {
		if !strings.Contains(msg, want) {
			t.Errorf("error message %q missing cell-identity token %q", msg, want)
		}
	}
	// errors.As round-trips.
	var target *AccessDeniedError
	if !errors.As(error(e), &target) {
		t.Error("AccessDeniedError does not satisfy errors.As to *AccessDeniedError")
	}
}

// TestResolveMachineCell_NilBlockIdentity is a focused unit check of the
// nil-block identity path (the standalone-daemon safe case): nil machine ⇒ no
// narrowing, requested honored when in ceiling, no pin, model passes through.
func TestResolveMachineCell_NilBlockIdentity(t *testing.T) {
	ceiling := map[AuthMode]bool{agent.AuthHostSession: true, agent.AuthBYOK: true}
	got, err := ResolveMachineCell(nil, "", "anthropic", "claude-sonnet", agent.AuthHostSession, ceiling)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.AuthMode != agent.AuthHostSession {
		t.Errorf("authMode: got %q, want host-session", got.AuthMode)
	}
	if got.Host != "" {
		t.Errorf("nil block should produce no host pin, got %q", got.Host)
	}
	if got.Model != "claude-sonnet" {
		t.Errorf("model: got %q, want claude-sonnet (pass-through)", got.Model)
	}
}

// TestResolveMachineCell_EmptyCeilingDenies confirms an empty ceiling fails
// closed regardless of machine block.
func TestResolveMachineCell_EmptyCeilingDenies(t *testing.T) {
	_, err := ResolveMachineCell(nil, "", "anthropic", "m", agent.AuthBYOK, map[AuthMode]bool{})
	var denied *AccessDeniedError
	if !errors.As(err, &denied) {
		t.Fatalf("empty ceiling must deny, got %v", err)
	}
}

func keysOfMatrix(m map[string]map[string]AccessCostCell) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
