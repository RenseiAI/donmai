package pi

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/RenseiAI/donmai/agent"
	"github.com/RenseiAI/donmai/runtime/harnessstate"
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
	// Same display name must not pin the root: two sessions under one
	// shared parent workarea with different worktree leaves get distinct
	// roots even when their display names collide. Display-name keying
	// collides here (same parent, same name) and goes RED.
	sharedParent := t.TempDir()
	siblingA := agent.Spec{Cwd: filepath.Join(sharedParent, "work-a"), SessionName: "shared"}
	siblingB := agent.Spec{Cwd: filepath.Join(sharedParent, "work-b"), SessionName: "shared"}
	if sessionStateRoot(siblingA) == sessionStateRoot(siblingB) {
		t.Errorf("sessions sharing a display name share state root %q", sessionStateRoot(siblingA))
	}
	// The leaf key itself must differ: display-name keying gives both
	// siblings the same `.pi-shared` leaf and goes RED here.
	if baseA, baseB := filepath.Base(sessionStateRoot(siblingA)), filepath.Base(sessionStateRoot(siblingB)); baseA == baseB {
		t.Errorf("same-name siblings share state leaf %q: the root must follow the worktree leaf, never the display name", baseA)
	}
	// Same workarea, resumed under a different display name: same root.
	// The root follows the workarea, never the name.
	renamed := agent.Spec{Cwd: cwd, SessionName: "s2"}
	if got := sessionStateRoot(renamed); got != root {
		t.Errorf("renamed sessionStateRoot = %q, want %q", got, root)
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
	layout, err := materializeExtensionForSpec(spec, false)
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

// TestMaterializeExtensionForSpec_LeavesCheckoutClean pins the P3 motive:
// the per-session `.pi-<leaf>` state dir must not report as untracked in
// `git status` — a `?? .pi-<leaf>/` line reads as session-dropped junk
// and invites deletion of live state. The static harnessstate table
// cannot name the per-session leaf, so materialization writes the exact
// entry to the checkout's exclude file at spawn time.
//
// RED: drop the EnsureGitExcluded call from materializeExtensionForSpec
// and the checkout reports the state dir as untracked.
func TestMaterializeExtensionForSpec_LeavesCheckoutClean(t *testing.T) {
	t.Parallel()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("hi\n"), 0o600); err != nil {
		t.Fatalf("seed file: %v", err)
	}
	for _, args := range [][]string{
		{"init", "--quiet"},
		{"config", "user.email", "test@example.invalid"},
		{"config", "user.name", "test"},
		{"add", "README.md"},
		{"commit", "--quiet", "-m", "seed"},
	} {
		cmd := exec.Command("git", append([]string{"-C", dir}, args...)...) //nolint:gosec // G204: test fixture; dir is t.TempDir() and args are literals.
		cmd.Env = harnessstate.GitLocationNeutralEnv(os.Environ())
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	// The workarea is a subdirectory of the checkout, the way a session
	// worktree sits inside one.
	cwd := filepath.Join(dir, "work")
	if err := os.MkdirAll(cwd, 0o700); err != nil {
		t.Fatal(err)
	}
	layout, err := materializeExtensionForSpec(agent.Spec{Cwd: cwd, SessionName: "s1"}, false)
	if err != nil {
		t.Fatalf("materializeExtensionForSpec: %v", err)
	}
	status := exec.Command("git", "-C", dir, "status", "--porcelain") //nolint:gosec // G204: test fixture; dir is t.TempDir().
	status.Env = harnessstate.GitLocationNeutralEnv(os.Environ())
	out, err := status.CombinedOutput()
	if err != nil {
		t.Fatalf("git status: %v\n%s", err, out)
	}
	if strings.TrimSpace(string(out)) != "" {
		t.Fatalf("checkout is dirty after materializing session state:\n%s", out)
	}
	// The state dir really is there — a clean status because nothing was
	// created would be a false pass.
	if _, err := os.Stat(layout.root); err != nil {
		t.Fatalf("state dir missing after materialize: %v", err)
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
