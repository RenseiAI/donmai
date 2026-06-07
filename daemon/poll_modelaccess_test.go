package daemon

import (
	"reflect"
	"testing"

	"github.com/RenseiAI/donmai/agent"
	"github.com/RenseiAI/donmai/runner/access"
)

// TestPollItemToSessionSpec_CarriesGateInputs asserts that the P3 narrow-only
// gate inputs (ADR-2026-06-06 §5.3) are copied through onto the SessionSpec:
// PlatformAllowed/Company/Model/AuthMode off the resolved profile, WorkType +
// Mode off the top-level poll item. This is pure plumbing — the daemon copies,
// it does NOT enforce. Faithfulness of PlatformAllowed (same set the platform
// stamps) is the load-bearing assertion for the S3 gate.
func TestPollItemToSessionSpec_CarriesGateInputs(t *testing.T) {
	projects := []ProjectConfig{{ID: "alpha", Repository: "https://github.com/acme/alpha"}}

	platformAllowed := []access.AuthMode{agent.AuthHostSession, agent.AuthBYOK, agent.AuthMetered}
	item := PollWorkItem{
		SessionID:   "sess-gate",
		ProjectName: "alpha",
		Ref:         "main",
		WorkType:    "kg-extraction",
		Mode:        "interview",
		ResolvedProfile: &SessionResolvedProfile{
			Provider:        "claude",
			Model:           "claude-sonnet-4-5",
			AuthMode:        string(agent.AuthHostSession),
			Company:         string(agent.CompanyAnthropic),
			PlatformAllowed: platformAllowed,
		},
	}

	spec := PollItemToSessionSpec(item, projects)

	if spec.Company != "anthropic" {
		t.Errorf("Company = %q, want anthropic", spec.Company)
	}
	if spec.Model != "claude-sonnet-4-5" {
		t.Errorf("Model = %q, want claude-sonnet-4-5", spec.Model)
	}
	if spec.AuthMode != string(agent.AuthHostSession) {
		t.Errorf("AuthMode = %q, want host-session", spec.AuthMode)
	}
	if spec.WorkType != "kg-extraction" {
		t.Errorf("WorkType = %q, want kg-extraction", spec.WorkType)
	}
	if spec.Mode != "interview" {
		t.Errorf("Mode = %q, want interview", spec.Mode)
	}
	// PlatformAllowed carried faithfully — same set, same order the platform
	// stamped. The gate uses this as the immutable ceiling.
	if !reflect.DeepEqual(spec.PlatformAllowed, platformAllowed) {
		t.Errorf("PlatformAllowed = %v, want %v (carried faithfully)", spec.PlatformAllowed, platformAllowed)
	}
}

// TestPollItemToSessionSpec_NilProfile_NoGateInputs asserts that a poll item
// with no resolved profile leaves all profile-sourced gate inputs zero — no
// panic on the nil dereference, identity behaviour for the legacy shape.
func TestPollItemToSessionSpec_NilProfile_NoGateInputs(t *testing.T) {
	projects := []ProjectConfig{{ID: "alpha", Repository: "https://github.com/acme/alpha"}}
	item := PollWorkItem{
		SessionID:   "sess-noprof",
		ProjectName: "alpha",
		Ref:         "main",
		WorkType:    "development", // top-level fields still carry through
	}

	spec := PollItemToSessionSpec(item, projects)

	if spec.WorkType != "development" {
		t.Errorf("WorkType = %q, want development", spec.WorkType)
	}
	if spec.Company != "" || spec.Model != "" || spec.AuthMode != "" {
		t.Errorf("profile-sourced fields non-empty on nil profile: company=%q model=%q authMode=%q",
			spec.Company, spec.Model, spec.AuthMode)
	}
	if spec.PlatformAllowed != nil {
		t.Errorf("PlatformAllowed = %v, want nil on nil profile", spec.PlatformAllowed)
	}
}

// TestPollItemToSessionSpec_PreP3Identity is the ADDITIVE/IDENTITY proof: a
// SessionSpec built from a pre-P3 poll item (no gate fields, no resolved
// profile) is byte-identical for the existing fields to a SessionSpec with all
// P3 fields explicitly zeroed. Adding the P3 fields changed nothing for the
// existing-field surface.
func TestPollItemToSessionSpec_PreP3Identity(t *testing.T) {
	projects := []ProjectConfig{{ID: "alpha", Repository: "https://github.com/acme/alpha"}}
	preP3 := PollWorkItem{
		SessionID:   "sess-legacy",
		ProjectName: "alpha",
		Ref:         "main",
		Resources:   &SessionResources{VCpu: 2, MemoryMB: 4096},
		Env:         map[string]string{"FOO": "bar"},
		MaxDuration: 1800,
	}

	got := PollItemToSessionSpec(preP3, projects)

	// The exact spec a pre-P3 daemon would have produced: existing fields
	// populated, every P3 gate field at its zero value.
	want := SessionSpec{
		SessionID:          "sess-legacy",
		Repository:         "https://github.com/acme/alpha",
		Ref:                "main",
		Resources:          &SessionResources{VCpu: 2, MemoryMB: 4096},
		Env:                map[string]string{"FOO": "bar"},
		MaxDurationSeconds: 1800,
		ProjectName:        "alpha",
		// PlatformAllowed/AuthMode/WorkType/Mode/Company/Model/Workload all zero.
	}

	if !reflect.DeepEqual(got, want) {
		t.Errorf("pre-P3 SessionSpec not identity:\n got = %+v\nwant = %+v", got, want)
	}
	// Belt-and-suspenders on the P3 fields specifically.
	if got.PlatformAllowed != nil || got.AuthMode != "" || got.WorkType != "" ||
		got.Mode != "" || got.Company != "" || got.Model != "" || got.Workload != "" {
		t.Errorf("pre-P3 item produced non-zero P3 gate fields: %+v", got)
	}
}
