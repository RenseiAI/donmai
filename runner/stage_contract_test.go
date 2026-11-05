package runner

import (
	"testing"

	"github.com/RenseiAI/donmai/prompt"
	"github.com/RenseiAI/donmai/stagewire"
)

// builtInGolden is today's behaviour for every built-in work-type name,
// written out literally: the completion obligations the runner acts on and
// the legacy status moves. It is the reference both 1.0 paths are held to.
// Moving any built-in in contracts.go or sdlc.go turns
// TestStageWireBuiltInsUnchanged red.
type builtInGolden struct {
	pr, verdict, workResult bool
	resultSensitive         bool
	completeStatus          string
	failStatus              string
}

var builtInGoldens = map[string]builtInGolden{
	WorkTypeResearch:                 {},
	WorkTypeBacklogCreation:          {},
	WorkTypeBacklogGroomer:           {},
	WorkTypeDevelopmentStr:           {pr: true, workResult: true, resultSensitive: true, completeStatus: "Finished"},
	WorkTypeInflight:                 {pr: true, workResult: true, resultSensitive: true, completeStatus: "Finished"},
	WorkTypeQAStr:                    {verdict: true, workResult: true, resultSensitive: true, completeStatus: "Delivered", failStatus: "Rejected"},
	WorkTypeAcceptance:               {verdict: true, workResult: true, resultSensitive: true, completeStatus: "Accepted", failStatus: "Rejected"},
	WorkTypeRefinement:               {completeStatus: "Backlog"},
	WorkTypeRefinementCoordination:   {completeStatus: "Backlog"},
	WorkTypeMerge:                    {resultSensitive: true},
	WorkTypeSecurity:                 {completeStatus: "Finished"},
	WorkTypeImprovementLoop:          {},
	WorkTypeOutcomeAuditor:           {},
	WorkTypeGAReadiness:              {},
	WorkTypeDocumentationSteward:     {},
	WorkTypeOperationalScannerVercel: {},
	WorkTypeOperationalScannerAudit:  {},
	WorkTypeOperationalScannerCI:     {},
	WorkTypeCoordination:             {workResult: true, resultSensitive: true, completeStatus: "Finished"},
	WorkTypeInflightCoordination:     {workResult: true, resultSensitive: true, completeStatus: "Finished"},
}

func contractFlags(c sessionContract) [3]bool { return [3]bool{c.pr, c.verdict, c.workResult} }

// TestStageWireBuiltInsUnchanged pins, for every name in AllWorkTypes, that
// the runner behaves as before the 1.0 wire: with the new fields absent
// (whatever legacy role rides along), and with a card whose contract equals
// today's table (with or without a stage whose role disagrees). The legacy
// status tables are checked against the same literal reference.
func TestStageWireBuiltInsUnchanged(t *testing.T) {
	t.Parallel()
	if len(builtInGoldens) != len(AllWorkTypes) {
		t.Fatalf("golden table covers %d names; AllWorkTypes has %d", len(builtInGoldens), len(AllWorkTypes))
	}
	for _, name := range AllWorkTypes {
		golden, ok := builtInGoldens[name]
		if !ok {
			t.Fatalf("built-in %q has no golden row", name)
		}
		want := [3]bool{golden.pr, golden.verdict, golden.workResult}
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			// New fields absent: the legacy name table, whatever role.
			for _, role := range []string{"", "inflight", "review", "intake", "terminal", "future"} {
				qw := QueuedWork{QueuedWork: prompt.QueuedWork{WorkType: name, WorkTypeRole: role}}
				got := qw.completionContract()
				if contractFlags(got) != want || got.source != contractFromLegacyName {
					t.Fatalf("absent fields, role %q: contract %+v; want %v from the legacy table", role, got, want)
				}
				if qw.requiresPR() != golden.pr || qw.requiresReviewVerdict() != golden.verdict {
					t.Fatalf("absent fields, role %q: publication/verdict gates moved", role)
				}
			}
			if workTypeCompleteStatus[name] != golden.completeStatus || workTypeFailStatus[name] != golden.failStatus ||
				isResultSensitive(name) != golden.resultSensitive {
				t.Fatalf("legacy status move moved: complete %q fail %q sensitive %v",
					workTypeCompleteStatus[name], workTypeFailStatus[name], isResultSensitive(name))
			}
			// A card contract equal to today's table: same behaviour,
			// whatever stage role rides along.
			card := &stagewire.Card{ID: "card-" + name, Handle: name, Contract: &stagewire.Contract{
				PR: golden.pr, Verdict: golden.verdict, WorkResult: golden.workResult,
			}}
			for _, stage := range []*stagewire.Stage{nil, {Key: "k", Role: stagewire.RoleReview}, {Key: "k", Role: stagewire.RoleIntake, RunRole: stagewire.RoleInflight}} {
				qw := QueuedWork{QueuedWork: prompt.QueuedWork{WorkType: name, Fields: stagewire.Fields{Stage: stage, Card: card}}}
				got := qw.completionContract()
				if contractFlags(got) != want || got.source != contractFromCard {
					t.Fatalf("card contract, stage %+v: contract %+v; want %v from the card", stage, got, want)
				}
			}
		})
	}
}

// TestStageWireContractPrecedence pins the 1.0 resolution order: card
// contract, then the stage run role's default, then (on a 1.0 work item)
// the legacy workTypeRole's default; the name table only for a work item
// with neither a stage nor a card.
func TestStageWireContractPrecedence(t *testing.T) {
	t.Parallel()
	inflight := [3]bool{true, false, true}
	review := [3]bool{false, true, true}
	intake := [3]bool{false, false, true}
	nothing := [3]bool{}
	for _, tc := range []struct {
		name   string
		qw     prompt.QueuedWork
		want   [3]bool
		source contractSource
	}{
		{
			name: "card contract beats the stage role",
			qw: prompt.QueuedWork{WorkType: "qa", Fields: stagewire.Fields{
				Stage: &stagewire.Stage{Key: "verify", Role: stagewire.RoleReview},
				Card:  &stagewire.Card{ID: "c", Contract: &stagewire.Contract{PR: true}},
			}},
			want: [3]bool{true, false, false}, source: contractFromCard,
		},
		{
			name: "run role beats the dispatch role: intake dispatch running inflight owes a pull request",
			qw: prompt.QueuedWork{WorkType: "research", Fields: stagewire.Fields{
				Stage: &stagewire.Stage{Key: "backlog", Role: stagewire.RoleIntake, RunRole: stagewire.RoleInflight},
			}},
			want: inflight, source: contractFromStageRole,
		},
		{
			name: "dispatch role when the run role is absent",
			qw: prompt.QueuedWork{WorkType: "development", Fields: stagewire.Fields{
				Stage: &stagewire.Stage{Key: "backlog", Role: stagewire.RoleIntake},
			}},
			want: intake, source: contractFromStageRole,
		},
		{
			name: "card without a contract falls back to the stage role",
			qw: prompt.QueuedWork{WorkType: "development", Fields: stagewire.Fields{
				Stage: &stagewire.Stage{Key: "verify", Role: stagewire.RoleReview},
				Card:  &stagewire.Card{ID: "c"},
			}},
			want: review, source: contractFromStageRole,
		},
		{
			name: "stage role beats the legacy role and the name",
			qw: prompt.QueuedWork{WorkType: "qa", WorkTypeRole: "review", Fields: stagewire.Fields{
				Stage: &stagewire.Stage{Key: "rework", Role: stagewire.RoleRework},
			}},
			want: inflight, source: contractFromStageRole,
		},
		{
			name: "card without stage uses the legacy role, never the name",
			qw: prompt.QueuedWork{WorkType: "qa", WorkTypeRole: "inflight", Fields: stagewire.Fields{
				Card: &stagewire.Card{ID: "c"},
			}},
			want: inflight, source: contractFromWorkTypeRole,
		},
		{
			name: "legacy terminal role owes nothing on a 1.0 work item",
			qw: prompt.QueuedWork{WorkType: "development", WorkTypeRole: "terminal", Fields: stagewire.Fields{
				Card: &stagewire.Card{ID: "c"},
			}},
			want: nothing, source: contractFromWorkTypeRole,
		},
		{
			name: "1.0 work item with no contract and no role owes nothing, never the name table",
			qw: prompt.QueuedWork{WorkType: "development", Fields: stagewire.Fields{
				Stage: &stagewire.Stage{Key: "custom"},
			}},
			want: nothing, source: contractFromNone,
		},
		{
			name:   "no stage and no card: the legacy name table",
			qw:     prompt.QueuedWork{WorkType: "development", WorkTypeRole: "review"},
			want:   inflight,
			source: contractFromLegacyName,
		},
		{
			name:   "no stage and no card, organisation name: the legacy role",
			qw:     prompt.QueuedWork{WorkType: "peer-review", WorkTypeRole: "review"},
			want:   [3]bool{false, true, true},
			source: contractFromLegacyName,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			qw := QueuedWork{QueuedWork: tc.qw}
			got := qw.completionContract()
			if contractFlags(got) != tc.want || got.source != tc.source {
				t.Fatalf("contract = %+v; want %v from %s", got, tc.want, tc.source)
			}
			if qw.requiresPR() != tc.want[0] || qw.requiresReviewVerdict() != tc.want[1] {
				t.Fatal("publication or verdict gate disagrees with the resolved contract")
			}
		})
	}
}

// TestStageWireRoleDefaultContractsReachTheRunner pins the default contract
// of each of the seven roles as the runner reads it from a stage alone.
func TestStageWireRoleDefaultContractsReachTheRunner(t *testing.T) {
	t.Parallel()
	want := map[stagewire.Role][3]bool{
		stagewire.RoleInflight: {true, false, true},
		stagewire.RoleRework:   {true, false, true},
		stagewire.RoleReview:   {false, true, true},
		stagewire.RoleIntake:   {false, false, true},
		stagewire.RoleHold:     {false, false, true},
		stagewire.RoleDone:     {},
		stagewire.RoleClosed:   {},
	}
	for _, role := range stagewire.Roles {
		for _, stage := range []stagewire.Stage{{Key: "s", Role: role}, {Key: "s", Role: stagewire.RoleDone, RunRole: role}} {
			// The name would owe a pull request on the legacy path; the
			// role must decide instead.
			qw := QueuedWork{QueuedWork: prompt.QueuedWork{WorkType: "development", Fields: stagewire.Fields{Stage: &stage}}}
			got := qw.completionContract()
			if contractFlags(got) != want[role] || got.source != contractFromStageRole {
				t.Fatalf("stage %+v: contract %+v; want %v", stage, got, want[role])
			}
		}
	}
}
