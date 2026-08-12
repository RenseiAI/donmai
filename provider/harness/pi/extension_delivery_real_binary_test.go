package pi

// extension_delivery_real_binary_test.go — real `pi` binary conformance
// fixtures for the additional-extension delivery seam (ADR-2026-08-12,
// donmai-architecture). Mirrors real_binary_test.go's scope/CI-gating
// discipline (see that file's doc comment): gated on `pi`/`node` being on
// PATH, real evidence on a machine with pi installed, not currently part of
// donmai's hosted CI.
//
// These fixtures prove, against the REAL pinned binary, the properties the
// scripted tests in extension_delivery_test.go cannot reach because
// skipProcess never execs a real child:
//
//   - tool registration through the seam (both delivery kinds) actually
//     succeeds against the real extension loader — pi.registerTool() does
//     not throw and the session completes normally;
//   - the headless-UI guarantee (D3): a delivered extension's OWN attempted
//     UI round-trip — one the runner has no reason to recognize, since it
//     carries none of the boundary extension's marker — resolves promptly
//     ("cancelled" → undefined) rather than hanging, and the session still
//     reaches a terminal event;
//   - the trust rule (D2): a workspace-discovered extension never loads in
//     an autonomous session (--no-extensions), and an operator-injected one
//     loads via `-e` even when the run explicitly declines project trust.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/RenseiAI/donmai/agent"
)

func readTestdata(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatalf("read testdata/%s: %v", name, err)
	}
	return b
}

func sha256HexOf(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// fixtureMarker is what conformance-fixture.ts writes to DONMAI_FIXTURE_MARKER.
type fixtureMarker struct {
	Loaded             bool   `json:"loaded"`
	HasUI              *bool  `json:"hasUI"`
	ToolExecuted       bool   `json:"toolExecuted"`
	UIRoundTripSettled bool   `json:"uiRoundTripSettled"`
	UIRoundTripMs      int64  `json:"uiRoundTripMs"`
	UIReply            any    `json:"uiReply"`
	UIError            string `json:"uiError"`
}

func readFixtureMarker(t *testing.T, path string) fixtureMarker {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read fixture marker %s: %v — the delivered extension never ran session_start", path, err)
	}
	var m fixtureMarker
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("unmarshal fixture marker %s: %v (raw: %s)", path, err, b)
	}
	return m
}

// TestRealBinary_AdditionalExtension_ToolRegistersAndHeadlessUIRefusesPromptly
// is the primary conformance fixture: for both delivery kinds (path and
// inline), the extension delivered through Spec.AdditionalExtensions loads
// into the REAL pi process, its pi.registerTool() call succeeds (proven by
// the session completing with no ErrorEvent — a throwing registerTool call
// surfaces as an extension_error the Handle turns into a session abort), and
// its own attempted UI round-trip — carrying no donmai marker — resolves
// promptly with a refusal rather than hanging the session.
func TestRealBinary_AdditionalExtension_ToolRegistersAndHeadlessUIRefusesPromptly(t *testing.T) {
	realBinaryAvailable(t)
	fixture := readTestdata(t, "conformance-fixture.ts")
	digest := sha256HexOf(fixture)

	for _, tt := range []struct {
		name    string
		specify func(t *testing.T, base agent.Spec) agent.Spec
	}{
		{
			name: "path",
			specify: func(t *testing.T, base agent.Spec) agent.Spec {
				t.Helper()
				dir := t.TempDir()
				p := filepath.Join(dir, "conformance-fixture.ts")
				if err := os.WriteFile(p, fixture, 0o600); err != nil {
					t.Fatalf("write path-delivery fixture: %v", err)
				}
				base.AdditionalExtensions = []agent.ExtensionDelivery{
					{ID: "conformance-fixture", Kind: agent.ExtensionDeliveryPath, Path: p, Digest: digest, Required: true},
				}
				return base
			},
		},
		{
			name: "inline",
			specify: func(_ *testing.T, base agent.Spec) agent.Spec {
				base.AdditionalExtensions = []agent.ExtensionDelivery{
					{ID: "conformance-fixture", Kind: agent.ExtensionDeliveryInline, Source: fixture, Basename: "conformance-fixture-inline.ts", Digest: digest, Required: true},
				}
				return base
			},
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			stub := newRealBinaryStub(t, realBinaryModel)
			cwd := t.TempDir()
			markerPath := filepath.Join(t.TempDir(), "marker.json")

			spec := realBinarySpec(cwd, "reply with a short greeting and nothing else", stub.baseURL())
			spec.Env = map[string]string{fixtureMarkerEnvVar: markerPath}
			spec = tt.specify(t, spec)

			p, err := New(Options{HandshakeTimeout: 30 * time.Second})
			if err != nil {
				t.Fatalf("New: %v", err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
			defer cancel()
			h, err := p.Spawn(ctx, spec)
			if err != nil {
				t.Fatalf("Spawn: %v — a delivered extension must not break spawn", err)
			}
			t.Cleanup(func() { _ = h.Stop(context.Background()) })

			for _, ev := range drainToResult(t, h, 30*time.Second) {
				if e, ok := ev.(agent.ErrorEvent); ok {
					t.Fatalf("session ended with an ErrorEvent instead of completing: %+v — the delivered extension's registerTool call likely threw", e)
				}
			}

			m := readFixtureMarker(t, markerPath)
			if !m.Loaded {
				t.Fatal("fixture marker reports loaded=false — the delivered extension's session_start handler never ran")
			}
			if m.HasUI == nil || !*m.HasUI {
				t.Fatalf("fixture marker hasUI = %v; the headless RPC lane reports hasUI=true (pi's own documented behavior — see docs/extensions.md ctx.hasUI), so a nil/false reading means the probe never observed a real RPC context", m.HasUI)
			}
			if !m.UIRoundTripSettled {
				t.Fatal("fixture's UI round-trip never settled within the drained session — this is the exact hang D3 forbids: a UI call from an extension the runner does not recognize must resolve promptly (as a refusal), never hang")
			}
			if m.UIRoundTripMs > 10000 {
				t.Errorf("fixture's UI round-trip took %dms to settle; want prompt cancellation (typically single-digit ms), not something indistinguishable from a hang", m.UIRoundTripMs)
			}
			if m.UIError == "" && m.UIReply != nil {
				t.Errorf("fixture's UI round-trip resolved to a non-null reply (%v) with no error — an unrecognized extension's round-trip must come back refused (pi's own contract: input() resolves to undefined on cancellation), never as if it were answered", m.UIReply)
			}
		})
	}
}

// fixtureMarkerEnvVar is DONMAI_FIXTURE_MARKER — named as a constant
// so a typo cannot silently desync this file from testdata/conformance-fixture.ts.
const fixtureMarkerEnvVar = "DONMAI_FIXTURE_MARKER"

// TestRealBinary_WorkspaceDiscovery_StaysDisabled plants a workspace-local
// auto-discovered extension (<cwd>/.pi/extensions/canary.ts — the exact
// location docs/extensions.md names for project-local auto-discovery) and
// proves it never loads through a normal Provider.Spawn: `--no-extensions`
// disables that discovery source outright for an autonomous session, which
// is the other half of D2's trust rule (workspace-discovered extensions
// never bypass trust — they are not merely gated, they are disabled).
func TestRealBinary_WorkspaceDiscovery_StaysDisabled(t *testing.T) {
	realBinaryAvailable(t)
	canary := readTestdata(t, "workspace-discovery-canary.ts")

	stub := newRealBinaryStub(t, realBinaryModel)
	cwd := t.TempDir()
	canaryDir := filepath.Join(cwd, ".pi", "extensions")
	if err := os.MkdirAll(canaryDir, 0o750); err != nil {
		t.Fatalf("mkdir workspace extensions dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(canaryDir, "canary.ts"), canary, 0o600); err != nil {
		t.Fatalf("plant workspace canary: %v", err)
	}
	canaryMarker := filepath.Join(t.TempDir(), "canary-marker.json")

	spec := realBinarySpec(cwd, "reply with a short greeting and nothing else", stub.baseURL())
	spec.Env = map[string]string{"DONMAI_CANARY_MARKER": canaryMarker}

	p, err := New(Options{HandshakeTimeout: 30 * time.Second})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	h, err := p.Spawn(ctx, spec)
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	t.Cleanup(func() { _ = h.Stop(context.Background()) })
	for _, ev := range drainToResult(t, h, 30*time.Second) {
		if e, ok := ev.(agent.ErrorEvent); ok {
			t.Fatalf("session ended with an ErrorEvent: %+v", e)
		}
	}

	if _, err := os.Stat(canaryMarker); err == nil {
		t.Fatal("the workspace-discovered canary extension LOADED — --no-extensions failed to disable auto-discovery, which means the trust rule's disabled-not-merely-gated half is broken")
	} else if !os.IsNotExist(err) {
		t.Fatalf("stat canary marker: %v", err)
	}
}

// TestRealBinary_TrustBypass_OperatorInjectedExtensionLoadsWithoutApprove
// drives the real pi binary DIRECTLY (bypassing this package's own argv
// construction, which always passes --approve as belt-and-suspenders) to
// prove pi's own documented behavior the seam's trust rule depends on: an
// extension loaded by explicit `-e` path loads regardless of project trust —
// even when the run EXPLICITLY declines it with --no-approve. This is D2's
// "operator-injected extensions bypass project trust" claim, tested at its
// true source rather than assumed from our own default flag choice.
func TestRealBinary_TrustBypass_OperatorInjectedExtensionLoadsWithoutApprove(t *testing.T) {
	realBinaryAvailable(t)
	fixture := readTestdata(t, "conformance-fixture.ts")

	cwd := t.TempDir()
	extPath := filepath.Join(cwd, "injected.ts")
	if err := os.WriteFile(extPath, fixture, 0o600); err != nil {
		t.Fatalf("write injected extension: %v", err)
	}
	sessionDir := filepath.Join(cwd, ".pi-session")
	markerPath := filepath.Join(t.TempDir(), "trust-bypass-marker.json")

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	// nolint:gosec // G204: fixed argv plus test-owned temp paths.
	cmd := exec.CommandContext(ctx, "pi",
		"--mode", "rpc",
		"-e", extPath,
		"--no-extensions",
		"--no-approve", // explicitly DECLINE project trust — the operator-injected
		// path must load anyway; only workspace-discovered resources are gated on
		// this decision.
		"--session-dir", sessionDir,
		"--model", "x",
	)
	cmd.Dir = cwd
	cmd.Env = append(os.Environ(),
		"DONMAI_FIXTURE_MARKER="+markerPath,
		piOfflineEnvVar+"=1",
		piSkipVersionCheckEnvVar+"=1",
	)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatalf("stdin pipe: %v", err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatalf("start pi: %v", err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill() })

	// A single get_state round trip is enough to prove the extension loaded
	// (session_start already ran by the time pi answers anything); no model
	// turn is needed for this fixture.
	_, _ = stdin.Write([]byte(`{"type":"get_state"}` + "\n"))
	time.Sleep(2 * time.Second)
	_ = stdin.Close()
	_ = cmd.Process.Kill()
	_, _ = cmd.Process.Wait()

	m := readFixtureMarker(t, markerPath)
	if !m.Loaded {
		t.Fatal("operator-injected extension (-e, --no-approve) never ran session_start — the trust bypass D2 depends on did not hold against the real binary")
	}
}
