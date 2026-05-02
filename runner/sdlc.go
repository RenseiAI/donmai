package runner

// SDLC status mappings + post-session transition decision.
//
// This file ports the Linear status-mapping tables from the legacy TS
// orchestrator (agentfactory/packages/linear/src/types.ts —
// WORK_TYPE_COMPLETE_STATUS / WORK_TYPE_FAIL_STATUS) and the
// post-session decision logic from
// packages/core/src/orchestrator/event-processor.ts:300-450.
//
// The runner consults these tables after the agent's terminal event to
// decide whether (and to what status) the Linear issue should be
// transitioned. Mirrors the TS behaviour:
//
//   - passed → workTypeCompleteStatus[workType]
//   - failed → workTypeFailStatus[workType]
//   - unknown → no transition; post a diagnostic comment instead
//   - non-result-sensitive types (research, backlog-creation, refinement, etc.)
//     promote on completion regardless of marker
//   - acceptance with a passing marker DEFERS to the merge worker when a
//     local merge queue is configured (REN-503/REN-1153)
//
// Wire shape: targetStatus is the Linear workflow-state name (e.g.
// "Finished", "Rejected", "Backlog"). The runtime/result layer resolves
// the name to a Linear stateId via the platform's issue-tracker proxy.

// Known agent work-type names. Verbatim mirror of AgentWorkType from
// agentfactory/packages/core/src/orchestrator/work-types.ts so any
// new work type the platform adds shows up here as a missing entry on
// `make test` (TestWorkTypeStatusMappings_Exhaustive).
//
// Stable string constants; never repurpose an existing value.
const (
	WorkTypeResearch                 = "research"
	WorkTypeBacklogCreation          = "backlog-creation"
	WorkTypeBacklogGroomer           = "backlog-groomer"
	WorkTypeDevelopmentStr           = "development"
	WorkTypeInflight                 = "inflight"
	WorkTypeQAStr                    = "qa"
	WorkTypeAcceptance               = "acceptance"
	WorkTypeRefinement               = "refinement"
	WorkTypeRefinementCoordination   = "refinement-coordination"
	WorkTypeMerge                    = "merge"
	WorkTypeSecurity                 = "security"
	WorkTypeImprovementLoop          = "improvement-loop"
	WorkTypeOutcomeAuditor           = "outcome-auditor"
	WorkTypeGAReadiness              = "ga-readiness"
	WorkTypeDocumentationSteward     = "documentation-steward"
	WorkTypeOperationalScannerVercel = "operational-scanner-vercel"
	WorkTypeOperationalScannerAudit  = "operational-scanner-audit"
	WorkTypeOperationalScannerCI     = "operational-scanner-ci"
	WorkTypeCoordination             = "coordination"
	WorkTypeInflightCoordination     = "inflight-coordination"
)

// AllWorkTypes lists every recognised work type. Used by exhaustive
// mapping tests (every entry must appear in workTypeCompleteStatus and
// workTypeFailStatus, even when the value is empty). Order matches the
// declaration above; tests do not depend on order.
var AllWorkTypes = []string{
	WorkTypeResearch,
	WorkTypeBacklogCreation,
	WorkTypeBacklogGroomer,
	WorkTypeDevelopmentStr,
	WorkTypeInflight,
	WorkTypeQAStr,
	WorkTypeAcceptance,
	WorkTypeRefinement,
	WorkTypeRefinementCoordination,
	WorkTypeMerge,
	WorkTypeSecurity,
	WorkTypeImprovementLoop,
	WorkTypeOutcomeAuditor,
	WorkTypeGAReadiness,
	WorkTypeDocumentationSteward,
	WorkTypeOperationalScannerVercel,
	WorkTypeOperationalScannerAudit,
	WorkTypeOperationalScannerCI,
	WorkTypeCoordination,
	WorkTypeInflightCoordination,
}

// workTypeCompleteStatus maps a work type to the Linear status the
// runner should transition the issue to on a passing/clean completion.
// Empty string ("") means no auto-transition.
//
// Verbatim port of WORK_TYPE_COMPLETE_STATUS from
// agentfactory/packages/linear/src/types.ts.
var workTypeCompleteStatus = map[string]string{
	WorkTypeResearch:                 "",          // No auto-transition; user moves to Backlog
	WorkTypeBacklogCreation:          "",          // Source stays in Icebox
	WorkTypeBacklogGroomer:           "",          // Groomer labels; scheduler drives next pass
	WorkTypeDevelopmentStr:           "Finished",  // Started -> Finished when work done
	WorkTypeInflight:                 "Finished",  // Started -> Finished when work done
	WorkTypeQAStr:                    "Delivered", // Finished -> Delivered on QA pass
	WorkTypeAcceptance:               "Accepted",  // Delivered -> Accepted on acceptance pass
	WorkTypeRefinement:               "Backlog",   // Rejected -> Backlog after refinement
	WorkTypeRefinementCoordination:   "Backlog",   // Rejected -> Backlog after coordinated refinement
	WorkTypeMerge:                    "",          // Merge completion handled by merge queue
	WorkTypeSecurity:                 "Finished",  // Security scan complete
	WorkTypeImprovementLoop:          "",          // PM Agent: no auto-transition
	WorkTypeOutcomeAuditor:           "",          // Tags issues with audit:* labels instead
	WorkTypeGAReadiness:              "",          // Posts report comment, authors blockers
	WorkTypeDocumentationSteward:     "",          // Posts scan summary comment
	WorkTypeOperationalScannerVercel: "",          // Authors bug-report issues
	WorkTypeOperationalScannerAudit:  "",          // Authors anomaly issues
	WorkTypeOperationalScannerCI:     "",          // Authors flaky/slow-test issues
	// Coordination work types — mirror development/inflight: when the
	// coordinator's children all land they roll up to Finished. Not
	// present in the legacy TS table because coordination is dispatched
	// as 'development' there; reserved here for parity with the wave-6
	// dispatch wire shape.
	WorkTypeCoordination:         "Finished",
	WorkTypeInflightCoordination: "Finished",
}

// workTypeFailStatus maps a work type to the Linear status the runner
// should transition the issue to when the agent reports failure.
// Empty string ("") means no auto-transition (stay in current status).
//
// Verbatim port of WORK_TYPE_FAIL_STATUS from
// agentfactory/packages/linear/src/types.ts.
var workTypeFailStatus = map[string]string{
	WorkTypeResearch:                 "",
	WorkTypeBacklogCreation:          "",
	WorkTypeBacklogGroomer:           "",
	WorkTypeDevelopmentStr:           "",
	WorkTypeInflight:                 "",
	WorkTypeQAStr:                    "Rejected", // QA fail -> Rejected (refinement picks up)
	WorkTypeAcceptance:               "Rejected", // Acceptance fail -> Rejected (rejection handler)
	WorkTypeRefinement:               "",
	WorkTypeRefinementCoordination:   "",
	WorkTypeMerge:                    "",
	WorkTypeSecurity:                 "",
	WorkTypeImprovementLoop:          "",
	WorkTypeOutcomeAuditor:           "",
	WorkTypeGAReadiness:              "",
	WorkTypeDocumentationSteward:     "",
	WorkTypeOperationalScannerVercel: "",
	WorkTypeOperationalScannerAudit:  "",
	WorkTypeOperationalScannerCI:     "",
	WorkTypeCoordination:             "",
	WorkTypeInflightCoordination:     "",
}

// resultSensitiveWorkTypes is the set of work types whose post-session
// status transition depends on a parsed WORK_RESULT marker. Mirrors the
// `isResultSensitive` predicate at event-processor.ts:335.
//
// Result-sensitive types:
//   - QA / acceptance — the agent decides pass/fail
//   - development / inflight / coordination — the agent rolls up the
//     subtree's pass/fail
//   - merge — the merge worker decides land/abort
//
// Non-result-sensitive types promote on completion regardless of marker
// (research, backlog-creation, refinement, etc.).
var resultSensitiveWorkTypes = map[string]bool{
	WorkTypeQAStr:                true,
	WorkTypeAcceptance:           true,
	WorkTypeDevelopmentStr:       true,
	WorkTypeInflight:             true,
	WorkTypeMerge:                true,
	WorkTypeCoordination:         true,
	WorkTypeInflightCoordination: true,
}

// isResultSensitive reports whether a work type's auto-transition is
// gated on the WORK_RESULT marker. Result-sensitive types fall through
// to the diagnostic-comment path when no marker is present; non-
// result-sensitive types promote on completion.
func isResultSensitive(workType string) bool {
	return resultSensitiveWorkTypes[workType]
}

// shouldDeferAcceptanceTransition decides whether a passing acceptance
// session should DEFER its Delivered → Accepted promotion to the merge
// worker. Verbatim port of `shouldDeferAcceptanceTransition` from
// agentfactory/packages/core/src/orchestrator/dispatcher.ts:74.
//
// Returns false when:
//   - no merge queue adapter is configured (acceptance merges directly)
//   - work type is not acceptance
//
// Today the Go runner does not yet ship a merge-queue adapter; callers
// pass `hasMergeQueueAdapter=false` so this gate is a no-op until the
// adapter lands. The function exists for parity so the wire-up is
// straightforward when the adapter arrives.
func shouldDeferAcceptanceTransition(workType string, hasMergeQueueAdapter bool) bool {
	if !hasMergeQueueAdapter {
		return false
	}
	return workType == WorkTypeAcceptance
}

// PostSessionDecision is the typed outcome of resolveTargetStatus.
// Callers use it to drive the actual side effects:
//
//   - TargetStatus non-empty + ShouldTransition true: call
//     UpdateIssueStatus(issueID, TargetStatus).
//   - PostDiagnostic true: post the "missing WORK_RESULT" comment to
//     Linear (do NOT transition).
//   - Deferred true: log "deferred to merge queue" and skip transition.
//
// Reason is a free-form short identifier surfaced in logs ("passed",
// "failed", "unknown", "agent-failed", "completed-non-sensitive",
// "deferred-merge-queue", "no-mapping").
type PostSessionDecision struct {
	WorkType         string
	WorkResult       string // "passed" | "failed" | "unknown" | ""
	TargetStatus     string // Linear workflow-state name, e.g. "Finished"
	ShouldTransition bool
	PostDiagnostic   bool
	Deferred         bool
	Reason           string
}

// resolveTargetStatus computes the post-session transition decision
// from the session outputs. Verbatim port of the decision tree at
// event-processor.ts:332-413.
//
// Inputs:
//   - workType — the agent work type (development, qa, acceptance, ...)
//   - sessionStatus — the runner's classification of the session result
//     ("completed" | "failed"). "failed" maps to a synthetic
//     workResult="failed" for result-sensitive types per the TS path
//     (event-processor.ts:340-344).
//   - workResult — parsed marker from the agent's output: "passed",
//     "failed", or "unknown".
//   - hasMergeQueueAdapter — true when the runner has a merge-queue
//     adapter configured (today: always false in Go).
//
// Returns a PostSessionDecision the caller acts on. Pure function — no
// side effects.
func resolveTargetStatus(workType, sessionStatus, workResult string, hasMergeQueueAdapter bool) PostSessionDecision {
	d := PostSessionDecision{
		WorkType:   workType,
		WorkResult: workResult,
	}

	// Empty/unknown work type — log and skip. Callers won't trigger
	// this in practice (the platform always sets a workType) but the
	// safety net keeps a misconfigured QueuedWork from crashing the
	// post-session path.
	if workType == "" {
		d.Reason = "no-work-type"
		return d
	}

	resultSensitive := isResultSensitive(workType)

	if resultSensitive {
		// Per event-processor.ts:340-344: a session-level "failed"
		// classification (crash, error, timeout) on a result-sensitive
		// work type is treated as a fail marker so the issue still
		// transitions to the fail status (Rejected for QA/acceptance).
		effectiveResult := workResult
		if sessionStatus == "failed" {
			effectiveResult = "failed"
		}

		switch effectiveResult {
		case "passed":
			// REN-503/REN-1153: passing acceptance defers to the
			// merge worker when the local queue is enabled.
			if shouldDeferAcceptanceTransition(workType, hasMergeQueueAdapter) {
				d.Deferred = true
				d.Reason = "deferred-merge-queue"
				return d
			}
			target := workTypeCompleteStatus[workType]
			if target == "" {
				d.Reason = "no-mapping"
				return d
			}
			d.TargetStatus = target
			d.ShouldTransition = true
			d.Reason = "passed"
		case "failed":
			target := workTypeFailStatus[workType]
			if target == "" {
				d.Reason = "no-mapping"
				return d
			}
			d.TargetStatus = target
			d.ShouldTransition = true
			if sessionStatus == "failed" && workResult != "failed" {
				d.Reason = "agent-failed"
			} else {
				d.Reason = "failed"
			}
		default:
			// "unknown" or empty — safe default per
			// event-processor.ts:382-406: don't transition; post a
			// diagnostic comment instead so the issue doesn't stall
			// silently.
			d.PostDiagnostic = true
			d.Reason = "unknown"
		}
		return d
	}

	// Non-result-sensitive types: promote on successful completion
	// regardless of marker (event-processor.ts:409-413).
	if sessionStatus == "completed" {
		target := workTypeCompleteStatus[workType]
		if target == "" {
			d.Reason = "no-mapping"
			return d
		}
		d.TargetStatus = target
		d.ShouldTransition = true
		d.Reason = "completed-non-sensitive"
		return d
	}

	// Non-sensitive type that failed — no transition (the TS path also
	// no-ops here; failure on these work types is reflected in the
	// session status only, not Linear state).
	d.Reason = "non-sensitive-failed"
	return d
}

// diagnosticCommentBody builds the comment posted to Linear when a
// result-sensitive session ends without a parseable WORK_RESULT marker.
// Mirrors the legacy TS comment body at event-processor.ts:393-399 so
// operators see the same message regardless of which orchestrator ran.
func diagnosticCommentBody() string {
	return "WARNING: Agent completed but no structured result marker was detected in the output.\n\n" +
		"**Issue status was NOT updated automatically.**\n\n" +
		"The orchestrator expected one of:\n" +
		"- `WORK_RESULT:passed` to promote the issue\n" +
		"- `WORK_RESULT:failed` to record a failure\n\n" +
		"This usually means the agent exited early (timeout, error, or missing logic). " +
		"Check the agent logs for details, then manually update the issue status or re-trigger the agent."
}
