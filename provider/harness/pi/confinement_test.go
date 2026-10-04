package pi

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/RenseiAI/donmai/agent"
)

// TestSessionStateRoot_SessionWorkarea pins the state placement: pi's own
// state lives inside the session's own workarea, keyed by the worktree leaf
// — never by the display name, which concurrent sessions share — so two
// sessions with the same display name resolve distinct roots, and the
// worktree lifecycle that removes the workarea removes the state with it.
//
// RED: key by SessionName and the same-name check fails; place beside the
// workdir and the cleanup-ownership check fails.
func TestSessionStateRoot_SessionWorkarea(t *testing.T) {
	cwd := filepath.Join(t.TempDir(), "repo")
	spec := agent.Spec{Cwd: cwd, SessionName: "s1"}
	root := sessionStateRoot(spec)
	if dir := filepath.Dir(filepath.Clean(root)); dir != filepath.Clean(cwd) {
		t.Errorf("sessionStateRoot parent = %q, want the session workarea %q", dir, cwd)
	}
	if base := filepath.Base(filepath.Clean(root)); !strings.HasPrefix(base, ".pi-") {
		t.Errorf("sessionStateRoot base = %q, want a .pi-<key> directory", base)
	}
	// Same display name, different workareas: distinct roots.
	other := agent.Spec{Cwd: filepath.Join(t.TempDir(), "repo"), SessionName: "s1"}
	if sessionStateRoot(other) == root {
		t.Errorf("sessions sharing a display name share state root %q", root)
	}
	// Same workarea, resumed: same root.
	resumed := agent.Spec{Cwd: cwd, SessionName: "s1"}
	if got := sessionStateRoot(resumed); got != root {
		t.Errorf("resumed sessionStateRoot = %q, want %q", got, root)
	}
	layout := newSessionLayoutForSpec(spec)
	if layout.root != root {
		t.Errorf("layout root = %q, want the session state root %q", layout.root, root)
	}
	for _, p := range []string{layout.extension, layout.injected, layout.agentHome} {
		if inside, err := isInside(p, root); err != nil || !inside {
			t.Errorf("layout path %q is not under the state root %q", p, root)
		}
		if inside, err := isInside(p, cwd); err != nil || !inside {
			t.Errorf("layout path %q is not under the session workarea %q", p, cwd)
		}
	}
	if layout.agentHome == layout.root {
		t.Errorf("agent home %q collides with the session root: pi's resume lookup needs them apart", layout.agentHome)
	}
}

func isInside(path, dir string) (bool, error) {
	rel, err := filepath.Rel(filepath.Clean(dir), filepath.Clean(path))
	if err != nil {
		return false, err
	}
	return rel != "." && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)), nil
}

// TestMaterializeExtensionForSpec_LivesOutsideCwd pins the materialization
// move end to end: the boundary extension lands outside the working folder
// with the same bytes and mode as before, and nothing pi-owned is left
// behind in the checkout.
func TestMaterializeExtensionForSpec_SessionWorkarea(t *testing.T) {
	cwd := t.TempDir()
	spec := agent.Spec{Cwd: cwd, SessionName: "s1"}
	layout, err := materializeExtensionForSpec(spec)
	if err != nil {
		t.Fatalf("materializeExtensionForSpec: %v", err)
	}
	if inside, _ := isInside(layout.extension, cwd); !inside {
		t.Errorf("materialized extension %q is not under the session workarea", layout.extension)
	}
	data, err := os.ReadFile(layout.extension)
	if err != nil {
		t.Fatalf("read extension: %v", err)
	}
	if string(data) != string(extensionSource()) {
		t.Errorf("materialized extension differs from embedded source")
	}
	assertMode0600(t, layout.extension)
	// Teardown removes the whole workarea, so the state root needs no
	// separate sweep: it must sit under the directory teardown removes.
	if dir := filepath.Dir(filepath.Clean(layout.root)); dir != filepath.Clean(cwd) {
		t.Errorf("state root parent = %q, want the workarea teardown removes %q", dir, cwd)
	}
}

// TestExecutableDigest_TracksBinaryBytes pins obligation (b)'s foundation:
// the digest is a hash of the executable's actual bytes. Flipping one byte
// changes it (so the self-test record goes stale when the binary changes),
// and a missing file errors instead of yielding an empty digest that would
// defeat staleness.
//
// RED: return a constant and the mismatch check fails.
func TestExecutableDigest_TracksBinaryBytes(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	bin := filepath.Join(dir, "fake-harness")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\necho hi\n"), 0o755); err != nil { //nolint:gosec // G306: test fixture needs the exec bit.
		t.Fatal(err)
	}
	first, err := executableDigestOf(bin)
	if err != nil {
		t.Fatalf("executableDigestOf: %v", err)
	}
	if !strings.HasPrefix(first, "sha256:") || len(first) != len("sha256:")+64 {
		t.Errorf("executableDigestOf = %q, want sha256:<64 hex>", first)
	}
	if err := os.WriteFile(bin, []byte("#!/bin/sh\necho yo\n"), 0o755); err != nil { //nolint:gosec // G306: test fixture.
		t.Fatal(err)
	}
	second, err := executableDigestOf(bin)
	if err != nil {
		t.Fatalf("executableDigestOf: %v", err)
	}
	if first == second {
		t.Errorf("digest did not change when the binary bytes changed: %q", first)
	}
	if _, err := executableDigestOf(filepath.Join(dir, "missing")); err == nil {
		t.Errorf("executableDigestOf(missing) = nil, want an error, never an empty digest")
	}
}

// TestNewHeadlessChildCommand_PassesNoExtraFiles pins obligation (a)'s
// headless half structurally: the child command carries no ExtraFiles, so
// the confined process inherits exactly its three standard-descriptor
// pipes and no descriptor open on an out-of-set file. The live test below
// proves the same behaviorally through the real spawn.
//
// RED: set cmd.ExtraFiles and the assertion fails.
func TestNewHeadlessChildCommand_PassesNoExtraFiles(t *testing.T) {
	t.Parallel()
	cmd := newHeadlessChildCommand([]string{"/bin/sh", "-c", "true"}, t.TempDir(), []string{"A=1"})
	if len(cmd.ExtraFiles) != 0 {
		t.Errorf("ExtraFiles = %d entries, want none: an inherited descriptor bypasses the boundary", len(cmd.ExtraFiles))
	}
	if cmd.Dir == "" || len(cmd.Env) == 0 {
		t.Errorf("child command lost its dir/env: dir=%q env=%d", cmd.Dir, len(cmd.Env))
	}
}

// TestPolicy_StateDirGuardCoversRelocatedRoot pins the guard move: deleting
// the relocated state root is refused exactly like deleting the legacy
// in-checkout directory, even under an allow-everything configuration.
//
// RED: drop stateRoot from the engine and the relocated refusal passes.
func TestPolicy_StateDirGuardCoversRelocatedRoot(t *testing.T) {
	cwd := t.TempDir()
	spec := agent.Spec{Cwd: cwd, SessionName: "s1", Autonomous: true, AllowedTools: []string{"*"}}
	engine := NewPolicyEngine(spec)
	if engine.stateRoot == "" {
		t.Fatalf("engine has no state root")
	}
	if inside, _ := isInside(engine.stateRoot, cwd); !inside {
		t.Fatalf("engine state root %q is not under the session workarea %q", engine.stateRoot, cwd)
	}
	for _, command := range []string{
		"rm -rf " + engine.stateRoot,
		"rm -rf " + filepath.Join(engine.stateRoot, "sessions", "x.jsonl"),
	} {
		if d := engine.Evaluate(ToolCall{Kind: ToolBash, Command: command}); d.Allow {
			t.Errorf("%q allowed, want refused: the relocated root is live harness state", command)
		}
	}
	// The legacy in-checkout directory stays guarded too.
	if d := engine.Evaluate(ToolCall{Kind: ToolBash, Command: "rm -rf .pi"}); d.Allow {
		t.Errorf("rm -rf .pi allowed, want refused: the legacy path may hold a previous version's state")
	}
	if d := engine.Evaluate(ToolCall{Kind: ToolBash, Command: "rm -rf pipeline"}); !d.Allow {
		t.Errorf("rm -rf pipeline denied, want allowed")
	}
}
