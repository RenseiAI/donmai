package runner

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/RenseiAI/donmai/agent"
	"github.com/RenseiAI/donmai/runtime/workarea"
)

type contextPackGateProvider struct {
	mu       sync.Mutex
	observed agent.Spec
	spawned  bool
	attests  bool
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
