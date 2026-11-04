package runner

import (
	"github.com/RenseiAI/donmai/stagewire"
)

// contractSource names where a session's completion contract came from.
type contractSource string

const (
	// contractFromCard: the dispatched card's own contract.
	contractFromCard contractSource = "card"
	// contractFromStageRole: the default contract of the stage's run role.
	contractFromStageRole contractSource = "stage-role"
	// contractFromWorkTypeRole: the default contract of the legacy
	// workTypeRole, on a work item that carries a stage or a card but no
	// card contract and no stage role.
	contractFromWorkTypeRole contractSource = "work-type-role"
	// contractFromNone: a stage or card is present and nothing above
	// applies; the run owes nothing and names are never consulted.
	contractFromNone contractSource = "none"
	// contractFromLegacyName: the LEGACY name-keyed tables (contracts.go,
	// sdlc.go). Used only when the work item carries neither a stage nor a
	// card, for older control planes and standalone use.
	contractFromLegacyName contractSource = "legacy-name"
)

// sessionContract is what a run owes, as the runner's behaviour reads it:
// a pull request (with its commits and pushed branch), a review verdict, a
// structured work result.
type sessionContract struct {
	source     contractSource
	pr         bool
	verdict    bool
	workResult bool
}

// completionContract resolves the session's completion contract. Order:
//
//  1. the card's contract (card.contract);
//  2. the default contract of the stage's run role (stage.runRole, else
//     stage.role);
//  3. on a 1.0 work item (stage or card present), the default contract of
//     the legacy workTypeRole, else nothing owed;
//  4. otherwise the LEGACY name-keyed resolution, byte-for-byte as before
//     the 1.0 wire: the per-name table for built-in names, the legacy
//     workTypeRole for any other name.
func (q QueuedWork) completionContract() sessionContract {
	if q.Card != nil && q.Card.Contract != nil {
		return contractOf(contractFromCard, *q.Card.Contract)
	}
	if role, ok := q.StageRunRole(); ok {
		return contractOf(contractFromStageRole, role.DefaultContract())
	}
	if q.HasStageOrCard() {
		if role, ok := stageRoleForWorkTypeRole(q.WorkTypeRole); ok {
			return contractOf(contractFromWorkTypeRole, role.DefaultContract())
		}
		return sessionContract{source: contractFromNone}
	}
	return legacyNameContract(q.WorkType, q.WorkTypeRole)
}

// legacyNameContract is the LEGACY name-keyed contract resolution, kept for
// work items without a stage or a card. It is deleted once every dispatch
// carries a card.
func legacyNameContract(workType, role string) sessionContract {
	return sessionContract{
		source:     contractFromLegacyName,
		pr:         RequiresPRURL(workType, role),
		verdict:    RequiresReviewVerdict(workType, role),
		workResult: RequiresWorkResult(workType, role),
	}
}

func contractOf(source contractSource, c stagewire.Contract) sessionContract {
	return sessionContract{source: source, pr: c.PR, verdict: c.Verdict, workResult: c.WorkResult}
}

// stageRoleForWorkTypeRole maps the four-role legacy vocabulary onto the
// seven stage roles: terminal becomes done (nothing owed); the other three
// keep their name.
func stageRoleForWorkTypeRole(raw string) (stagewire.Role, bool) {
	role, ok := ParseWorkTypeRole(raw)
	if !ok {
		return "", false
	}
	if role == WorkTypeRoleTerminal {
		return stagewire.RoleDone, true
	}
	return stagewire.ParseRole(string(role))
}

// requiresPR reports whether the session owes a pull request. Every
// publication path (steering, backstop, pull request verification,
// checkpoints, nudges) gates on it.
func (q QueuedWork) requiresPR() bool { return q.completionContract().pr }

// requiresReviewVerdict reports whether the session owes a review verdict.
func (q QueuedWork) requiresReviewVerdict() bool { return q.completionContract().verdict }
