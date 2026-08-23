package runner

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/RenseiAI/donmai/agent"
	"github.com/RenseiAI/donmai/attachwire"
	"github.com/RenseiAI/donmai/runtime/workarea"
)

type workareaGateProvider struct {
	mu                       sync.Mutex
	observed                 agent.Spec
	supportsReadOnlySelected bool
}

func (*workareaGateProvider) Name() agent.ProviderName { return agent.ProviderStub }
func (*workareaGateProvider) Capabilities() agent.Capabilities {
	return agent.Capabilities{SupportsMessageInjection: true, SupportsSessionResume: true, HumanLabel: "Workarea gate"}
}

func (p *workareaGateProvider) Manifest() agent.HarnessManifest {
	return agent.HarnessManifest{
		Name: agent.HarnessStub, HumanLabel: "Workarea gate", Family: agent.FamilyHarness, ContractABI: "workarea-gate/v1",
		Caps: agent.HarnessCaps{
			MultiRepositoryWorkareaProtocols: []string{string(workarea.ProtocolSessionRootV1)},
			RepositoryAuthorityEnforcement:   string(workarea.RepositoryAuthorityIsolatedReadOnlyV1),
			NoticeDelivery:                   agent.NoticeDeliveryInBoxLoop,
			SupportsInteractivePTY:           true,
			SupportsReadOnlySelectedCWD:      p.supportsReadOnlySelected,
		},
	}
}

func (p *workareaGateProvider) Spawn(_ context.Context, spec agent.Spec) (agent.Handle, error) {
	p.mu.Lock()
	p.observed = spec
	p.mu.Unlock()
	events := make(chan agent.Event, 2)
	events <- agent.InitEvent{SessionID: "workarea-gate-session"}
	events <- agent.ResultEvent{Success: true, Message: "WORK_RESULT:passed"}
	close(events)
	handle := &workareaGateHandle{events: events}
	if spec.Interactive != nil {
		done := make(chan struct{})
		close(done)
		handle.interactive = &workareaGateInteractiveSession{done: done}
	}
	return handle, nil
}

func (p *workareaGateProvider) Resume(ctx context.Context, _ string, spec agent.Spec) (agent.Handle, error) {
	return p.Spawn(ctx, spec)
}
func (*workareaGateProvider) Shutdown(context.Context) error { return nil }

type workareaGateHandle struct {
	events      <-chan agent.Event
	interactive agent.InteractiveSession
}

func (*workareaGateHandle) SessionID() string                    { return "workarea-gate-session" }
func (h *workareaGateHandle) Events() <-chan agent.Event         { return h.events }
func (*workareaGateHandle) Inject(context.Context, string) error { return nil }
func (*workareaGateHandle) Stop(context.Context) error           { return nil }
func (h *workareaGateHandle) InteractiveSession() agent.InteractiveSession {
	return h.interactive
}

type workareaGateInteractiveSession struct{ done <-chan struct{} }

func (*workareaGateInteractiveSession) WriteInput(p []byte) (int, error) { return len(p), nil }
func (*workareaGateInteractiveSession) Resize(uint32, uint32, uint32, uint32) error {
	return nil
}

func (*workareaGateInteractiveSession) Snapshot() (attachwire.Screen, attachwire.HostSeq, error) {
	return attachwire.Screen{}, 0, nil
}

func (*workareaGateInteractiveSession) EmitSnapshot() (attachwire.Frame, bool, error) {
	return attachwire.Frame{}, false, nil
}
func (*workareaGateInteractiveSession) EmitMarker(string) error { return nil }
func (*workareaGateInteractiveSession) Subscribe(attachwire.HostSeq) (agent.InteractiveSubscription, error) {
	frames := make(chan attachwire.Frame)
	close(frames)
	return &workareaGateSubscription{frames: frames}, nil
}
func (s *workareaGateInteractiveSession) Done() <-chan struct{} { return s.done }
func (*workareaGateInteractiveSession) Exit() (attachwire.ExitPayload, bool) {
	return attachwire.NewNormalExit(0), true
}

type workareaGateSubscription struct{ frames <-chan attachwire.Frame }

func (s *workareaGateSubscription) Frames() <-chan attachwire.Frame { return s.frames }
func (*workareaGateSubscription) Close() error                      { return nil }

// TestRunLegacyWorkItemRemainsFlatWithoutSessionRootNegotiation is the V16
// control for the protocol activation boundary. The current work-item wire has
// no versioned repository declaration and no exact-executor session-root-v1
// attestation, so the runner must retain the representable legacy layout rather
// than activating a protocol the producer never negotiated.
func TestRunLegacyWorkItemRemainsFlatWithoutSessionRootNegotiation(t *testing.T) {
	h := newRunnerHarness(t)
	qw := h.queuedWork("LEGACY-WORKAREA")

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	res, err := h.runner.Run(ctx, qw)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Status != "completed" {
		t.Fatalf("Status = %q (FailureMode=%q, Error=%q)", res.Status, res.FailureMode, res.Error)
	}
	if got := filepath.Base(res.WorktreePath); got != qw.SessionID {
		t.Errorf("legacy worktree leaf = %q; want session id %q until session-root-v1 is negotiated", got, qw.SessionID)
	}
}

func TestRunNegotiatedDeclarationSelectsExactRepositoryAndBindsAuthority(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	h := newRunnerHarness(t)
	provider := &workareaGateProvider{supportsReadOnlySelected: true}
	if err := h.runner.registry.Register(provider); err != nil {
		t.Fatal(err)
	}
	primary := h.bareRepo
	contextRepo := makeBareRepo(t)
	qw := h.queuedWork("NEGOTIATED-WORKAREA")
	qw.WorkType = "acceptance"
	qw.Ref = ""
	qw.RepositoryDeclaration = &workarea.RepositoryDeclarationV1{
		Protocol: workarea.ProtocolSessionRootV1,
		Repositories: []workarea.DeclaredRepositoryV1{
			{Source: workarea.RepositorySource{Repository: primary}, Name: "primary", Role: workarea.RepositoryRolePrimary, Authority: workarea.RepositoryMutable},
			{Source: workarea.RepositorySource{Repository: contextRepo}, Name: "context", Role: workarea.RepositoryRoleContext, Authority: workarea.RepositoryReadOnly},
		},
		Select: &workarea.RepositoryFilter{Kind: workarea.RepositoryFilterNamed, Name: "context"},
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	res, err := h.runner.Run(ctx, qw)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.WorkareaRoot == "" || res.WorkareaRoot == res.WorktreePath || filepath.Base(res.WorktreePath) != "context" {
		t.Fatalf("result paths = root %q cwd %q, want distinct selected context", res.WorkareaRoot, res.WorktreePath)
	}
	origin, err := runGit(context.Background(), res.WorktreePath, gitIdentity{}, "remote", "get-url", "origin")
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(origin) != contextRepo {
		t.Fatalf("selected repository origin = %q, want %q", strings.TrimSpace(origin), contextRepo)
	}
	provider.mu.Lock()
	observed := provider.observed
	provider.mu.Unlock()
	if observed.Cwd != res.WorktreePath || observed.RepositoryAuthority == nil {
		t.Fatalf("provider observed CWD/policy = %q/%#v", observed.Cwd, observed.RepositoryAuthority)
	}
	if observed.SandboxLevel != agent.SandboxWorkspaceWrite || len(observed.RepositoryAuthority.ReadOnlyPaths) != 1 || len(observed.RepositoryAuthority.MutablePaths) != 1 {
		t.Fatalf("provider authority = %#v sandbox=%q", observed.RepositoryAuthority, observed.SandboxLevel)
	}
	if _, err := workarea.ReadDeclaration(workarea.RootPath(res.WorkareaRoot)); err != nil {
		t.Fatalf("ReadDeclaration: %v", err)
	}
	if _, err := os.Stat(filepath.Join(res.WorktreePath, ".agent")); !os.IsNotExist(err) {
		t.Fatalf("runner-owned state modified selected read-only repository: %v", err)
	}
	if _, err := os.Stat(filepath.Join(res.WorkareaRoot, workarea.DeclarationDirName, "runner", ".agent", "state.json")); err != nil {
		t.Fatalf("root-owned runner state missing: %v", err)
	}
}

func TestRunAllMutableDeclarationStillBindsCompleteAuthorityPartition(t *testing.T) {
	h := newRunnerHarness(t)
	provider := &workareaGateProvider{}
	if err := h.runner.registry.Register(provider); err != nil {
		t.Fatal(err)
	}
	qw := h.queuedWork("ALL-MUTABLE-WORKAREA")
	qw.WorkType = "acceptance"
	qw.RepositoryDeclaration = &workarea.RepositoryDeclarationV1{
		Protocol: workarea.ProtocolSessionRootV1,
		Repositories: []workarea.DeclaredRepositoryV1{
			{Source: workarea.RepositorySource{Repository: qw.Repository}, Name: "primary", Role: workarea.RepositoryRolePrimary, Authority: workarea.RepositoryMutable},
			{Source: workarea.RepositorySource{Repository: makeBareRepo(t)}, Name: "secondary", Role: workarea.RepositoryRoleSecondary, Authority: workarea.RepositoryMutable},
		},
	}
	res, err := h.runner.Run(t.Context(), qw)
	if err != nil {
		t.Fatal(err)
	}
	provider.mu.Lock()
	policy := provider.observed.RepositoryAuthority
	provider.mu.Unlock()
	if policy == nil || len(policy.MutablePaths) != 2 || len(policy.ReadOnlyPaths) != 0 || policy.WorkareaRoot != res.WorkareaRoot {
		t.Fatalf("all-mutable authority partition = %#v", policy)
	}
}

func TestRunDeclarationRefusedWhenExactExecutorAttestationIsAbsent(t *testing.T) {
	h := newRunnerHarness(t)
	qw := h.queuedWork("UNATTESTED-WORKAREA")
	qw.RepositoryDeclaration = &workarea.RepositoryDeclarationV1{
		Protocol: workarea.ProtocolSessionRootV1,
		Repositories: []workarea.DeclaredRepositoryV1{{
			Source: workarea.RepositorySource{Repository: qw.Repository}, Role: workarea.RepositoryRolePrimary, Authority: workarea.RepositoryMutable,
		}},
	}
	res, err := h.runner.Run(context.Background(), qw)
	var contractErr *workarea.RepositoryContractError
	if !errors.As(err, &contractErr) || contractErr.Reason != workarea.ReasonProtocolUnsupported {
		t.Fatalf("Run error = %#v, want protocol unsupported", err)
	}
	if res.FailureMode != FailureWorktreeProvision || res.WorktreePath != "" {
		t.Fatalf("result = %#v, want pre-provision refusal", res)
	}
}

func TestRunSelectedReadOnlyRefusedWhenExecutorCannotProtectCWD(t *testing.T) {
	h := newRunnerHarness(t)
	provider := &workareaGateProvider{}
	if err := h.runner.registry.Register(provider); err != nil {
		t.Fatal(err)
	}
	qw := h.queuedWork("READONLY-CWD-REFUSED")
	qw.RepositoryDeclaration = &workarea.RepositoryDeclarationV1{
		Protocol: workarea.ProtocolSessionRootV1,
		Repositories: []workarea.DeclaredRepositoryV1{
			{Source: workarea.RepositorySource{Repository: qw.Repository}, Name: "primary", Role: workarea.RepositoryRolePrimary, Authority: workarea.RepositoryMutable},
			{Source: workarea.RepositorySource{Repository: makeBareRepo(t)}, Name: "context", Role: workarea.RepositoryRoleContext, Authority: workarea.RepositoryReadOnly},
		},
		Select: &workarea.RepositoryFilter{Kind: workarea.RepositoryFilterNamed, Name: "context"},
	}
	res, err := h.runner.Run(t.Context(), qw)
	var contractErr *workarea.RepositoryContractError
	if !errors.As(err, &contractErr) || contractErr.Reason != workarea.ReasonAuthorityEnforcementMissing {
		t.Fatalf("selected read-only refusal = %#v, want authority exclusion", err)
	}
	if res.WorktreePath != "" {
		t.Fatalf("selected read-only refusal provisioned a root: %+v", res)
	}
}

func TestRunInteractiveUsesSameNestedSelectedRepositoryLayout(t *testing.T) {
	h := newRunnerHarness(t)
	provider := &workareaGateProvider{}
	if err := h.runner.registry.Register(provider); err != nil {
		t.Fatal(err)
	}
	qw := h.queuedWork("INTERACTIVE-WORKAREA")
	qw.WorkType = "acceptance"
	qw.Mode = "interactive"
	qw.RepositoryDeclaration = &workarea.RepositoryDeclarationV1{
		Protocol: workarea.ProtocolSessionRootV1,
		Repositories: []workarea.DeclaredRepositoryV1{
			{Source: workarea.RepositorySource{Repository: qw.Repository}, Name: "primary", Role: workarea.RepositoryRolePrimary, Authority: workarea.RepositoryMutable},
			{Source: workarea.RepositorySource{Repository: makeBareRepo(t)}, Name: "context", Role: workarea.RepositoryRoleContext, Authority: workarea.RepositoryReadOnly},
		},
	}
	res, err := h.runner.Run(context.Background(), qw)
	if err != nil {
		t.Fatalf("Run interactive: %v", err)
	}
	if res.Status != "completed" || res.WorkareaRoot == res.WorktreePath || filepath.Base(res.WorktreePath) != "primary" || filepath.Dir(res.WorktreePath) != res.WorkareaRoot {
		t.Fatalf("interactive result paths/status = root %q cwd %q status %q", res.WorkareaRoot, res.WorktreePath, res.Status)
	}
}
