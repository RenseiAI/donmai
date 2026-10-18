package runner

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

func contractForRole(role WorkTypeRole) (CompletionContract, bool) {
	switch role {
	case WorkTypeRoleInflight:
		return completionContracts[WorkTypeDevelopmentStr], true
	case WorkTypeRoleReview:
		return CompletionContract{Required: []CompletionField{fieldWorkResult}}, true
	case WorkTypeRoleIntake:
		return CompletionContract{}, true
	default:
		return CompletionContract{}, false
	}
}
