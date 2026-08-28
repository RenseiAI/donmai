package pi

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/RenseiAI/donmai/agent"
)

// This file is the materialization/admission half of the D8 fixture family for
// ADR-2026-08-12's additional-extension delivery seam (D1/D2). It proves the
// Go-side mechanics — materialization, TOCTOU-closing digest verification,
// argv ordering, and fail-closed denial. The interactive fixture at the end
// adds independent evidence through a real bare-PTY pi process; the existing
// headless real-binary fixtures remain in extension_delivery_real_binary_test.go.

func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// TestMaterializeAdditionalExtensions_PathDelivery proves a path-kind
// delivery is read back from the caller-supplied absolute path and verified
// against its digest — never written to disk by this package.
func TestMaterializeAdditionalExtensions_PathDelivery(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	srcPath := filepath.Join(dir, "pack.ts")
	body := []byte("export default function activate(pi) {}\n")
	if err := os.WriteFile(srcPath, body, 0o600); err != nil {
		t.Fatal(err)
	}

	layout := newSessionLayout(t.TempDir())
	paths, err := materializeAdditionalExtensions(layout, []agent.ExtensionDelivery{
		{ID: "pack-1", Kind: agent.ExtensionDeliveryPath, Path: srcPath, Digest: sha256Hex(body), Required: true},
	})
	if err != nil {
		t.Fatalf("materializeAdditionalExtensions: %v", err)
	}
	if len(paths) != 1 || paths[0] != srcPath {
		t.Fatalf("paths = %v, want [%q] (path deliveries load from their own path, unmoved)", paths, srcPath)
	}
}

// TestMaterializeAdditionalExtensions_InlineDelivery proves an inline-kind
// delivery is written under layout.injected (never layout.root itself, so it
// can never collide with extensionFileName) and verified after the write.
func TestMaterializeAdditionalExtensions_InlineDelivery(t *testing.T) {
	t.Parallel()
	layout := newSessionLayout(t.TempDir())
	body := []byte("export default function activate(pi) { /* inline pack */ }\n")

	paths, err := materializeAdditionalExtensions(layout, []agent.ExtensionDelivery{
		{ID: "pack-1", Kind: agent.ExtensionDeliveryInline, Source: body, Basename: "pack.ts", Digest: sha256Hex(body), Required: true},
	})
	if err != nil {
		t.Fatalf("materializeAdditionalExtensions: %v", err)
	}
	if len(paths) != 1 {
		t.Fatalf("paths = %v, want exactly one entry", paths)
	}
	if filepath.Dir(paths[0]) != layout.injected {
		t.Errorf("inline delivery materialized at %q, want it under layout.injected %q", paths[0], layout.injected)
	}
	got, err := os.ReadFile(paths[0])
	if err != nil {
		t.Fatalf("read materialized inline delivery: %v", err)
	}
	if string(got) != string(body) {
		t.Errorf("materialized inline delivery differs from Source")
	}
}

// TestMaterializeAdditionalExtensions_DigestMismatchFailsClosed pins D1.2/
// D2(b): a delivery whose digest does not match what actually landed on disk
// denies — no path is returned, and the error names the delivery. This holds
// for both delivery kinds: verification always reads back from disk, never
// trusts Source or the caller's path claim.
func TestMaterializeAdditionalExtensions_DigestMismatchFailsClosed(t *testing.T) {
	t.Parallel()

	t.Run("inline", func(t *testing.T) {
		t.Parallel()
		layout := newSessionLayout(t.TempDir())
		_, err := materializeAdditionalExtensions(layout, []agent.ExtensionDelivery{
			{ID: "tampered", Kind: agent.ExtensionDeliveryInline, Source: []byte("actual bytes"), Basename: "pack.ts", Digest: sha256Hex([]byte("claimed bytes")), Required: true},
		})
		if err == nil {
			t.Fatal("materializeAdditionalExtensions succeeded on a digest mismatch; want a fail-closed error")
		}
	})

	t.Run("path", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		srcPath := filepath.Join(dir, "pack.ts")
		if err := os.WriteFile(srcPath, []byte("actual bytes"), 0o600); err != nil {
			t.Fatal(err)
		}
		layout := newSessionLayout(t.TempDir())
		_, err := materializeAdditionalExtensions(layout, []agent.ExtensionDelivery{
			{ID: "tampered", Kind: agent.ExtensionDeliveryPath, Path: srcPath, Digest: sha256Hex([]byte("claimed bytes")), Required: true},
		})
		if err == nil {
			t.Fatal("materializeAdditionalExtensions succeeded on a digest mismatch; want a fail-closed error")
		}
	})
}

// TestMaterializeAdditionalExtensions_MissingPathFailsClosed pins D1.2's "no
// warn-and-strip path": a path delivery pointing at a file that does not
// exist denies rather than being silently skipped.
func TestMaterializeAdditionalExtensions_MissingPathFailsClosed(t *testing.T) {
	t.Parallel()
	layout := newSessionLayout(t.TempDir())
	_, err := materializeAdditionalExtensions(layout, []agent.ExtensionDelivery{
		{ID: "missing", Kind: agent.ExtensionDeliveryPath, Path: filepath.Join(t.TempDir(), "does-not-exist.ts"), Digest: sha256Hex([]byte("x")), Required: true},
	})
	if err == nil {
		t.Fatal("materializeAdditionalExtensions succeeded against a nonexistent path; want a fail-closed error")
	}
}

// TestMaterializeAdditionalExtensions_MalformedDeliveryFailsClosedBeforeIO
// pins that structural validation (agent.ValidateExtensionDeliveries) runs
// BEFORE any materialization: a batch containing one malformed entry denies
// the WHOLE batch, and nothing is written for any entry in it — including
// entries that would otherwise have been well-formed (D1.2: a required
// delivery that cannot be verified must not leave a session having silently
// received a partial set).
func TestMaterializeAdditionalExtensions_MalformedDeliveryFailsClosedBeforeIO(t *testing.T) {
	t.Parallel()
	layout := newSessionLayout(t.TempDir())
	goodBody := []byte("good")
	_, err := materializeAdditionalExtensions(layout, []agent.ExtensionDelivery{
		{ID: "good", Kind: agent.ExtensionDeliveryInline, Source: goodBody, Basename: "good.ts", Digest: sha256Hex(goodBody), Required: true},
		{ID: "bad", Kind: agent.ExtensionDeliveryInline, Source: []byte("x"), Basename: "bad.ts", Digest: "not-a-sha256"},
	})
	if err == nil {
		t.Fatal("materializeAdditionalExtensions accepted a malformed digest; want a fail-closed error")
	}
	entries, readErr := os.ReadDir(layout.injected)
	if readErr == nil && len(entries) != 0 {
		t.Errorf("materialization ran ahead of validation: %d file(s) written under %q despite the batch being denied", len(entries), layout.injected)
	}
}

// TestMaterializeAdditionalExtensions_Empty proves the no-op path: an empty
// or nil AdditionalExtensions list is not an error and returns no paths, so
// the boundary extension remains the only `-e` entry for every existing
// caller (additive, wire-compatible).
func TestMaterializeAdditionalExtensions_Empty(t *testing.T) {
	t.Parallel()
	layout := newSessionLayout(t.TempDir())
	paths, err := materializeAdditionalExtensions(layout, nil)
	if err != nil {
		t.Fatalf("materializeAdditionalExtensions(nil) = %v, want nil error", err)
	}
	if len(paths) != 0 {
		t.Errorf("materializeAdditionalExtensions(nil) = %v, want no paths", paths)
	}
}

// TestRPCArgs_BoundaryFirstThenAdditionalExtensionsInOrder pins D1: the
// boundary extension always loads first via `-e`, followed by every
// additional delivery in the caller's declared order, all before
// `--no-extensions` — so nothing else is ever discoverable.
func TestRPCArgs_BoundaryFirstThenAdditionalExtensionsInOrder(t *testing.T) {
	t.Parallel()
	layout := sessionLayout{root: "/session", extension: "/session/policy.ts"}
	extensionPaths := []string{layout.extension, "/session/extensions-injected/a-pack.ts", "/session/extensions-injected/b-pack.ts"}

	args := rpcArgs(layout, extensionPaths, launchPrompt, "", agent.Spec{})

	wantPrefix := []string{
		"--mode", "rpc",
		"-e", "/session/policy.ts",
		"-e", "/session/extensions-injected/a-pack.ts",
		"-e", "/session/extensions-injected/b-pack.ts",
		"--no-extensions",
	}
	if len(args) < len(wantPrefix) {
		t.Fatalf("rpcArgs = %v, too short to contain the expected prefix %v", args, wantPrefix)
	}
	for i, want := range wantPrefix {
		if args[i] != want {
			t.Fatalf("rpcArgs[%d] = %q, want %q (full argv: %v)", i, args[i], want, args)
		}
	}
}

// TestInteractiveArgs_BoundaryFirstThenAdditionalExtensionsInOrder is the PTY
// lane's counterpart: ADR-2026-08-12 D1 is host-agnostic across spawn modes.
func TestInteractiveArgs_BoundaryFirstThenAdditionalExtensionsInOrder(t *testing.T) {
	t.Parallel()
	layout := sessionLayout{root: "/session", extension: "/session/policy.ts"}
	extensionPaths := []string{layout.extension, "/session/extensions-injected/a-pack.ts"}

	args := interactiveArgs(agent.Spec{}, layout, extensionPaths)

	wantPrefix := []string{
		"-e", "/session/policy.ts",
		"-e", "/session/extensions-injected/a-pack.ts",
		"--no-extensions",
	}
	for i, want := range wantPrefix {
		if args[i] != want {
			t.Fatalf("interactiveArgs[%d] = %q, want %q (full argv: %v)", i, args[i], want, args)
		}
	}
}

// TestInteractiveArgs_NoAdditionalExtensionKeepsBoundaryOnly pins the default
// path: the matrix flip must not introduce a synthetic delivery or change a
// session that carries no AdditionalExtensions. The embedded policy boundary
// remains the sole explicit -e entry and all ambient discovery stays disabled.
func TestInteractiveArgs_NoAdditionalExtensionKeepsBoundaryOnly(t *testing.T) {
	t.Parallel()
	layout := sessionLayout{root: "/session", extension: "/session/policy.ts"}
	args := interactiveArgs(agent.Spec{}, layout, []string{layout.extension})

	var extensionPaths []string
	for i, arg := range args {
		if arg == "-e" && i+1 < len(args) {
			extensionPaths = append(extensionPaths, args[i+1])
		}
	}
	if len(extensionPaths) != 1 || extensionPaths[0] != layout.extension {
		t.Fatalf("interactive explicit extensions = %v, want only the embedded boundary %q", extensionPaths, layout.extension)
	}
	if !strings.Contains(strings.Join(args, "\x00"), "--no-extensions") {
		t.Fatalf("interactive argv lost --no-extensions: %v", args)
	}
}

// TestSpawn_RequiredExtensionDeliveryDenialFailsBeforeProcessSpawn is the
// integration-level proof that launch() wires materializeAdditionalExtensions
// in ahead of spawnChild: a required delivery that fails verification denies
// Spawn itself, before skipProcess's scripted pipe (standing in for the real
// child) is ever touched — no process, no credential delivery, no argv ever
// gets a chance to reference the unverified artifact.
func TestSpawn_RequiredExtensionDeliveryDenialFailsBeforeProcessSpawn(t *testing.T) {
	t.Parallel()
	p, err := New(Options{skipProcess: true, handshakeToken: testHandshakeToken})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	spec := agent.Spec{
		Cwd: t.TempDir(),
		AdditionalExtensions: []agent.ExtensionDelivery{
			{ID: "bad-pack", Kind: agent.ExtensionDeliveryInline, Source: []byte("actual"), Basename: "pack.ts", Digest: sha256Hex([]byte("claimed")), Required: true},
		},
	}
	if _, err := p.Spawn(context.Background(), spec); err == nil {
		t.Fatal("Spawn succeeded with an unverifiable required extension delivery; want a fail-closed denial")
	}
}

// TestSpawn_ValidAdditionalExtension_HandshakeStillVerifiesNormally proves the
// positive path integrates cleanly: a spec carrying a well-formed additional
// extension delivery still completes the boundary handshake exactly as a
// spec with none does — the seam does not perturb the existing trust
// boundary (D2.1: a pack never substitutes for the boundary, it only rides
// alongside it).
func TestSpawn_ValidAdditionalExtension_HandshakeStillVerifiesNormally(t *testing.T) {
	t.Parallel()
	body := []byte("export default function activate(pi) {}\n")
	spec := agent.Spec{
		AdditionalExtensions: []agent.ExtensionDelivery{
			{ID: "pack-1", Kind: agent.ExtensionDeliveryInline, Source: body, Basename: "pack.ts", Digest: sha256Hex(body), Required: true},
		},
	}
	body2 := getStateResponse("ses_extra") +
		event(map[string]any{"type": "agent_start"}) +
		event(map[string]any{"type": "agent_settled"})
	_, h, err := spawnScripted(t, spec, handshakeEvent("h1"), body2)
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	drain(t, h)
}

// --- AdditionalExtensions routed through the generic tool-lifecycle plan
// (ADR-2026-08-12 D6, ADR-2026-08-06 D1.1) ---
//
// The tests above prove the Go-side materialization/argv/trust mechanics.
// These two prove the OTHER half: the SAME Spec.AdditionalExtensions list
// also goes through agent.PrepareHarness/PrepareToolLifecycle — the generic,
// cross-harness-compiled tool_plugin channel every other harness's tool/MCP/
// policy Spec fields already use — before it ever reaches this package's own
// materializer, so pi's Caps.SupportsToolPlugins:true is backed by a plan
// that describes the delivery and a receipt that attests it, not just a
// manifest literal.

// TestSpawn_AdditionalExtensions_ProducesToolPluginReceiptEntry is the
// positive fixture: a real (scripted) Spawn call carrying a well-formed
// AdditionalExtensions list produces a ToolLifecycleReceipt naming
// ToolChannelToolPlugin, admitted, with the ToolDeliveryPiAdditionalExtension
// boundary the headless manifest profile declares — observed through
// Spec.OnToolLifecycleAdapted, the same hook every harness's receipt rides.
func TestSpawn_AdditionalExtensions_ProducesToolPluginReceiptEntry(t *testing.T) {
	t.Parallel()
	body := []byte("export default function activate(pi) {}\n")
	var receipt agent.ToolLifecycleReceipt
	spec := agent.Spec{
		AdditionalExtensions: []agent.ExtensionDelivery{
			{ID: "pack-1", Kind: agent.ExtensionDeliveryInline, Source: body, Basename: "pack.ts", Digest: sha256Hex(body), Required: true},
		},
		OnToolLifecycleAdapted: func(r agent.ToolLifecycleReceipt) error {
			receipt = r
			return nil
		},
	}
	body2 := getStateResponse("ses_receipt") +
		event(map[string]any{"type": "agent_start"}) +
		event(map[string]any{"type": "agent_settled"})
	_, h, err := spawnScripted(t, spec, handshakeEvent("h1"), body2)
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	drain(t, h)

	if receipt.Decision != "ready" {
		t.Fatalf("receipt decision = %q, want ready; receipt=%+v", receipt.Decision, receipt)
	}
	var found bool
	for _, entry := range receipt.Entries {
		if entry.Channel != agent.ToolChannelToolPlugin {
			continue
		}
		found = true
		if entry.Outcome != agent.ToolOutcomeAdmitted {
			t.Errorf("tool_plugin entry outcome = %q, want admitted", entry.Outcome)
		}
		if entry.Delivery != agent.ToolDeliveryPiAdditionalExtension {
			t.Errorf("tool_plugin entry delivery = %q, want %q", entry.Delivery, agent.ToolDeliveryPiAdditionalExtension)
		}
	}
	if !found {
		t.Fatalf("receipt must name the tool_plugin channel; entries=%+v", receipt.Entries)
	}
}

// TestSpawn_Interactive_InvalidAdditionalExtensionsFailBeforePTY keeps the
// pre-spawn trust boundary after interactive delivery becomes supported.
// Malformed and duplicate entries both deny before the sentinel PTY child can
// write its started marker; support never means load malformed input.
func TestSpawn_Interactive_InvalidAdditionalExtensionsFailBeforePTY(t *testing.T) {
	body := []byte("export default function activate(pi) {}\n")
	valid := agent.ExtensionDelivery{
		ID: "pack-1", Kind: agent.ExtensionDeliveryInline, Source: body,
		Basename: "pack.ts", Digest: sha256Hex(body), Required: true,
	}
	for _, tc := range []struct {
		name       string
		deliveries []agent.ExtensionDelivery
		wantError  string
	}{
		{name: "malformed", deliveries: []agent.ExtensionDelivery{{ID: "bad", Kind: agent.ExtensionDeliveryInline, Source: body, Basename: "bad.ts", Digest: "not-a-sha256", Required: true}}, wantError: "digest"},
		{name: "duplicate", deliveries: []agent.ExtensionDelivery{valid, valid}, wantError: "duplicate id"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			workdir := t.TempDir()
			p := newFakeInteractivePiProvider(t, `printf started > "$PWD/pty-started"`)
			_, err := p.Spawn(context.Background(), agent.Spec{
				Cwd:                  workdir,
				Interactive:          &agent.InteractiveSpec{Cols: 80, Rows: 24},
				AdditionalExtensions: tc.deliveries,
			})
			if err == nil || !strings.Contains(err.Error(), tc.wantError) {
				t.Fatalf("Spawn error = %v, want fail-closed %q denial", err, tc.wantError)
			}
			if _, statErr := os.Stat(filepath.Join(workdir, "pty-started")); !os.IsNotExist(statErr) {
				t.Fatalf("PTY child ran despite %s extension denial (stat err=%v)", tc.name, statErr)
			}
		})
	}
}

const interactiveConformanceExtension = `
import type { ExtensionAPI } from "@earendil-works/pi-coding-agent";
import { writeFileSync } from "node:fs";
import { Type } from "typebox";

const markerPath = process.env.DONMAI_INTERACTIVE_EXTENSION_MARKER ?? "";
const state = {
  loaded: false,
  toolExecuted: false,
  inheritedProviderSecret: false,
  inheritedRunnerControl: false,
  sessionKeyPresent: false,
};

function persist() {
  if (markerPath) writeFileSync(markerPath, JSON.stringify(state), { mode: 0o600 });
}

export default function activate(pi: ExtensionAPI) {
  pi.registerTool({
    name: "donmai_fixture_tool",
    label: "Donmai Fixture Tool",
    description: "Safe no-input conformance receipt.",
    parameters: Type.Object({}),
    async execute() {
      state.toolExecuted = true;
      persist();
      return { content: [{ type: "text", text: "fixture-tool-ok" }] };
    },
  });

  pi.on("session_start", () => {
    state.loaded = true;
    state.inheritedProviderSecret = process.env.ANTHROPIC_API_KEY !== undefined || process.env.OPENAI_API_KEY === "parent-secret-must-not-reach-pi";
    state.inheritedRunnerControl = process.env.ATTACH_TOKEN !== undefined;
    state.sessionKeyPresent = process.env.DONMAI_PI_KEY !== undefined;
    persist();
  });
}
`

type interactiveConformanceMarker struct {
	Loaded                  bool `json:"loaded"`
	ToolExecuted            bool `json:"toolExecuted"`
	InheritedProviderSecret bool `json:"inheritedProviderSecret"`
	InheritedRunnerControl  bool `json:"inheritedRunnerControl"`
	SessionKeyPresent       bool `json:"sessionKeyPresent"`
}

type interactiveToolStub struct {
	server *httptest.Server

	mu              sync.Mutex
	requestCount    int
	toolNames       []string
	fixtureResults  int
	authorizations  []string
	fixtureReceived chan struct{}
	fixtureOnce     sync.Once
}

func newInteractiveToolStub(t *testing.T) *interactiveToolStub {
	t.Helper()
	s := &interactiveToolStub{fixtureReceived: make(chan struct{})}
	s.server = httptest.NewServer(http.HandlerFunc(s.handle))
	t.Cleanup(s.server.Close)
	return s
}

func (s *interactiveToolStub) baseURL() string { return s.server.URL + "/v1" }

func (s *interactiveToolStub) handle(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/v1/chat/completions" {
		http.Error(w, "unsupported test route", http.StatusNotFound)
		return
	}
	var body struct {
		Messages []struct {
			Role    string `json:"role"`
			Content any    `json:"content"`
		} `json:"messages"`
		Tools []struct {
			Function struct {
				Name string `json:"name"`
			} `json:"function"`
		} `json:"tools"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "invalid JSON", http.StatusBadRequest)
		return
	}

	s.mu.Lock()
	s.requestCount++
	requestNumber := s.requestCount
	s.authorizations = append(s.authorizations, r.Header.Get("Authorization"))
	if requestNumber == 1 {
		for _, tool := range body.Tools {
			s.toolNames = append(s.toolNames, tool.Function.Name)
		}
	}
	for _, message := range body.Messages {
		if message.Role != "tool" {
			continue
		}
		encoded, _ := json.Marshal(message.Content)
		if strings.Contains(string(encoded), "fixture-tool-ok") {
			s.fixtureResults++
			s.fixtureOnce.Do(func() { close(s.fixtureReceived) })
		}
	}
	s.mu.Unlock()

	w.Header().Set("Content-Type", "text/event-stream")
	w.WriteHeader(http.StatusOK)
	if requestNumber == 1 {
		s.writeChunk(w, map[string]any{"choices": []map[string]any{{
			"index": 0,
			"delta": map[string]any{
				"role": "assistant",
				"tool_calls": []map[string]any{{
					"index": 0, "id": "call-interactive-conformance", "type": "function",
					"function": map[string]any{"name": "donmai_fixture_tool", "arguments": "{}"},
				}},
			},
			"finish_reason": "tool_calls",
		}}})
	} else {
		s.writeChunk(w, map[string]any{"choices": []map[string]any{{"index": 0, "delta": map[string]any{"role": "assistant", "content": "tool receipt observed"}, "finish_reason": "stop"}}})
	}
	_, _ = fmt.Fprint(w, "data: [DONE]\n\n")
}

func (s *interactiveToolStub) writeChunk(w http.ResponseWriter, payload map[string]any) {
	payload["id"] = "chatcmpl-interactive-extension-conformance"
	payload["object"] = "chat.completion.chunk"
	payload["created"] = time.Now().Unix()
	payload["model"] = realBinaryModel
	b, _ := json.Marshal(payload)
	_, _ = fmt.Fprintf(w, "data: %s\n\n", b)
	if flusher, ok := w.(http.Flusher); ok {
		flusher.Flush()
	}
}

func (s *interactiveToolStub) observations() (toolNames []string, fixtureResults int, authorizations []string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.toolNames...), s.fixtureResults, append([]string(nil), s.authorizations...)
}

// TestSpawn_Interactive_AdditionalExtensionRealBarePTYRegistersAndExecutesTool
// is the mode-specific D6 evidence. Provider.Spawn launches bare pi under the
// real PTY driver, the local model stub observes the exact registered tool
// names, requests one safe fixture tool call, and receives its result on the
// next model turn. The extension independently records load, execution, and
// env-boundary booleans from inside the real child.
func TestSpawn_Interactive_AdditionalExtensionRealBarePTYRegistersAndExecutesTool(t *testing.T) {
	realBinaryAvailable(t)
	if runtime.GOOS == "windows" {
		t.Skip("bare PTY conformance is unix-only")
	}
	t.Setenv("ANTHROPIC_API_KEY", "parent-secret-must-not-reach-pi")
	t.Setenv("OPENAI_API_KEY", "parent-secret-must-not-reach-pi")
	t.Setenv("ATTACH_TOKEN", "runner-control-must-not-reach-pi")

	stub := newInteractiveToolStub(t)
	workdir := t.TempDir()
	markerPath := filepath.Join(t.TempDir(), "interactive-marker.json")
	extension := []byte(interactiveConformanceExtension)
	var receipt agent.ToolLifecycleReceipt
	spec := realBinarySpec(workdir, "Invoke donmai_fixture_tool exactly once, then stop.", stub.baseURL())
	spec.Interactive = &agent.InteractiveSpec{Cols: 100, Rows: 30}
	spec.AllowedTools = []string{"donmai_fixture_tool"}
	spec.Env = map[string]string{"DONMAI_INTERACTIVE_EXTENSION_MARKER": markerPath}
	spec.AdditionalExtensions = []agent.ExtensionDelivery{{
		ID: "interactive-conformance", Kind: agent.ExtensionDeliveryInline,
		Source: extension, Basename: "interactive-conformance.ts",
		Digest: sha256Hex(extension), Required: true,
	}}
	spec.OnToolLifecycleAdapted = func(got agent.ToolLifecycleReceipt) error {
		receipt = got
		return nil
	}

	p, err := New(Options{})
	if err != nil {
		t.Fatalf("New(real pi): %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	h, err := p.Spawn(ctx, spec)
	if err != nil {
		t.Fatalf("Spawn(real bare PTY pi): %v", err)
	}
	t.Cleanup(func() { _ = h.Stop(context.Background()) })
	capable, ok := h.(agent.InteractiveCapable)
	if !ok || capable.InteractiveSession() == nil {
		t.Fatal("interactive Spawn did not return a live PTY session")
	}

	select {
	case <-stub.fixtureReceived:
	case <-time.After(30 * time.Second):
		screen, _, snapshotErr := capable.InteractiveSession().Snapshot()
		t.Fatalf("timed out waiting for the fixture tool result; snapshot=%+v snapshotErr=%v", screen, snapshotErr)
	}

	toolNames, fixtureResults, authorizations := stub.observations()
	counts := make(map[string]int, len(toolNames))
	for _, name := range toolNames {
		counts[name]++
	}
	for _, want := range []string{"read", "write", "edit", "bash", "donmai_fixture_tool"} {
		if counts[want] != 1 {
			t.Errorf("registered tool %q count = %d, want 1; all names=%v", want, counts[want], toolNames)
		}
	}
	if fixtureResults != 1 {
		t.Errorf("model-side fixture tool result receipts = %d, want exactly 1", fixtureResults)
	}
	for _, authorization := range authorizations {
		if authorization != "Bearer real-binary-stub-key" {
			t.Errorf("model request authorization = %q, want only the session binding credential", authorization)
		}
	}

	b, err := os.ReadFile(markerPath)
	if err != nil {
		t.Fatalf("read interactive extension marker: %v", err)
	}
	var marker interactiveConformanceMarker
	if err := json.Unmarshal(b, &marker); err != nil {
		t.Fatalf("decode interactive extension marker: %v (raw=%s)", err, b)
	}
	if !marker.Loaded || !marker.ToolExecuted {
		t.Errorf("interactive extension marker = %+v, want loaded and one executed tool", marker)
	}
	if marker.InheritedProviderSecret || marker.InheritedRunnerControl {
		t.Errorf("interactive child observed inherited host-only env: %+v", marker)
	}
	if !marker.SessionKeyPresent {
		t.Errorf("interactive child lost the explicit per-session model binding: %+v", marker)
	}

	if receipt.Decision != "ready" {
		t.Fatalf("adaptation receipt decision = %q, want ready; receipt=%+v", receipt.Decision, receipt)
	}
	wantDeliveries := map[agent.ToolLifecycleChannel]agent.ToolDeliveryKind{
		agent.ToolChannelToolPlugin:   agent.ToolDeliveryPiAdditionalExtension,
		agent.ToolChannelAllowedTools: agent.ToolDeliveryPiInteractiveLocalToolPolicy,
	}
	for channel, wantDelivery := range wantDeliveries {
		var found bool
		for _, entry := range receipt.Entries {
			if entry.Channel != channel {
				continue
			}
			found = true
			if entry.Outcome != agent.ToolOutcomeAdmitted || entry.Delivery != wantDelivery {
				t.Errorf("receipt %q entry = %+v, want admitted via %q", channel, entry, wantDelivery)
			}
		}
		if !found {
			t.Errorf("adaptation receipt omitted %q; entries=%+v", channel, receipt.Entries)
		}
	}

	if err := h.Stop(context.Background()); err != nil {
		t.Fatalf("Stop(real bare PTY pi): %v", err)
	}
	awaitPTYExit(t, h)
}
