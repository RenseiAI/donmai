package runner

import "slices"

// WorkTypeRole is the coarse behavioural bucket a work-type name belongs
// to. The platform stamps the role next to the name on every work item so
// the runner can serve organisation-defined names it was never compiled
// with: the name stays the display/logging identity while every behaviour
// decision consults the role (completion contracts, prompt selection,
// routing weights).
//
// Stable string constants; never repurpose an existing value.
type WorkTypeRole string

const (
	// WorkTypeRoleInflight marks work that produces code on a branch and
	// opens a pull request (development, inflight and their custom kin).
	WorkTypeRoleInflight WorkTypeRole = "inflight"
	// WorkTypeRoleReview marks work that renders a review verdict and no
	// pull request (qa, acceptance and their custom kin).
	WorkTypeRoleReview WorkTypeRole = "review"
	// WorkTypeRoleIntake marks work whose deliverable is the completed
	// investigation or triage itself (research, backlog-creation and
	// their custom kin).
	WorkTypeRoleIntake WorkTypeRole = "intake"
	// WorkTypeRoleTerminal marks work with no completion contract — the
	// session's terminal state is the whole outcome.
	WorkTypeRoleTerminal WorkTypeRole = "terminal"
)

// ParseWorkTypeRole validates a raw role string from the wire. An empty
// or unrecognised value reports absent (false) so callers fall back to
// the name-keyed legacy behaviour; the role never invents a contract a
// built-in name did not already carry.
func ParseWorkTypeRole(s string) (WorkTypeRole, bool) {
	switch WorkTypeRole(s) {
	case WorkTypeRoleInflight, WorkTypeRoleReview, WorkTypeRoleIntake, WorkTypeRoleTerminal:
		return WorkTypeRole(s), true
	default:
		return "", false
	}
}

// unrecognizedWorkTypeRole reports whether role is a non-empty value outside
// the role vocabulary for an unknown work name. Callers log the skew so
// operators can see a producer and runner disagreeing on the role set; the
// decision itself stays least-privilege (no contract, identical to pre-role
// handling of unknown names). Empty roles are the normal absent-field case
// and known names keep their legacy table whatever the role claims, so both
// stay silent. Factored so the observability branch is itself testable.
func unrecognizedWorkTypeRole(workType, role string) bool {
	if role == "" || IsKnownWorkType(workType) {
		return false
	}
	_, valid := ParseWorkTypeRole(role)
	return !valid
}

func contractForRole(role WorkTypeRole) (CompletionContract, bool) {
	switch role {
	case WorkTypeRoleInflight:
		c := completionContracts[WorkTypeDevelopmentStr]
		c.Required = slices.Clone(c.Required)
		return c, true
	case WorkTypeRoleReview:
		return CompletionContract{Required: []CompletionField{fieldWorkResult}}, true
	case WorkTypeRoleIntake:
		return CompletionContract{}, true
	default:
		return CompletionContract{}, false
	}
}
