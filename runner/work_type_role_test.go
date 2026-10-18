package runner

import (
	"context"
	"os/exec"
	"reflect"
	"testing"
	"time"

	"github.com/RenseiAI/donmai/agent"
	"github.com/RenseiAI/donmai/provider/harness/stub"
	"github.com/RenseiAI/donmai/runtime/workarea"
)

// TestWorkTypeRoleUnrecognizedNames the observability branch for role
// vocabulary skew: a non-empty unknown role for an unknown name reports a
// skew, while empty roles, valid roles, and any role on a known name stay
// silent. Deleting the warn branch (always silent) fails the skew rows.
func TestWorkTypeRoleUnrecognized(t *testing.T) {
	cases := []struct {
		workType, role string
		want           bool
	}{
		{"chief-of-staff", "future", true},
		{"chief-of-staff", "INFLIGHT", true},
		{"chief-of-staff", "", false},
		{"chief-of-staff", "inflight", false},
		{"chief-of-staff", "terminal", false},
		{"development", "future", false},
		{"development", "", false},
		{"qa", "bogus", false},
	}
	for _, tc := range cases {
		t.Run(tc.workType+"/"+tc.role, func(t *testing.T) {
			if got := unrecognizedWorkTypeRole(tc.workType, tc.role); got != tc.want {
				t.Fatalf("unrecognizedWorkTypeRole(%q, %q) = %v; want %v", tc.workType, tc.role, got, tc.want)
			}
		})
	}
}

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

// TestWorkTypeRoleCheckpointProviderError pins the provider-error decision
// point through its production entry point: custom inflight work that fails
// on a provider error must leave a pushed WIP checkpoint instead of being
// marked resumable with nothing kept. Dropping the role at the checkpoint
// call marks the session resumable and skips the checkpoint, so this test
// goes RED.
func TestWorkTypeRoleCheckpointProviderError(t *testing.T) {
	bare, wt, res := checkpointCheckout(t)
	work := queuedWorkBase("WIP-ROLE")
	work.WorkType, work.WorkTypeRole = "chief-of-staff", "inflight"
	writeFile(t, wt, "notes.txt", "work\n")
	(&Runner{logger: noopLogger()}).checkpointProviderError(QueuedWork{QueuedWork: work}, res)
	if res.ResumeCheckpoint == nil {
		t.Fatalf("no checkpoint: Resumable=%v warnings %v", res.Resumable, res.PostSessionWarnings)
	}
	if !res.Resumable {
		t.Fatal("checkpointed session was not marked resumable")
	}
	if got := gitRun(t, bare, "rev-parse", "refs/heads/wip/"+work.SessionID); got != res.ResumeCheckpoint.CommitSHA {
		t.Fatalf("remote WIP head = %s, want %s", got, res.ResumeCheckpoint.CommitSHA)
	}
}

// TestWorkTypeRoleRunPostSession pins the post-session decision point
// through its production entry point: custom inflight and review work that
// ends without a work result must post the missing-result diagnostic.
// Dropping the role at the resolveTargetStatus call reads the session as
// not result-sensitive, so no diagnostic is posted and this test goes RED.
func TestWorkTypeRoleRunPostSession(t *testing.T) {
	for _, role := range []string{"inflight", "review"} {
		t.Run(role, func(t *testing.T) {
			srv, calls, mu := stubProxyServer(t)
			r := makePostSessionRunner(t, srv)
			work := queuedWorkBase("ROLE-POST-" + role)
			work.WorkType, work.WorkTypeRole = "chief-of-staff", role
			qw := QueuedWork{QueuedWork: work, WorkerID: "wkr-post", AuthToken: "tok-post", PlatformURL: srv.URL}
			qw.IssueID = "issue-uuid-role-" + role
			res := &Result{}
			res.Status = "completed"
			r.runPostSession(context.Background(), qw, res)
			mu.Lock()
			defer mu.Unlock()
			if len(*calls) != 1 || (*calls)[0].Method != "createComment" {
				t.Fatalf("proxy calls = %v; want the missing-result diagnostic comment", *calls)
			}
		})
	}
}

// TestWorkTypeRoleDeliveredAtBudgetStop pins the budget-stop decision point:
// delivered custom inflight work (verified pull request plus a passed
// result) counts as delivered when a cap ends the session. Dropping the
// role reports no PR owed, so delivered work reads as undelivered and this
// test goes RED.
func TestWorkTypeRoleDeliveredAtBudgetStop(t *testing.T) {
	work := queuedWorkBase("ROLE-BUDGET")
	work.WorkType, work.WorkTypeRole = "chief-of-staff", "inflight"
	qw := QueuedWork{QueuedWork: work}
	res := &Result{}
	res.Status = "completed"
	res.PullRequestURL = "https://github.com/example/repo/pull/7"
	res.WorkResult = "passed"
	if !deliveredAtBudgetStop(qw, res, nil) {
		t.Fatal("delivered custom inflight work at a budget stop did not count as delivered")
	}
}

// TestWorkTypeRoleDeclaredBackstop pins the declared multi-repo decision
// point: custom inflight work is backstop-eligible and the declared
// backstop triggers for it. Dropping the role refuses the contract gate,
// so the backstop never triggers and this test goes RED.
func TestWorkTypeRoleDeclaredBackstop(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	remote, repo := backstopRemoteFixture(t)
	_ = remote
	session := "agent/role-declared"
	gitRun(t, repo, "checkout", "-q", "-b", session)
	writeFile(t, repo, "src/fix.go", "package fix\n")
	const wantURL = "https://github.com/example/project/pull/99"
	stubGhVisibilityAndPRCreate(t, "PRIVATE", wantURL)
	work := queuedWorkBase("ROLE-DECLARED")
	work.WorkType, work.WorkTypeRole = "chief-of-staff", "inflight"
	qw := QueuedWork{QueuedWork: work}
	res := &Result{}
	res.Status = "completed"
	res.WorktreePath = repo
	if !shouldBackstop(res, qw.WorkType, qw.WorkTypeRole) {
		t.Fatal("declared custom inflight work was not backstop-eligible")
	}
	declaration, err := (workarea.RepositoryDeclarationV1{
		Protocol: workarea.ProtocolSessionRootV1,
		Repositories: []workarea.DeclaredRepositoryV1{
			{Source: workarea.RepositorySource{Repository: "https://github.com/example/project.git"}, Name: "primary", Role: workarea.RepositoryRolePrimary, Authority: workarea.RepositoryMutable},
		},
	}).Normalize()
	if err != nil {
		t.Fatal(err)
	}
	report := minimalRunner(t).runDeclaredBackstops(context.Background(), qw, session, res, declaration, map[string]string{
		declaration.Selected.Name: repo,
	})
	if !report.Triggered {
		t.Fatalf("declared backstop did not trigger: %+v", report)
	}
}

// TestWorkTypeRoleSteering pins the steering decision points through the
// production entry point: custom inflight work is steer-eligible and the
// steering prompt lands in the session's injected prompts. The delivery is
// asserted on the provider's recorded prompts (not by draining the stub
// handle's event channel, which blocks once the scripted turns are spent).
// Dropping the role at either the shouldSteer call or the attemptSteering
// boundary refuses the contract gate, so no prompt is recorded and this
// test goes RED.
func TestWorkTypeRoleSteering(t *testing.T) {
	obs := streamObservation{terminalSuccess: true}
	caps := agent.Capabilities{SupportsMessageInjection: true}
	if !shouldSteer(obs, caps, "chief-of-staff", "inflight") {
		t.Fatal("custom inflight work was not steer-eligible")
	}
	base, err := stub.New(stub.WithCapabilities(caps))
	if err != nil {
		t.Fatal(err)
	}
	harness, ok := base.(agent.HarnessProvider)
	if !ok {
		t.Fatal("stub provider is not a HarnessProvider")
	}
	provider := &recordingSteerProvider{HarnessProvider: harness}
	r := minimalRunner(t)
	if err := r.registry.Register(provider); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := withCtx(t)
	defer cancel()
	spec := agent.Spec{}
	handle, err := provider.Spawn(ctx, spec)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = handle.Stop(context.Background()) }()
	work := queuedWorkBase("ROLE-STEER")
	work.WorkType, work.WorkTypeRole = "chief-of-staff", "inflight"
	qw := QueuedWork{QueuedWork: work}
	if _, err := r.attemptSteering(ctx, provider, handle, spec, caps, qw, obs, &Result{}); err != nil {
		t.Fatalf("attemptSteering: %v", err)
	}
	if len(provider.prompts) != 1 {
		t.Fatalf("steering prompts = %q; want the single pull request nudge", provider.prompts)
	}
}

// recordingSteerProvider wraps the stub harness with an Inject that records
// the steering text and succeeds without consuming a scripted turn, so the
// test observes delivery without draining a handle channel.
type recordingSteerProvider struct {
	agent.HarnessProvider
	prompts []string
}

func (p *recordingSteerProvider) Spawn(_ context.Context, _ agent.Spec) (agent.Handle, error) {
	return &recordingSteerHandle{provider: p}, nil
}

type recordingSteerHandle struct {
	provider *recordingSteerProvider
}

func (h *recordingSteerHandle) SessionID() string          { return "steer-session" }
func (h *recordingSteerHandle) Events() <-chan agent.Event { return nil }
func (h *recordingSteerHandle) Stop(context.Context) error { return nil }
func (h *recordingSteerHandle) Inject(_ context.Context, text string) error {
	h.provider.prompts = append(h.provider.prompts, text)
	return nil
}

// TestWorkTypeRoleReviewContinued pins the review-role tail-recovery
// decision point through a full Run: a custom review session whose first
// turn ends with no verdict gets the review follow-up turn, and the final
// message holds the verdict as the structured field. Dropping the role at
// the reviewWork decision reads the session as non-review, so no review
// follow-up is sent and this test goes RED.
func TestWorkTypeRoleReviewContinued(t *testing.T) {
	res, provider := runScriptedSession(t, scriptedSession{
		workType:     "chief-of-staff",
		workTypeRole: "review",
		repository:   followUpRepository,
		turns: []verdictScriptTurn{
			{text: "Still reading the diff; no verdict yet."},
			{text: "Findings complete.\nWORK_RESULT: passed\nREVIEW_VERDICT: APPROVE"},
		},
	})
	wantContinuationsWithGoal(t, provider.prompts, continueReviewPrompt)
	wantContinuations(t, res, 1, 0, false)
	if res.WorkResult != "passed" {
		t.Fatalf("WorkResult = %q; want passed", res.WorkResult)
	}
	if res.ReviewVerdict != "APPROVE" {
		t.Fatalf("ReviewVerdict = %q; want APPROVE", res.ReviewVerdict)
	}
}

// TestWorkTypeRoleInflightSteered pins the inflight-role tail-recovery
// decision point through a full Run: a custom inflight session whose first
// turn leaves a passed verdict but no pull request gets the pull request
// nudge. Dropping the role at the steering decision sends no nudge and
// delivers no pull request, so this test goes RED.
func TestWorkTypeRoleInflightSteered(t *testing.T) {
	res, provider := runScriptedSession(t, scriptedSession{
		workType:     "chief-of-staff",
		workTypeRole: "inflight",
		repository:   followUpRepository,
		pulls:        map[int]string{7: pullAtSessionCommit},
		turns: []verdictScriptTurn{
			{text: "Implementation done.\nWORK_RESULT: passed"},
			{text: "Opened " + followUpPR},
		},
	})
	if !res.SteeringTriggered {
		t.Fatalf("SteeringTriggered = false; want the pull request nudge (prompts=%q)", provider.prompts)
	}
	if res.PullRequestURL != followUpPR {
		t.Fatalf("PullRequestURL = %q; want %q", res.PullRequestURL, followUpPR)
	}
}

// TestWorkTypeRoleVerifierBuilt pins the verifier-construction decision
// point through a full Run: a custom inflight session that opens its pull
// request drifts it to a draft, and the tail recovery continues the turn
// with the draft prompt until the pull request is marked ready. Dropping
// the role at verifier construction accepts no pull request, so the draft
// is never observed, no draft continuation is sent, and this test goes RED.
func TestWorkTypeRoleVerifierBuilt(t *testing.T) {
	draft := &scriptedDraft{states: []bool{true, false}}
	res, provider := runScriptedSession(t, scriptedSession{
		workType:     "chief-of-staff",
		workTypeRole: "inflight",
		repository:   followUpRepository,
		pulls:        map[int]string{7: pullAtSessionCommit},
		draft:        draft.lookup,
		turns: []verdictScriptTurn{
			{toolCalls: 2, text: "Opened draft " + followUpPR + " to keep the work safe. Next I will write the tests."},
			{toolCalls: 2, text: "Tests written; " + followUpPR + " is ready for review."},
		},
	})
	if res.Status != "completed" || res.PullRequestURL != followUpPR {
		t.Fatalf("Status=%q PullRequestURL=%q (%s: %s); want completed with %s", res.Status, res.PullRequestURL, res.FailureMode, res.Error, followUpPR)
	}
	wantContinuationsWithGoal(t, provider.prompts, continueDraftPrompt)
	wantContinuations(t, res, 1, 0, false)
	if got := draft.reads(); len(got) != 2 || got[0] != followUpPR || got[1] != followUpPR {
		t.Errorf("draft reads = %q; want the session's pull request read after each turn", got)
	}
}

// TestWorkTypeRoleDeclaredMissingPR pins the per-mutable-repo missing-PR
// gate: a declared custom inflight session whose mutable repository gains
// no pull request lists that repository as missing, while the same session
// without a role lists none. The loop's missingPRs gate consults this list,
// so dropping the role there skips the contract failure for role-carrying
// work; the second assertion goes RED on that mutant.
func TestWorkTypeRoleDeclaredMissingPR(t *testing.T) {
	work := queuedWorkBase("ROLE-DECLARED-MISSING")
	work.WorkType, work.WorkTypeRole = "chief-of-staff", "inflight"
	qw := QueuedWork{QueuedWork: work}
	declaration, err := (workarea.RepositoryDeclarationV1{
		Protocol: workarea.ProtocolSessionRootV1,
		Repositories: []workarea.DeclaredRepositoryV1{
			{Source: workarea.RepositorySource{Repository: "https://github.com/example/project.git"}, Name: "primary", Role: workarea.RepositoryRolePrimary, Authority: workarea.RepositoryMutable},
		},
	}).Normalize()
	if err != nil {
		t.Fatal(err)
	}
	res := &Result{}
	res.Status = "completed"
	// Mirror the loop's missing-PR gate: the missing list is consulted only
	// when the role says a pull request is owed.
	missing := []string(nil)
	if RequiresPRURL(qw.WorkType, qw.WorkTypeRole) {
		missing = missingMutablePullRequests(res, declaration)
	}
	if len(missing) != 1 || missing[0] != declaration.Selected.Name {
		t.Fatalf("missing pull requests = %q; want the declared mutable repository", missing)
	}
	missingWithoutRole := []string(nil)
	if RequiresPRURL(qw.WorkType) {
		missingWithoutRole = missingMutablePullRequests(res, declaration)
	}
	if len(missingWithoutRole) != 0 {
		t.Fatalf("missing pull requests without the role = %q; want none", missingWithoutRole)
	}
}
