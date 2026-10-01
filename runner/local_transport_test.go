package runner

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/RenseiAI/donmai/agent"
	"github.com/RenseiAI/donmai/executioncell"
	"github.com/RenseiAI/donmai/internal/localqueue"
	"github.com/RenseiAI/donmai/provider/harness/codex"
	"github.com/RenseiAI/donmai/result"
	"github.com/RenseiAI/donmai/runtime/worktree"
)

func localTransportWork(t *testing.T, admission *LocalAdmission) QueuedWork {
	t.Helper()
	var qw QueuedWork
	if err := json.Unmarshal(admission.OperationalPayload, &qw); err != nil {
		t.Fatal(err)
	}
	binding, err := executioncell.DecodeRuntimeBinding(admission.RuntimeBinding)
	if err != nil {
		t.Fatal(err)
	}
	qw.SessionID, qw.WorkerID = binding.RequestID, binding.WorkerID
	qw.PlatformURL = "http://127.0.0.1:7734/api/daemon/local"
	qw.AdmissionReceipt, qw.EffectiveCell = admission.Receipt, admission.EffectiveCell
	qw.ExecutionRuntimeBinding, qw.OperationalPayload = admission.RuntimeBinding, admission.OperationalPayload
	qw.HostAdaptationReceipt = admission.HostPreflight
	return qw
}

func TestLocalTransportPreparedAndActualMCPMatch(t *testing.T) {
	t.Parallel()
	for _, explicit := range []bool{false, true} {
		t.Run(map[bool]string{false: "no_explicit_server", true: "explicit_server_required"}[explicit], func(t *testing.T) {
			t.Parallel()
			reg, config, request := localAdmissionFixture(t)
			if explicit {
				request.Work.McpServers = []agent.MCPServerConfig{{Name: "authored-tools", Command: "authored-mcp"}}
			}
			output := admitLocalFixture(t, reg, config, request)
			qw := localTransportWork(t, output)
			admission, err := reg.PreflightHarness(qw)
			if err != nil {
				t.Fatal(err)
			}
			spec, names, err := buildPreparedSourceSpec(qw, admission.selection, nil)
			if err != nil {
				t.Fatal(err)
			}
			if len(names) != 0 {
				t.Fatalf("local plan has implicit MCP servers: %v", names)
			}
			host, err := executioncell.DecodeHostAdaptationReceipt(output.HostPreflight)
			if err != nil {
				t.Fatal(err)
			}
			var plan agent.PreparedHarness
			if err = json.Unmarshal(host.Plan, &plan); err != nil {
				t.Fatal(err)
			}
			spec.PreparedHarness = &plan
			provider := admission.selection.Provider.(agent.HarnessProvider)
			// Repeat the execution lane's real MCP derivation, without spawning a model.
			qw.runtimeTransport = admission.selection.runtimeTransport
			spec.MCPServers = mergeMCPServers(defaultMCPServersForHarness(qw, "/runtime/worktree", admission.selection.Provider, agent.PromptModeAutonomous), qw.McpServers)
			if _, err = agent.ApplyPreparedHarness(spec, provider.Manifest()); err != nil {
				t.Fatalf("actual local MCP set disagrees with host plan: %v", err)
			}
			if explicit {
				if len(spec.MCPServers) != 1 || spec.MCPServers[0].Name != "authored-tools" {
					t.Fatal("explicit MCP server lost")
				}
				spec.MCPServers = nil
				if _, err = agent.ApplyPreparedHarness(spec, provider.Manifest()); err == nil {
					t.Fatal("missing explicitly required MCP server accepted")
				}
			}
			remote := NewRegistry()
			if err = remote.Register(admission.selection.Provider); err != nil {
				t.Fatal(err)
			}
			// Receipt preflight checks linkage, not materialization. The real
			// run-loop control below proves wrong-mode refusal before Spawn.
			if _, err = remote.PreflightHarness(qw); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestRuntimeTransportIsTrustedRegistryConfiguration(t *testing.T) {
	t.Parallel()
	if _, err := NewRegistryWithOptions(RegistryOptions{RuntimeTransportMode: "unknown"}); err == nil {
		t.Fatal("unknown mode accepted")
	}
	reg, config, request := localAdmissionFixture(t)
	remote := NewRegistry()
	provider, err := reg.Resolve(agent.ProviderCodex)
	if err != nil {
		t.Fatal(err)
	}
	if err = remote.Register(provider); err != nil {
		t.Fatal(err)
	}
	if _, err = NewLocalAdmissionProducer(remote, config); err == nil {
		t.Fatal("default controller registry minted local evidence")
	}
	for _, bearer := range []string{"worker", "mcp"} {
		qw := request.Work
		qw.ResolvedProfile.Harness = "codex"
		if bearer == "worker" {
			qw.AuthToken = "synthetic"
		} else {
			qw.McpAuthToken = "synthetic"
		}
		if _, err = reg.PreflightHarness(qw); err == nil {
			t.Fatal("local mode consumed controller credentials")
		}
	}
	var qw QueuedWork
	if err = json.Unmarshal([]byte(`{"runtimeTransport":"local","RuntimeTransportMode":"local"}`), &qw); err != nil {
		t.Fatal(err)
	}
	if qw.runtimeTransport != "" {
		t.Fatal("wire selected local mode")
	}
	beforeMode, err := DigestOperationalPayload(qw)
	if err != nil {
		t.Fatal(err)
	}
	qw.runtimeTransport = RuntimeTransportLocal
	afterMode, err := DigestOperationalPayload(qw)
	if err != nil {
		t.Fatal(err)
	}
	if beforeMode != afterMode {
		t.Fatal("process transport altered operational payload authority")
	}
	raw, err := json.Marshal(qw)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(raw, []byte("runtimeTransport")) || bytes.Contains(raw, []byte("RuntimeTransport")) {
		t.Fatal("transport policy serialized")
	}
	output := admitLocalFixture(t, reg, config, request)
	if !bytes.Contains(output.Evidence, []byte(`"RuntimeTransportMode":"local"`)) {
		t.Fatal("trusted mode missing from bound configuration")
	}
}

func TestControllerTransportKeepsImplicitGateway(t *testing.T) {
	t.Parallel()
	reg, _, _ := localAdmissionFixture(t)
	provider, err := reg.Resolve(agent.ProviderCodex)
	if err != nil {
		t.Fatal(err)
	}
	qw := QueuedWork{PlatformURL: "https://controller.example", AuthToken: "synthetic"}
	qw.SessionID = "session"
	actual := defaultMCPServersForHarness(qw, "/runtime/worktree", provider, agent.PromptModeAutonomous)
	prepared := defaultMCPServersForHarness(materializeRuntimeAuthority(qw), "/runtime/worktree", provider, agent.PromptModeAutonomous)
	if len(actual) != 1 || len(prepared) != 1 || actual[0].Name != prepared[0].Name {
		t.Fatal("controller implicit MCP behavior changed")
	}
	if !strings.Contains(actual[0].Name, "platform") {
		t.Fatal("expected controller gateway")
	}
}

// Intercept only execution: manifest, preparation and all other provider
// behavior remain the actual native implementation. No model process starts.
type localTransportProbeProvider struct {
	*codex.Provider
	calls atomic.Int32
}

var errLocalTransportProbe = errors.New("execution intercepted after materialization")

func (p *localTransportProbeProvider) Spawn(_ context.Context, spec agent.Spec) (agent.Handle, error) {
	p.calls.Add(1)
	if _, err := agent.ApplyPreparedHarness(spec, p.Manifest()); err != nil {
		return nil, err
	}
	return nil, errLocalTransportProbe
}

func TestLocalTransportRunLoopModeBoundary(t *testing.T) {
	t.Parallel()
	for _, mode := range []RuntimeTransportMode{RuntimeTransportLocal, RuntimeTransportController} {
		t.Run(string(mode), func(t *testing.T) {
			t.Parallel()
			local, config, request := localAdmissionFixture(t)
			output := admitLocalFixture(t, local, config, request)
			original, err := local.Resolve(agent.ProviderCodex)
			if err != nil {
				t.Fatal(err)
			}
			probe := &localTransportProbeProvider{Provider: original.(*codex.Provider)}
			reg, err := NewRegistryWithOptions(RegistryOptions{RuntimeTransportMode: mode})
			if err != nil {
				t.Fatal(err)
			}
			if err = reg.Register(probe); err != nil {
				t.Fatal(err)
			}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				t.Errorf("unexpected network before intercepted Spawn: %s", r.URL.Path)
				w.WriteHeader(http.StatusInternalServerError)
			}))
			t.Cleanup(server.Close)
			manager, err := worktree.NewManager(worktree.Options{ParentDir: t.TempDir()})
			if err != nil {
				t.Fatal(err)
			}
			poster, err := result.NewPoster(result.Options{PlatformURL: server.URL, WorkerID: config.WorkerID, AuthToken: "synthetic"})
			if err != nil {
				t.Fatal(err)
			}
			run, err := New(Options{Registry: reg, WorktreeManager: manager, Poster: poster, SkipBackstop: true, SkipSteering: true, SkipPostSession: true})
			if err != nil {
				t.Fatal(err)
			}
			qw := localTransportWork(t, output)
			qw.PlatformURL = server.URL
			_, err = run.runLoop(context.Background(), qw, run.now().UnixMilli(), nil)
			if mode == RuntimeTransportLocal {
				if !errors.Is(err, errLocalTransportProbe) || probe.calls.Load() != 1 {
					t.Fatalf("local plan failed actual execution materialization: calls=%d err=%v", probe.calls.Load(), err)
				}
			} else {
				var drift *agent.AuthorityDriftError
				if !errors.As(err, &drift) || probe.calls.Load() != 0 {
					t.Fatalf("wrong mode reached execution or lost typed refusal: calls=%d err=%v", probe.calls.Load(), err)
				}
				if !strings.Contains(strings.Join(drift.Fields, ","), "mcpServers") {
					t.Fatalf("wrong drift fields: %v", drift.Fields)
				}
			}
		})
	}
}

func TestLocalTransportProducerStoreRoundTrip(t *testing.T) {
	t.Parallel()
	registry, config, request := localAdmissionFixture(t)
	output := admitLocalFixture(t, registry, config, request)
	cell, err := executioncell.DecodeResolvedExecutionCell(output.EffectiveCell)
	if err != nil {
		t.Fatal(err)
	}
	binding := localqueue.ConfiguredHostBinding{ScopeID: config.ScopeID, HostID: config.PlacementID, WorkerID: config.WorkerID, Selectors: &executioncell.ExecutionSelectorRegistry{HarnessVersions: map[string][]string{cell.Harness.ID: {cell.Harness.Version}}, Models: []string{cell.Model.Author + "/" + cell.Model.ID}, Endpoints: []string{cell.Endpoint.ID}, AuthBindings: []string{cell.AuthBinding.ID}, Placements: []string{cell.Placement.ID}, Capabilities: []string{}}}
	root := filepath.Join(t.TempDir(), "queue")
	store, err := localqueue.Open(root, binding)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	env := localqueue.AdmissionEnvelope{ScopeID: output.ScopeID, Source: localqueue.GitHubSource{RepositoryID: 1234, OwnerRepo: "acme/repo", IssueNumber: 1, IssueURL: "https://github.com/acme/repo/issues/1"}, Session: output.Session, Intent: output.Intent, Receipt: output.Receipt, EffectiveCell: output.EffectiveCell, RuntimeBinding: output.RuntimeBinding, OperationalPayload: output.OperationalPayload, HostPreflight: output.HostPreflight, ProducerEvidence: output.Evidence, InitialEvent: localqueue.LifecycleEvent{Type: localqueue.EventSessionAdmitted, ScopeID: output.ScopeID, SessionID: output.Session.SessionID, ReceiptID: output.Session.AdmissionReceiptID, RecordedAt: time.Now().UTC().Format(time.RFC3339Nano)}, Dispatch: localqueue.DispatchOutbox{State: localqueue.DispatchPending}, CredentialHandle: "cred_fixture"}
	if _, err = store.Admit(context.Background(), store.CurrentRevision(), env); err != nil {
		t.Fatal(err)
	}
	if err = store.Close(); err != nil {
		t.Fatal(err)
	}
	store, err = localqueue.Open(root, binding)
	if err != nil {
		t.Fatal(err)
	}
	projection, err := store.Session(context.Background(), output.ScopeID, output.Session.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	restored := projection.Admission.Envelope
	for name, pair := range map[string][2][]byte{"host": {output.HostPreflight, restored.HostPreflight}, "evidence": {output.Evidence, restored.ProducerEvidence}, "payload": {output.OperationalPayload, restored.OperationalPayload}} {
		if !bytes.Equal(pair[0], pair[1]) {
			t.Fatalf("%s changed across journal", name)
		}
	}
	if _, err = registry.PreflightHarness(localTransportWork(t, &LocalAdmission{Receipt: restored.Receipt, EffectiveCell: restored.EffectiveCell, RuntimeBinding: restored.RuntimeBinding, OperationalPayload: restored.OperationalPayload, HostPreflight: restored.HostPreflight})); err != nil {
		t.Fatal(err)
	}
}
