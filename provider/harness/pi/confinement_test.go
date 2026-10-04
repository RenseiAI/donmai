package pi

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/RenseiAI/donmai/agent"
)

// TestSessionStateRoot_SiblingOfWorkdir pins the state relocation: pi's own
// state lives beside the working folder, never inside it, so the working
// folder can be confined as repository content.
//
// RED: point sessionStateRoot back under spec.Cwd and the InsideCwd checks
// fail.
func TestSessionStateRoot_SiblingOfWorkdir(t *testing.T) {
	t.Setenv(piStateDirEnvOverride, "")
	cwd := filepath.Join(t.TempDir(), "repo")
	spec := agent.Spec{Cwd: cwd, SessionName: "s1"}
	root := sessionStateRoot(spec)
	if inside, err := isInside(root, cwd); err != nil || inside {
		t.Errorf("sessionStateRoot %q is inside the working folder %q", root, cwd)
	}
	parent := filepath.Dir(filepath.Clean(cwd))
	if dir := filepath.Dir(filepath.Clean(root)); dir != parent {
		t.Errorf("sessionStateRoot parent = %q, want the workdir parent %q", dir, parent)
	}
	if base := filepath.Base(filepath.Clean(root)); !strings.HasPrefix(base, ".pi-") {
		t.Errorf("sessionStateRoot base = %q, want a .pi-<key> directory", base)
	}
	if got := sessionKey(spec); got != "s1" {
		t.Errorf("sessionKey = %q, want the session name %q", got, "s1")
	}
	unnamed := agent.Spec{Cwd: cwd}
	if got := sessionKey(unnamed); got != "repo" {
		t.Errorf("sessionKey without a session name = %q, want the workdir name", got)
	}
	layout := newSessionLayoutForSpec(spec)
	if layout.root != root {
		t.Errorf("layout root = %q, want the session state root %q", layout.root, root)
	}
	for _, p := range []string{layout.extension, layout.injected, layout.agentHome} {
		if inside, err := isInside(p, root); err != nil || !inside {
			t.Errorf("layout path %q is not under the state root %q", p, root)
		}
		if inside, err := isInside(p, cwd); err != nil || inside {
			t.Errorf("layout path %q is inside the working folder", p)
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

// TestSessionStateRoot_EnvOverride pins the test seam: the override names
// the parent, and the session key still scopes the directory.
func TestSessionStateRoot_EnvOverride(t *testing.T) {
	base := t.TempDir()
	t.Setenv(piStateDirEnvOverride, base)
	spec := agent.Spec{Cwd: filepath.Join(t.TempDir(), "repo"), SessionName: "s1"}
	if got := sessionStateBase(spec); got != base {
		t.Errorf("sessionStateBase = %q, want the override %q", got, base)
	}
	if want := filepath.Join(base, ".pi-s1"); sessionStateRoot(spec) != want {
		t.Errorf("sessionStateRoot = %q, want %q", sessionStateRoot(spec), want)
	}
}

// TestMaterializeExtensionForSpec_LivesOutsideCwd pins the materialization
// move end to end: the boundary extension lands outside the working folder
// with the same bytes and mode as before, and nothing pi-owned is left
// behind in the checkout.
func TestMaterializeExtensionForSpec_LivesOutsideCwd(t *testing.T) {
	base := t.TempDir()
	t.Setenv(piStateDirEnvOverride, base)
	cwd := t.TempDir()
	spec := agent.Spec{Cwd: cwd, SessionName: "s1"}
	layout, err := materializeExtensionForSpec(spec)
	if err != nil {
		t.Fatalf("materializeExtensionForSpec: %v", err)
	}
	if inside, _ := isInside(layout.extension, cwd); inside {
		t.Errorf("materialized extension %q is inside the working folder", layout.extension)
	}
	data, err := os.ReadFile(layout.extension)
	if err != nil {
		t.Fatalf("read extension: %v", err)
	}
	if string(data) != string(extensionSource()) {
		t.Errorf("materialized extension differs from embedded source")
	}
	assertMode0600(t, layout.extension)
	entries, err := os.ReadDir(cwd)
	if err != nil {
		t.Fatalf("read cwd: %v", err)
	}
	if len(entries) != 0 {
		t.Errorf("working folder holds %d entr(ies) after materialization, want none", len(entries))
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
	base := t.TempDir()
	t.Setenv(piStateDirEnvOverride, base)
	cwd := t.TempDir()
	spec := agent.Spec{Cwd: cwd, SessionName: "s1", Autonomous: true, AllowedTools: []string{"*"}}
	engine := NewPolicyEngine(spec)
	if engine.stateRoot == "" {
		t.Fatalf("engine has no state root")
	}
	if inside, _ := isInside(engine.stateRoot, cwd); inside {
		t.Fatalf("engine state root %q is inside the working folder", engine.stateRoot)
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
