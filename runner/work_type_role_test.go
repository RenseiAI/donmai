package runner

import (
	"context"
	"reflect"
	"testing"
	"time"

	"github.com/RenseiAI/donmai/agent"
	"github.com/RenseiAI/donmai/provider/harness/stub"
)

func TestWorkTypeRoleContracts(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		role                                    string
		result, pr, review, sensitive, contract bool
		fields                                  []CompletionFieldType
	}{
		{"inflight", true, true, false, true, true, []CompletionFieldType{FieldCommitsPresent, FieldBranchPushed, FieldPRURL, FieldWorkResult}},
		{"review", true, false, true, true, true, []CompletionFieldType{FieldWorkResult}},
		{"intake", false, false, false, false, true, nil},
		{"terminal", false, false, false, false, false, nil},
		{"", false, false, false, false, false, nil},
		{"future", false, false, false, false, false, nil},
		{"INFLIGHT", false, false, false, false, false, nil},
	} {
		t.Run(tc.role, func(t *testing.T) {
			t.Parallel()
			const name = "chief-of-staff"
			role, valid := ParseWorkTypeRole(tc.role)
			if valid != (tc.contract || tc.role == "terminal") || (!valid && role != "") {
				t.Fatal("unexpected role validation")
			}
			got := []bool{RequiresWorkResult(name, tc.role), RequiresPRURL(name, tc.role), RequiresReviewVerdict(name, tc.role), isResultSensitive(name, tc.role)}
			if !reflect.DeepEqual(got, []bool{tc.result, tc.pr, tc.review, tc.sensitive}) {
				t.Fatalf("obligations = %v", got)
			}
			if shouldSteer(streamObservation{terminalSuccess: true}, agent.Capabilities{SupportsMessageInjection: true}, name, tc.role) != tc.pr || shouldBackstop(&Result{}, name, tc.role) != tc.pr {
				t.Fatal("publication recovery disagrees with contract")
			}
			if d := resolveTargetStatus(name, "completed", "unknown", false, tc.role); d.PostDiagnostic != tc.sensitive {
				t.Fatalf("result sensitivity ignored: %+v", d)
			}
			c, ok := GetCompletionContract(name, tc.role)
			var fields []CompletionFieldType
			for _, f := range c.Required {
				fields = append(fields, f.Type)
			}
			if ok != tc.contract || !reflect.DeepEqual(fields, tc.fields) || (ok && c.WorkType != name) {
				t.Fatalf("contract = %+v, %v", c, ok)
			}
		})
	}
}

func TestWorkTypeRoleLegacyInvariance(t *testing.T) {
	t.Parallel()
	// Literal pre-role expectations, including known names without contracts.
	result := map[string]bool{"development": true, "inflight": true, "coordination": true, "inflight-coordination": true, "qa": true, "acceptance": true}
	for _, name := range AllWorkTypes {
		for _, role := range [][]string{nil, {""}, {"intake"}, {"inflight"}, {"review"}, {"terminal"}, {"future"}} {
			want := []bool{result[name], name == "development" || name == "inflight", name == "qa" || name == "acceptance", result[name] || name == "merge"}
			got := []bool{RequiresWorkResult(name, role...), RequiresPRURL(name, role...), RequiresReviewVerdict(name, role...), isResultSensitive(name, role...)}
			legacy, exists := completionContracts[name]
			contract, ok := GetCompletionContract(name, role...)
			if !reflect.DeepEqual(got, want) || ok != exists || !reflect.DeepEqual(contract, legacy) {
				t.Fatalf("%s/%v: obligations %v, contract %+v", name, role, got, contract)
			}
		}
	}
}

func TestWorkTypeRoleRunnerPushedWithoutPR(t *testing.T) {
	stubGhOnPath(t, 1, "publication unavailable")
	h := newRunnerHarness(t)
	base := gitRun(t, h.bareRepo, "rev-parse", "refs/heads/main")
	inner, err := stub.New()
	if err != nil {
		t.Fatal(err)
	}
	provider := &branchWritingProvider{Provider: inner, expectedBase: base, expectedBranch: "work/role"}
	registry, err := NewRegistryWithOptions(RegistryOptions{RuntimeTransportMode: RuntimeTransportLocalV2})
	if err != nil {
		t.Fatal(err)
	}
	if err = registry.Register(provider); err != nil {
		t.Fatal(err)
	}
	h.runner.registry = registry
	h.runner.skipBackstop = false
	qw := h.queuedWork("role-completion")
	qw.AuthToken = ""
	qw.WorkType, qw.WorkTypeRole = "chief-of-staff", "inflight"
	qw.BaseRef, qw.Ref, qw.Branch = "main", "", "work/role"
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	res, err := h.runner.Run(ctx, qw)
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != "failed" || res.FailureMode != FailureBackstop || res.PullRequestURL != "" || res.BackstopReport == nil || !res.BackstopReport.Pushed {
		t.Fatalf("unpublished work: status=%s failure=%s PR=%q", res.Status, res.FailureMode, res.PullRequestURL)
	}
	if got := gitRun(t, h.bareRepo, "rev-parse", "refs/heads/work/role"); got == base {
		t.Fatal("branch was not pushed")
	}
}
