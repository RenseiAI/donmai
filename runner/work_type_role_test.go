package runner

import (
	"context"
	"os/exec"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/RenseiAI/donmai/agent"
	"github.com/RenseiAI/donmai/provider/harness/stub"
	"github.com/RenseiAI/donmai/runtime/workarea"
)

// TestWorkTypeRoleUnrecognized covers the observability branch for role
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
// gate through a full Run: a declared custom inflight session whose
// mutable repository gains no pull request fails with FailureBackstop.
// Dropping the role at the loop's missingPRs gate skips the contract
// failure for role-carrying work, so this test goes RED on that mutant.
// The session writes one file but never pushes or opens a pull request,
// so the single mutable repository stays missing; the backstop is
// disabled so the failure can only come from the missing-PR gate.
func TestWorkTypeRoleDeclaredMissingPR(t *testing.T) {
	bare := githubRepositoryFixture(t, "https://github.com/example/project.git")
	_ = bare
	res, _ := runScriptedSession(t, scriptedSession{
		workType:     "chief-of-staff",
		workTypeRole: "inflight",
		declaration: &workarea.RepositoryDeclarationV1{
			Protocol: workarea.ProtocolSessionRootV1,
			Repositories: []workarea.DeclaredRepositoryV1{
				{Source: workarea.RepositorySource{Repository: "https://github.com/example/project.git"}, Name: "primary", Role: workarea.RepositoryRolePrimary, Authority: workarea.RepositoryMutable},
			},
		},
		turns: []verdictScriptTurn{{
			text:  "Work done.\nWORK_RESULT: passed",
			files: map[string]string{"fix.go": "package fix\n"},
		}},
	})
	if res.Status != "failed" || res.FailureMode != FailureBackstop {
		t.Fatalf("Status = %q (%s: %s); want failed %s", res.Status, res.FailureMode, res.Error, FailureBackstop)
	}
	if !strings.Contains(res.Error, "completion contract missing pull requests") {
		t.Fatalf("Error = %q; want the missing pull request contract failure", res.Error)
	}
}

// TestWorkTypeRoleDeclaredPRRelievesMissing pins the other half of the
// missing-PR gate through a full Run: a declared custom inflight session
// whose mutable repository gains its pull request does not fail on the
// contract. The turn pushes to its pull request ref, so the verifier
// confirms the envelope's pull request as the session's own.
func TestWorkTypeRoleDeclaredPRRelievesMissing(t *testing.T) {
	bare := githubRepositoryFixture(t, followUpRepository)
	setPullRef(t, bare, 7, pullAtSessionCommit)
	res, _ := runScriptedSession(t, scriptedSession{
		workType:     "chief-of-staff",
		workTypeRole: "inflight",
		declaration: &workarea.RepositoryDeclarationV1{
			Protocol: workarea.ProtocolSessionRootV1,
			Repositories: []workarea.DeclaredRepositoryV1{
				{Source: workarea.RepositorySource{Repository: followUpRepository}, Name: "primary", Role: workarea.RepositoryRolePrimary, Authority: workarea.RepositoryMutable},
			},
		},
		pulls: map[int]string{7: pullAtSessionCommit},
		turns: []verdictScriptTurn{{
			text:  "Opened " + followUpPR + ".\nWORK_RESULT: passed",
			files: map[string]string{"fix.go": "package fix\n"},
			push:  []string{"refs/pull/7/head"},
		}},
	})
	if res.Status != "completed" {
		t.Fatalf("Status = %q (%s: %s); want completed", res.Status, res.FailureMode, res.Error)
	}
	if res.PullRequestURL != followUpPR {
		t.Fatalf("PullRequestURL = %q; want %q", res.PullRequestURL, followUpPR)
	}
}

// TestWorkTypeRoleContinueSeed pins the continue-mode seed gate through a
// full Run: a custom inflight continue session seeds the continued pull
// request before tail recovery, so a no-new-commit run still reports it.
// Dropping the role at the seed gate leaves nothing seeded, so the
// envelope carries no pull request and this test goes RED on that mutant.
func TestWorkTypeRoleContinueSeed(t *testing.T) {
	res, _ := runScriptedSession(t, scriptedSession{
		workType:       "chief-of-staff",
		workTypeRole:   "inflight",
		skipSteering:   true,
		repository:     "https://github.com/example/repo",
		continueNumber: 12,
		turns:          []verdictScriptTurn{{text: "nothing to do"}},
	})
	if res.PullRequestURL != "https://github.com/example/repo/pull/12" {
		t.Fatalf("PullRequestURL = %q; want the continued pull request under test", res.PullRequestURL)
	}
}

// TestWorkTypeRoleContinueDelivery pins the continue-mode delivery gate
// through a full Run: a custom inflight continue session that leaves the
// continued head unmoved fails the no-new-commit delivery check. Dropping
// the role at the delivery gate skips that check, so the session ends
// completed instead of failed and this test goes RED on that mutant.
func TestWorkTypeRoleContinueDelivery(t *testing.T) {
	res, _ := runScriptedSession(t, scriptedSession{
		workType:       "chief-of-staff",
		workTypeRole:   "inflight",
		skipSteering:   true,
		repository:     "https://github.com/example/repo",
		continueNumber: 12,
		turns:          []verdictScriptTurn{{text: "WORK_RESULT:passed"}},
	})
	if res.Status != "failed" || res.FailureMode != FailureBackstop {
		t.Fatalf("Status = %q (%s: %s); want failed %s", res.Status, res.FailureMode, res.Error, FailureBackstop)
	}
	if want := "continued pull request #12 has no new commit since dispatch"; res.Error != want {
		t.Fatalf("Error = %q, want %q", res.Error, want)
	}
}

// TestWorkTypeRoleContinueProbe pins the continue-mode backstop probe
// through a full Run: a custom inflight continue session that leaves the
// continued head unmoved still pushes its commit to the continued branch
// via the probe-cleared eligibility. Dropping the role at the probe
// clears eligibility, so nothing is pushed and this test goes RED on
// that mutant.
func TestWorkTypeRoleContinueProbe(t *testing.T) {
	res, _ := runScriptedSession(t, scriptedSession{
		workType:       "chief-of-staff",
		workTypeRole:   "inflight",
		skipSteering:   true,
		repository:     "https://github.com/example/repo",
		continueNumber: 12,
		backstop:       true,
		turns: []verdictScriptTurn{{
			manifest: passedManifest,
			text:     "WORK_RESULT:passed",
			files:    map[string]string{"fix.go": "package fix\n"},
		}},
	})
	if res.Status != "completed" {
		t.Fatalf("Status = %q (%s: %s); want completed", res.Status, res.FailureMode, res.Error)
	}
	if res.PullRequestURL != "https://github.com/example/repo/pull/12" {
		t.Fatalf("PullRequestURL = %q; want the continued pull request", res.PullRequestURL)
	}
	remoteHead := gitRun(t, res.WorktreePath, "ls-remote", "origin", "refs/heads/continued/pr-12")
	if !strings.Contains(remoteHead, gitRun(t, res.WorktreePath, "rev-parse", "HEAD")) {
		t.Fatalf("continued head %q does not carry the session commit", remoteHead)
	}
}

// TestWorkTypeRoleDeclaredSecondaryBackstop pins the declared
// backstop-eligibility gate through a full Run: a declared custom inflight
// session whose primary repository already carries its pull request still
// runs the declared backstop for the secondary mutable repository without
// one. Dropping the role at the eligibility switch skips the backstop, so
// the secondary stays missing and the session fails on the missing-PR
// contract instead of completing; this test goes RED on that mutant.
func TestWorkTypeRoleDeclaredSecondaryBackstop(t *testing.T) {
	primaryURL := "https://github.com/example/primary.git"
	secondaryURL := "https://github.com/example/secondary.git"
	primaryBare := githubRepositoryFixture(t, primaryURL)
	_ = githubRepositoryFixture(t, secondaryURL)
	setPullRef(t, primaryBare, 7, pullAtSessionCommit)
	primaryPR := "https://github.com/example/primary/pull/7"
	secondaryPR := "https://github.com/example/secondary/pull/8"
	stubGhVisibilityAndPRCreate(t, "PRIVATE", secondaryPR)
	res, _ := runScriptedSession(t, scriptedSession{
		workType:     "chief-of-staff",
		workTypeRole: "inflight",
		skipSteering: true,
		declaration: &workarea.RepositoryDeclarationV1{
			Protocol: workarea.ProtocolSessionRootV1,
			Repositories: []workarea.DeclaredRepositoryV1{
				{Source: workarea.RepositorySource{Repository: primaryURL}, Name: "primary", Role: workarea.RepositoryRolePrimary, Authority: workarea.RepositoryMutable},
				{Source: workarea.RepositorySource{Repository: secondaryURL}, Name: "secondary", Role: workarea.RepositoryRoleSecondary, Authority: workarea.RepositoryMutable},
			},
		},
		backstop: true,
		turns: []verdictScriptTurn{{
			text:  "Opened " + primaryPR + ".\nWORK_RESULT: passed",
			files: map[string]string{"fix.go": "package fix\n"},
			push:  []string{"refs/pull/7/head"},
		}},
	})
	if res.Status != "completed" {
		t.Fatalf("Status = %q (%s: %s); want completed", res.Status, res.FailureMode, res.Error)
	}
	if strings.Contains(res.Error, "completion contract missing pull requests") {
		t.Fatalf("Error = %q; want no missing-PR contract failure", res.Error)
	}
	if res.PullRequestURL != primaryPR {
		t.Fatalf("PullRequestURL = %q; want %q", res.PullRequestURL, primaryPR)
	}
	if res.BackstopReport == nil {
		t.Fatal("BackstopReport = nil; want the declared backstop to have run for the secondary")
	}
	var secondary *agent.RepositoryBackstopReport
	for i := range res.BackstopReport.Repositories {
		if res.BackstopReport.Repositories[i].Name == "secondary" {
			secondary = &res.BackstopReport.Repositories[i]
			break
		}
	}
	if secondary == nil {
		t.Fatalf("BackstopReport = %+v; want a secondary entry", res.BackstopReport.Repositories)
	}
	if !secondary.Report.Triggered {
		t.Fatalf("secondary backstop Triggered = false (report=%+v); want the secondary recovered", secondary.Report)
	}
	if secondary.Report.PRURL != secondaryPR {
		t.Fatalf("secondary backstop PRURL = %q; want %q", secondary.Report.PRURL, secondaryPR)
	}
}
