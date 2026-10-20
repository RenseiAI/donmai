package runner

import "slices"

// WorkTypeRole describes completion obligations for unrecognised work names.
type WorkTypeRole string

// Supported completion roles on the dispatch wire.
const (
	WorkTypeRoleIntake   WorkTypeRole = "intake"
	WorkTypeRoleInflight WorkTypeRole = "inflight"
	WorkTypeRoleReview   WorkTypeRole = "review"
	WorkTypeRoleTerminal WorkTypeRole = "terminal"
)

// ParseWorkTypeRole validates at use, preserving the original wire string for hashing.
func ParseWorkTypeRole(value string) (WorkTypeRole, bool) {
	switch role := WorkTypeRole(value); role {
	case WorkTypeRoleIntake, WorkTypeRoleInflight, WorkTypeRoleReview, WorkTypeRoleTerminal:
		return role, true
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
