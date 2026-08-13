package pi

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"

	"github.com/RenseiAI/donmai/agent"
)

// This file is the scripted/unit half of the D8 fixture family for
// ADR-2026-08-12's additional-extension delivery seam (D1/D2). It proves the
// Go-side mechanics — materialization, TOCTOU-closing digest verification,
// argv ordering, and fail-closed denial — without needing the real pi
// binary. The real-binary half (tool registration through a live process,
// the headless-UI guarantee, workspace-discovery staying disabled, and the
// operator-injected trust bypass) lives in
// extension_delivery_real_binary_test.go.

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

// TestSpawn_Interactive_AdditionalExtensionsDeniesBeforePTYWork is the
// negative fixture: pi's interactive PTY profile still declares
// ToolPluginDelivery: Unsupported (no fixture proves tool registration
// through that lane), so a spec combining Spec.Interactive with a populated
// AdditionalExtensions list must deny inside prepare() — before
// spawnInteractive, before any PTY driver work — exactly as
// TestSpawn_RequiredExtensionDeliveryDenialFailsBeforeProcessSpawn proves for
// a digest failure on the headless lane.
func TestSpawn_Interactive_AdditionalExtensionsDeniesBeforePTYWork(t *testing.T) {
	t.Parallel()
	p, err := New(Options{skipProcess: true, handshakeToken: testHandshakeToken})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	body := []byte("export default function activate(pi) {}\n")
	spec := agent.Spec{
		Cwd:         t.TempDir(),
		Interactive: &agent.InteractiveSpec{Cols: 80, Rows: 24},
		AdditionalExtensions: []agent.ExtensionDelivery{
			{ID: "pack-1", Kind: agent.ExtensionDeliveryInline, Source: body, Basename: "pack.ts", Digest: sha256Hex(body), Required: true},
		},
	}
	if _, err := p.Spawn(context.Background(), spec); err == nil {
		t.Fatal("Spawn succeeded on the interactive lane with AdditionalExtensions set; want a fail-closed denial (interactive ToolPluginDelivery is Unsupported)")
	}
}
