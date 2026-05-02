package runner

// Completion contracts.
//
// Typed declaration of what each work type must produce before a session
// is considered successful. Verbatim port of the per-workType contract
// from agentfactory/packages/core/src/orchestrator/completion-contracts.ts
// — see that file for the design rationale.
//
// The runner consults these contracts in two places:
//
//  1. Session-end backstop (runner/backstop.go) — backstop-capable
//     fields (branch_pushed, pr_url) trigger deterministic recovery.
//  2. WORK_RESULT diagnostic (runner/loop.go) — when development /
//     coordination / inflight / inflight-coordination work ends without
//     a WORK_RESULT marker, the runner posts the diagnostic comment from
//     sdlc.go::diagnosticCommentBody.
//
// REN-1467 (this wave): WORK_RESULT is added to the development /
// coordination / inflight / inflight-coordination required fields so the
// post-session decision tree (sdlc.go::resolveTargetStatus) has the
// signal it needs to decide promote vs fail.

// CompletionFieldType is the discriminator for the field set every
// contract enumerates. Verbatim mirror of CompletionFieldType from the
// legacy TS file.
type CompletionFieldType string

// Field-type constants. Stable wire strings — never repurpose an
// existing one.
const (
	FieldPRURL              CompletionFieldType = "pr_url"
	FieldBranchPushed       CompletionFieldType = "branch_pushed"
	FieldCommitsPresent     CompletionFieldType = "commits_present"
	FieldWorkResult         CompletionFieldType = "work_result"
	FieldIssueUpdated       CompletionFieldType = "issue_updated"
	FieldCommentPosted      CompletionFieldType = "comment_posted"
	FieldSubIssuesCreated   CompletionFieldType = "sub_issues_created"
	FieldPRMerged           CompletionFieldType = "pr_merged"
	FieldPRMergedOrEnqueued CompletionFieldType = "pr_merged_or_enqueued"
)

// CompletionField is a single required or optional field in a
// completion contract. BackstopCapable=true means the runner can fill
// the field deterministically post-session (e.g. push a branch). Fields
// that require agent judgement (work_result) are NOT backstop-capable.
type CompletionField struct {
	Type            CompletionFieldType
	Label           string
	BackstopCapable bool
}

// CompletionContract enumerates the required + optional fields a
// session of the given work type must produce.
type CompletionContract struct {
	WorkType string
	Required []CompletionField
	Optional []CompletionField
}

// Shared field definitions (DRY) — mirror the FIELD object at
// completion-contracts.ts:126.
var (
	fieldPRURL = CompletionField{
		Type:            FieldPRURL,
		Label:           "Pull request URL",
		BackstopCapable: true,
	}
	fieldBranchPushed = CompletionField{
		Type:            FieldBranchPushed,
		Label:           "Branch pushed to remote",
		BackstopCapable: true,
	}
	fieldCommitsPresent = CompletionField{
		Type:            FieldCommitsPresent,
		Label:           "Commits on branch",
		BackstopCapable: true,
	}
	fieldWorkResult = CompletionField{
		Type:            FieldWorkResult,
		Label:           "Structured work result (passed/failed)",
		BackstopCapable: false,
	}
	fieldIssueUpdated = CompletionField{
		Type:            FieldIssueUpdated,
		Label:           "Issue description updated",
		BackstopCapable: false,
	}
	fieldCommentPosted = CompletionField{
		Type:            FieldCommentPosted,
		Label:           "Comment posted to issue",
		BackstopCapable: false,
	}
	fieldSubIssuesCreated = CompletionField{
		Type:            FieldSubIssuesCreated,
		Label:           "Sub-issues created",
		BackstopCapable: false,
	}
	fieldPRMerged = CompletionField{
		Type:            FieldPRMerged,
		Label:           "Pull request merged",
		BackstopCapable: false,
	}
	fieldPRMergedOrEnqueued = CompletionField{
		Type:            FieldPRMergedOrEnqueued,
		Label:           "Pull request merged or enqueued for merge",
		BackstopCapable: false,
	}
)

// completionContracts is the per-workType contract table.
//
// REN-1467 difference vs the legacy TS table: development / inflight /
// coordination / inflight-coordination now include FieldWorkResult so
// the post-session decision tree has a parseable signal for
// promote-vs-fail. The legacy TS table is being updated in lockstep
// (umbrella REN-1466).
var completionContracts = map[string]CompletionContract{
	WorkTypeDevelopmentStr: {
		WorkType: WorkTypeDevelopmentStr,
		Required: []CompletionField{fieldCommitsPresent, fieldBranchPushed, fieldPRURL, fieldWorkResult},
	},
	WorkTypeInflight: {
		WorkType: WorkTypeInflight,
		Required: []CompletionField{fieldCommitsPresent, fieldBranchPushed, fieldPRURL, fieldWorkResult},
	},
	WorkTypeCoordination: {
		WorkType: WorkTypeCoordination,
		Required: []CompletionField{fieldWorkResult, fieldCommentPosted},
	},
	WorkTypeInflightCoordination: {
		WorkType: WorkTypeInflightCoordination,
		Required: []CompletionField{fieldWorkResult, fieldCommentPosted},
	},
	WorkTypeQAStr: {
		WorkType: WorkTypeQAStr,
		Required: []CompletionField{fieldWorkResult, fieldCommentPosted},
	},
	WorkTypeAcceptance: {
		WorkType: WorkTypeAcceptance,
		Required: []CompletionField{fieldWorkResult, fieldPRMergedOrEnqueued},
	},
	WorkTypeRefinement: {
		WorkType: WorkTypeRefinement,
		Required: []CompletionField{fieldCommentPosted},
		Optional: []CompletionField{fieldIssueUpdated},
	},
	WorkTypeRefinementCoordination: {
		WorkType: WorkTypeRefinementCoordination,
		Required: []CompletionField{fieldCommentPosted},
	},
	WorkTypeResearch: {
		WorkType: WorkTypeResearch,
		Required: []CompletionField{fieldIssueUpdated},
		Optional: []CompletionField{fieldCommentPosted},
	},
	WorkTypeBacklogCreation: {
		WorkType: WorkTypeBacklogCreation,
		Required: []CompletionField{fieldSubIssuesCreated},
		Optional: []CompletionField{fieldCommentPosted},
	},
	WorkTypeBacklogGroomer: {
		WorkType: WorkTypeBacklogGroomer,
		Required: []CompletionField{fieldCommentPosted},
		Optional: []CompletionField{fieldIssueUpdated},
	},
	WorkTypeMerge: {
		WorkType: WorkTypeMerge,
		Required: []CompletionField{fieldPRMerged},
	},
	WorkTypeDocumentationSteward: {
		WorkType: WorkTypeDocumentationSteward,
		Required: []CompletionField{fieldCommentPosted},
		Optional: []CompletionField{fieldIssueUpdated},
	},
}

// GetCompletionContract returns the completion contract for a work
// type. Returns the contract and true on hit; returns the zero
// CompletionContract and false for unknown work types (caller treats as
// no contract — best-effort completion).
func GetCompletionContract(workType string) (CompletionContract, bool) {
	c, ok := completionContracts[workType]
	return c, ok
}

// RequiresWorkResult reports whether the contract for this work type
// includes FieldWorkResult as a required field. Drives the
// diagnostic-comment branch in the post-session block when the agent
// exits without emitting a WORK_RESULT marker.
func RequiresWorkResult(workType string) bool {
	c, ok := completionContracts[workType]
	if !ok {
		return false
	}
	for _, f := range c.Required {
		if f.Type == FieldWorkResult {
			return true
		}
	}
	return false
}
