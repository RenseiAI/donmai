package runner

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/RenseiAI/donmai/agent"
	"github.com/RenseiAI/donmai/result"
	"github.com/RenseiAI/donmai/runtime/workarea"
	"github.com/RenseiAI/donmai/runtime/worktree"
)

type contextPackGateProvider struct {
	mu       sync.Mutex
	observed agent.Spec
	spawned  bool
	attests  bool
	// workareaCaps, when non-nil, replaces the stub manifest's
	// multi-repository workarea attestation so a scripted session can
	// provision a declared workarea while gating negotiated documents.
	workareaCaps *agent.HarnessCaps
}

func (p *contextPackGateProvider) Name() agent.ProviderName { return agent.ProviderStub }
func (p *contextPackGateProvider) Capabilities() agent.Capabilities {
	return agent.Capabilities{SupportsMessageInjection: true, SupportsSessionResume: true}
}

func (p *contextPackGateProvider) Manifest() agent.HarnessManifest {
	caps := agent.HarnessCaps{NoticeDelivery: agent.NoticeDeliveryInBoxLoop, SupportsInteractivePTY: true}
	if p.attests {
		caps.ContextPackProtocols = []string{workarea.ContextPackProtocol}
	}
	if p.workareaCaps != nil {
		caps.MultiRepositoryWorkareaProtocols = p.workareaCaps.MultiRepositoryWorkareaProtocols
		caps.RepositoryAuthorityEnforcement = p.workareaCaps.RepositoryAuthorityEnforcement
		caps.SupportsReadOnlySelectedCWD = p.workareaCaps.SupportsReadOnlySelectedCWD
	}
	return agent.HarnessManifest{Name: agent.HarnessStub, HumanLabel: "Context gate", Family: agent.FamilyHarness, ContractABI: "context-gate/v1", Caps: caps}
}

func (p *contextPackGateProvider) Spawn(_ context.Context, spec agent.Spec) (agent.Handle, error) {
	p.mu.Lock()
	p.observed = spec
	p.spawned = true
	p.mu.Unlock()
	events := make(chan agent.Event, 2)
	events <- agent.InitEvent{SessionID: "context-pack-session"}
	events <- agent.ResultEvent{Success: true, Message: "WORK_RESULT:passed"}
	close(events)
	return &contextPackGateHandle{events: events}, nil
}

func (p *contextPackGateProvider) Resume(ctx context.Context, _ string, spec agent.Spec) (agent.Handle, error) {
	return p.Spawn(ctx, spec)
}
func (*contextPackGateProvider) Shutdown(context.Context) error { return nil }

type contextPackGateHandle struct{ events <-chan agent.Event }

func (h *contextPackGateHandle) SessionID() string                  { return "context-pack-session" }
func (h *contextPackGateHandle) Events() <-chan agent.Event         { return h.events }
func (*contextPackGateHandle) Inject(context.Context, string) error { return nil }
func (*contextPackGateHandle) Stop(context.Context) error           { return nil }
func (h *contextPackGateHandle) InteractiveSession() agent.InteractiveSession {
	return nil
}

func testContextPack(t *testing.T, docs map[string]string) (workarea.ContextPackV1, json.RawMessage) {
	t.Helper()
	pack := workarea.ContextPackV1{Version: workarea.ContextPackProtocol, PackID: "pack1"}
	for path, content := range docs {
		sum := sha256.Sum256([]byte(content))
		pack.Documents = append(pack.Documents, workarea.ContextDocumentV1{
			Path:          path,
			ContentBase64: base64.StdEncoding.EncodeToString([]byte(content)),
			Digest:        "sha256:" + hex.EncodeToString(sum[:]),
		})
	}
	raw, err := json.Marshal(pack)
	if err != nil {
		t.Fatal(err)
	}
	return pack, raw
}

func TestRunContextPackMaterializesBeforeSpawn(t *testing.T) {
	h := newRunnerHarness(t)
	provider := &contextPackGateProvider{attests: true}
	if err := h.runner.registry.Register(provider); err != nil {
		t.Fatal(err)
	}
	qw := h.queuedWork("CONTEXT-PACK-LIVE")
	qw.WorkType = "acceptance"
	_, raw := testContextPack(t, map[string]string{"notes/guide.md": "hello pack", "AGENTS.md": "instructions"})
	qw.ContextPack = raw
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	res, err := h.runner.Run(ctx, qw)
	if err != nil || res.Status != "completed" {
		t.Fatalf("Run = %+v, err=%v", res, err)
	}
	if res.ContextPack == nil || len(res.ContextPack.Included) != 2 {
		t.Fatalf("diagnostics = %+v", res.ContextPack)
	}
	for _, inc := range res.ContextPack.Included {
		body, err := os.ReadFile(filepath.Join(res.WorkareaRoot, res.ContextPack.Dir, filepath.FromSlash(inc.Path)))
		if err != nil {
			t.Fatalf("read %s: %v", inc.Path, err)
		}
		if len(body) != inc.Bytes {
			t.Fatalf("byte count mismatch for %s", inc.Path)
		}
	}
	provider.mu.Lock()
	spawned := provider.spawned
	provider.mu.Unlock()
	if !spawned {
		t.Fatal("executor never launched after materialization")
	}
}

func TestRunContextPackRefusalsFailBeforeSpawn(t *testing.T) {
	cases := map[string]func() json.RawMessage{
		"traversal": func() json.RawMessage {
			pack := workarea.ContextPackV1{Version: workarea.ContextPackProtocol, PackID: "pack1"}
			sum := sha256.Sum256([]byte("x"))
			pack.Documents = []workarea.ContextDocumentV1{{Path: "../escape.md", ContentBase64: base64.StdEncoding.EncodeToString([]byte("x")), Digest: "sha256:" + hex.EncodeToString(sum[:])}}
			raw, _ := json.Marshal(pack)
			return raw
		},
		"digest": func() json.RawMessage {
			raw, _ := json.Marshal(workarea.ContextPackV1{Version: workarea.ContextPackProtocol, PackID: "pack1", Documents: []workarea.ContextDocumentV1{{Path: "ok.md", ContentBase64: base64.StdEncoding.EncodeToString([]byte("x")), Digest: "sha256:" + hex.EncodeToString(make([]byte, 32))}}})
			return raw
		},
		"unsupported-version": func() json.RawMessage {
			raw, _ := json.Marshal(map[string]any{"version": "unknown-v9", "packId": "pack1", "documents": []any{}})
			return raw
		},
	}
	for name, carrier := range cases {
		t.Run(name, func(t *testing.T) {
			h := newRunnerHarness(t)
			provider := &contextPackGateProvider{attests: true}
			if err := h.runner.registry.Register(provider); err != nil {
				t.Fatal(err)
			}
			qw := h.queuedWork("CONTEXT-REFUSE-" + name)
			qw.ContextPack = carrier()
			res, err := h.runner.Run(context.Background(), qw)
			if err == nil || res.FailureMode != FailureWorktreeProvision {
				t.Fatalf("refusal = %+v, err=%v", res, err)
			}
			if res.ContextPack != nil {
				t.Fatal("rejected documents claimed as included")
			}
			provider.mu.Lock()
			spawned := provider.spawned
			provider.mu.Unlock()
			if spawned {
				t.Fatal("executor launched despite refusal")
			}
		})
	}
}

func TestRunContextPackAdvertisementMatchesExecution(t *testing.T) {
	_, raw := testContextPack(t, map[string]string{"doc.md": "x"})
	h := newRunnerHarness(t)
	plain := &contextPackGateProvider{attests: false}
	if err := h.runner.registry.Register(plain); err != nil {
		t.Fatal(err)
	}
	qw := h.queuedWork("CONTEXT-UNATTESTED")
	qw.ContextPack = raw
	res, err := h.runner.Run(context.Background(), qw)
	if err == nil || res.FailureMode != FailureWorktreeProvision {
		t.Fatalf("unsupported carrier = %+v, err=%v", res, err)
	}
	plain.mu.Lock()
	spawned := plain.spawned
	plain.mu.Unlock()
	if spawned {
		t.Fatal("unsupported carrier launched the executor")
	}
	// Legacy sessions without the carrier remain unchanged.
	h2 := newRunnerHarness(t)
	if err := h2.runner.registry.Register(&contextPackGateProvider{attests: true}); err != nil {
		t.Fatal(err)
	}
	legacy := h2.queuedWork("CONTEXT-LEGACY")
	res2, err := h2.runner.Run(context.Background(), legacy)
	if err != nil || res2.Status != "completed" || res2.ContextPack != nil {
		t.Fatalf("legacy = %+v, err=%v", res2, err)
	}
	if _, statErr := os.Stat(filepath.Join(res2.WorkareaRoot, workarea.DeclarationDirName, workarea.ContextPackDirName)); !os.IsNotExist(statErr) {
		t.Fatal("legacy session created a context directory")
	}
}

// TestRunContextPackReachesTerminalStatusPost pins the diagnostics
// plumbing through the production Run entry: the terminal /status post
// must carry the materialized pack report, not just the in-memory
// result value.
func TestRunContextPackReachesTerminalStatusPost(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	platform := newRecordingPlatformServer(t)
	wtm, err := worktree.NewManager(worktree.Options{ParentDir: t.TempDir()})
	if err != nil {
		t.Fatalf("worktree.NewManager: %v", err)
	}
	poster, err := result.NewPoster(result.Options{
		PlatformURL: platform.URL,
		WorkerID:    "worker-1",
		AuthToken:   "tok",
		HTTPClient:  platform.Client(),
		BaseDelay:   1,
	})
	if err != nil {
		t.Fatalf("result.NewPoster: %v", err)
	}
	reg := NewRegistry()
	provider := &contextPackGateProvider{attests: true}
	if err := reg.Register(provider); err != nil {
		t.Fatal(err)
	}
	r, err := New(Options{
		Registry:               reg,
		WorktreeManager:        wtm,
		Poster:                 poster,
		HTTPClient:             platform.Client(),
		SkipBackstop:           true,
		SkipSteering:           true,
		SkipPostSession:        true,
		PreserveWorktreeAlways: true,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	qw := QueuedWork{
		QueuedWork:  queuedWorkBase("CONTEXT-PACK-STATUS"),
		WorkerID:    "worker-1",
		AuthToken:   "tok",
		PlatformURL: platform.URL,
		ResolvedProfile: ResolvedProfile{
			Provider: agent.ProviderStub,
		},
	}
	qw.WorkType = "acceptance"
	qw.Repository = makeBareRepo(t)
	_, raw := testContextPack(t, map[string]string{"doc.md": "negotiated bytes"})
	qw.ContextPack = raw
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	res, err := r.Run(ctx, qw)
	if err != nil || res.Status != "completed" {
		t.Fatalf("Run = %+v, err=%v", res, err)
	}
	if res.ContextPack == nil || len(res.ContextPack.Included) != 1 {
		t.Fatalf("diagnostics = %+v", res.ContextPack)
	}
	status := platform.terminalStatus(t)
	posted, ok := status["contextPack"]
	if !ok {
		t.Fatalf("terminal /status POST omitted contextPack (keys present: %d)", len(status))
	}
	var report workarea.ContextPackReport
	if err := json.Unmarshal(posted, &report); err != nil {
		t.Fatalf("posted contextPack is not a pack report: %v", err)
	}
	if report.PackID != res.ContextPack.PackID || len(report.Included) != len(res.ContextPack.Included) {
		t.Fatalf("posted contextPack = %+v, want %+v", report, res.ContextPack)
	}
	if len(report.Included) != 1 || report.Included[0].Path != "doc.md" {
		t.Fatalf("posted included = %+v", report.Included)
	}
}

// declaredContextPackFixture provisions retained declared workareas for the
// refusal-path tests below: the provider attests both the negotiated-document
// protocol and the declared-workarea capabilities, and both runners share one
// worktree parent so the second run re-enters the first run's generation.
func declaredContextPackFixture(t *testing.T, repoURL string) (*workarea.RepositoryDeclarationV1, *agent.HarnessCaps) {
	t.Helper()
	declaration := &workarea.RepositoryDeclarationV1{
		Protocol: workarea.ProtocolSessionRootV1,
		Repositories: []workarea.DeclaredRepositoryV1{
			{Source: workarea.RepositorySource{Repository: repoURL}, Name: "primary", Role: workarea.RepositoryRolePrimary, Authority: workarea.RepositoryMutable},
		},
	}
	caps := &agent.HarnessCaps{
		MultiRepositoryWorkareaProtocols: []string{string(workarea.ProtocolSessionRootV1)},
		RepositoryAuthorityEnforcement:   string(workarea.RepositoryAuthorityIsolatedReadOnlyV1),
		SupportsReadOnlySelectedCWD:      true,
	}
	return declaration, caps
}

func newDeclaredContextPackRunner(t *testing.T, sharedParent, rescueDir string, platform *recordingPlatformServer, preserveAlways bool, caps *agent.HarnessCaps) (*Runner, *contextPackGateProvider) {
	t.Helper()
	provider := &contextPackGateProvider{attests: true, workareaCaps: caps}
	manager, err := worktree.NewManager(worktree.Options{ParentDir: sharedParent})
	if err != nil {
		t.Fatalf("worktree.NewManager: %v", err)
	}
	poster, err := result.NewPoster(result.Options{
		PlatformURL: platform.URL, WorkerID: "worker-1", AuthToken: "tok",
		HTTPClient: platform.Client(), BaseDelay: 1,
	})
	if err != nil {
		t.Fatalf("result.NewPoster: %v", err)
	}
	reg := NewRegistry()
	if err := reg.Register(provider); err != nil {
		t.Fatalf("Register: %v", err)
	}
	r, err := New(Options{
		Registry: reg, WorktreeManager: manager, Poster: poster,
		HTTPClient: platform.Client(), SkipBackstop: true, SkipPostSession: true,
		PreserveWorktreeAlways: preserveAlways, RescueDir: rescueDir,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	r.skipSteering = true
	r.providerRetryBackoff = func(int) time.Duration { return 0 }
	return r, provider
}

func declaredContextPackWork(t *testing.T, sessionID, repoURL string, platform *recordingPlatformServer, declaration *workarea.RepositoryDeclarationV1) QueuedWork {
	t.Helper()
	qw := QueuedWork{
		QueuedWork:      queuedWorkBase("CTX-DECLARED"),
		WorkerID:        "worker-1",
		AuthToken:       "tok",
		PlatformURL:     platform.URL,
		ResolvedProfile: ResolvedProfile{Provider: agent.ProviderStub},
	}
	qw.SessionID = sessionID
	qw.Repository = repoURL
	qw.RepositoryDeclaration = declaration
	// Acceptance work carries no pull-request completion contract, so the
	// run can complete without opening a PR for the mutable repository.
	qw.WorkType = "acceptance"
	return qw
}

// TestRunContextPackRefusalPreservesPriorWork pins the refusal-rescue fix:
// a re-entered declared session whose negotiated documents are refused must
// still archive unrelated repository work before teardown deletes the
// workarea.
func TestRunContextPackRefusalPreservesPriorWork(t *testing.T) {
	const repoURL = "https://github.com/example/prior-work-repo"
	githubRepositoryFixture(t, repoURL)
	declaration, caps := declaredContextPackFixture(t, repoURL)
	sharedParent := t.TempDir()
	rescueDir := t.TempDir()
	platform := newRecordingPlatformServer(t)
	const sessionID = "test-session-CTX-PRIOR"

	first, _ := newDeclaredContextPackRunner(t, sharedParent, rescueDir, platform, true, caps)
	qw1 := declaredContextPackWork(t, sessionID, repoURL, platform, declaration)
	_, raw := testContextPack(t, map[string]string{"doc.md": "negotiated bytes"})
	qw1.ContextPack = raw
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	res1, err := first.Run(ctx, qw1)
	if err != nil || res1.Status != "completed" {
		t.Fatalf("first Run = %+v, err=%v", res1, err)
	}
	// Unpublished repository work the re-entered session must not destroy.
	if err := os.WriteFile(filepath.Join(res1.WorktreePath, "prior.txt"), []byte("unpublished repository work\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	// An operator change to a published document turns the identical
	// redelivery into a refusal.
	publishedAbs := filepath.Join(res1.WorkareaRoot, workarea.DeclarationDirName, workarea.ContextPackDirName, "pack1", "doc.md")
	if err := os.WriteFile(publishedAbs, []byte("operator bytes\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	second, provider2 := newDeclaredContextPackRunner(t, sharedParent, rescueDir, platform, false, caps)
	qw2 := declaredContextPackWork(t, sessionID, repoURL, platform, declaration)
	qw2.ContextPack = raw
	res2, err := second.Run(ctx, qw2)
	if err == nil || res2.FailureMode != FailureWorktreeProvision {
		t.Fatalf("refusal = %+v, err=%v", res2, err)
	}
	if res2.ContextPack != nil {
		t.Fatal("rejected documents claimed as included")
	}
	if len(res2.rescueTargets) == 0 {
		t.Fatal("refusal recorded no rescue targets; prior work cannot be preserved")
	}
	provider2.mu.Lock()
	spawned := provider2.spawned
	provider2.mu.Unlock()
	if spawned {
		t.Fatal("executor launched despite refusal")
	}
	patch := onlyRescuePatch(t, rescueDir)
	if body := readFile(t, patch); !strings.Contains(body, "prior.txt") || !strings.Contains(body, "unpublished repository work") {
		t.Fatalf("rescue patch does not carry the prior work: %q", body)
	}
}

// TestRunContextPackRefusesSymlinkedContextParent pins the dedicated
// directory fix through the production Run entry: a retained declared
// workarea whose session-owned context directory is a symlink into other
// storage must refuse before spawn without writing there.
func TestRunContextPackRefusesSymlinkedContextParent(t *testing.T) {
	const repoURL = "https://github.com/example/symlink-ctx-repo"
	githubRepositoryFixture(t, repoURL)
	declaration, caps := declaredContextPackFixture(t, repoURL)
	sharedParent := t.TempDir()
	rescueDir := t.TempDir()
	platform := newRecordingPlatformServer(t)
	const sessionID = "test-session-CTX-SYMLINK"

	first, _ := newDeclaredContextPackRunner(t, sharedParent, rescueDir, platform, true, caps)
	qw1 := declaredContextPackWork(t, sessionID, repoURL, platform, declaration)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	res1, err := first.Run(ctx, qw1)
	if err != nil || res1.Status != "completed" {
		t.Fatalf("first Run = %+v, err=%v", res1, err)
	}
	outside := filepath.Join(res1.WorkareaRoot, "redirect-target")
	if err := os.Mkdir(outside, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(outside, "sentinel.md"), []byte("repository bytes"), 0o600); err != nil {
		t.Fatal(err)
	}
	// The link stays inside the workarea root (like `.workarea/context ->
	// ../primary` in a retained declared workarea), so only the
	// dedicated-directory guard — not root confinement — can stop the
	// redirect into repository storage.
	linkAbs := filepath.Join(res1.WorkareaRoot, workarea.DeclarationDirName, workarea.ContextPackDirName)
	if _, err := os.Lstat(linkAbs); !os.IsNotExist(err) {
		t.Fatalf("context directory unexpectedly exists: %v", err)
	}
	if err := os.Symlink(outside, linkAbs); err != nil {
		t.Fatal(err)
	}

	second, provider2 := newDeclaredContextPackRunner(t, sharedParent, rescueDir, platform, true, caps)
	qw2 := declaredContextPackWork(t, sessionID, repoURL, platform, declaration)
	_, raw := testContextPack(t, map[string]string{"guide.md": "negotiated bytes"})
	qw2.ContextPack = raw
	res2, err := second.Run(ctx, qw2)
	if err == nil || res2.FailureMode != FailureWorktreeProvision {
		t.Fatalf("redirected pack = %+v, err=%v", res2, err)
	}
	if res2.ContextPack != nil {
		t.Fatal("redirected documents claimed as included")
	}
	if !strings.Contains(res2.Error, "not a real directory") {
		t.Fatalf("redirection refusal = %q; want the dedicated-directory guard", res2.Error)
	}
	provider2.mu.Lock()
	spawned := provider2.spawned
	provider2.mu.Unlock()
	if spawned {
		t.Fatal("executor launched with a redirected context directory")
	}
	entries, err := os.ReadDir(outside)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != "sentinel.md" {
		t.Fatalf("storage changed through symlinked context: %v", entries)
	}
	if body := readFile(t, filepath.Join(outside, "sentinel.md")); body != "repository bytes" {
		t.Fatalf("repository file overwritten: %q", body)
	}
}
