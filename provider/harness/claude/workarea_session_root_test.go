package claude

import (
	"slices"
	"testing"

	"github.com/RenseiAI/donmai/agent"
)

// TestBuildArgs_MutableDeclarationGrantsEveryMutablePath pins the
// session-root-v1 write scope on the headless argv: the session CWD stays
// first and every other declared mutable repository path follows as its own
// --add-dir, so the agent can work in each writable repository. A
// read-only declaration path must never enter the grants.
func TestBuildArgs_MutableDeclarationGrantsEveryMutablePath(t *testing.T) {
	t.Parallel()

	spec := agent.Spec{
		Prompt: "work everywhere",
		Cwd:    "/work/root/primary",
		RepositoryAuthority: &agent.RepositoryAuthorityPolicy{
			Protocol: "session-root-v1", WorkareaRoot: "/work/root", SelectedPath: "/work/root/primary",
			MutablePaths:  []string{"/work/root/primary", "/work/root/secondary"},
			ReadOnlyPaths: []string{"/work/root/context"}, Enforcement: "none",
		},
	}
	argv, _ := buildArgs(spec, "", "")

	var addDirs []string
	for i, flag := range argv {
		if flag == "--add-dir" && i+1 < len(argv) {
			addDirs = append(addDirs, argv[i+1])
		}
	}
	want := []string{"/work/root/primary", "/work/root/secondary"}
	if !slices.Equal(addDirs, want) {
		t.Fatalf("--add-dir grants = %q, want %q", addDirs, want)
	}
}

// TestBuildArgs_NoAuthorityKeepsSingleCwdGrant is the flat-layout control:
// without a declaration the argv carries exactly the session CWD.
func TestBuildArgs_NoAuthorityKeepsSingleCwdGrant(t *testing.T) {
	t.Parallel()

	argv, _ := buildArgs(agent.Spec{Prompt: "x", Cwd: "/work/leaf"}, "", "")

	var addDirs []string
	for i, flag := range argv {
		if flag == "--add-dir" && i+1 < len(argv) {
			addDirs = append(addDirs, argv[i+1])
		}
	}
	if !slices.Equal(addDirs, []string{"/work/leaf"}) {
		t.Fatalf("--add-dir grants = %q, want [/work/leaf]", addDirs)
	}
}

// TestInteractiveArgs_MutableDeclarationGrantsEveryMutablePath pins the
// session-root-v1 write scope on the interactive REPL argv: it must match
// the headless lane grant for grant (CWD first, then every other declared
// mutable path), never silently dropping the sibling leaves.
func TestInteractiveArgs_MutableDeclarationGrantsEveryMutablePath(t *testing.T) {
	t.Parallel()

	spec := agent.Spec{
		Prompt: "work everywhere",
		Cwd:    "/work/root/primary",
		RepositoryAuthority: &agent.RepositoryAuthorityPolicy{
			Protocol: "session-root-v1", WorkareaRoot: "/work/root", SelectedPath: "/work/root/primary",
			MutablePaths:  []string{"/work/root/primary", "/work/root/secondary"},
			ReadOnlyPaths: []string{"/work/root/context"}, Enforcement: "none",
		},
	}
	headless, _ := buildArgs(spec, "", "")
	got := interactiveArgs(spec)

	addDirsOf := func(argv []string) []string {
		var dirs []string
		for i, flag := range argv {
			if flag == "--add-dir" && i+1 < len(argv) {
				dirs = append(dirs, argv[i+1])
			}
		}
		return dirs
	}
	headlessDirs, interactiveDirs := addDirsOf(headless), addDirsOf(got)
	want := []string{"/work/root/primary", "/work/root/secondary"}
	if !slices.Equal(interactiveDirs, want) {
		t.Fatalf("interactive --add-dir grants = %q, want %q", interactiveDirs, want)
	}
	if !slices.Equal(interactiveDirs, headlessDirs) {
		t.Fatalf("interactive grants %q diverge from headless grants %q", interactiveDirs, headlessDirs)
	}
	if len(got) == 0 || got[len(got)-1] != spec.Prompt {
		t.Fatalf("positional prompt must stay last: %q", got)
	}
}

// TestManifest_AttestsSessionRootV1WithoutReadOnlyEnforcement pins the
// protocol gate half of the declaration: this harness attests
// session-root-v1 (an all-mutable declaration passes admission) while
// declaring no read-only enforcement (a read-only-bearing authority still
// fails admission at ValidateSpecCapabilities).
func TestManifest_AttestsSessionRootV1WithoutReadOnlyEnforcement(t *testing.T) {
	t.Parallel()

	manifest := (&Provider{}).Manifest()
	if !slices.Contains(manifest.Caps.MultiRepositoryWorkareaProtocols, "session-root-v1") {
		t.Fatalf("MultiRepositoryWorkareaProtocols = %q, want session-root-v1 attested", manifest.Caps.MultiRepositoryWorkareaProtocols)
	}
	if manifest.Caps.RepositoryAuthorityEnforcement != "" {
		t.Fatalf("RepositoryAuthorityEnforcement = %q, want unset (no proven read-only boundary)", manifest.Caps.RepositoryAuthorityEnforcement)
	}

	mutable := agent.Spec{
		Cwd: "/work/root/primary", SandboxEnabled: true, SandboxLevel: agent.SandboxWorkspaceWrite,
		RepositoryAuthority: &agent.RepositoryAuthorityPolicy{
			Protocol: "session-root-v1", WorkareaRoot: "/work/root", SelectedPath: "/work/root/primary",
			MutablePaths: []string{"/work/root/primary", "/work/root/secondary"}, Enforcement: "",
		},
	}
	if err := agent.ValidateSpecCapabilities(mutable, manifest); err != nil {
		t.Fatalf("all-mutable authority admission = %v, want nil", err)
	}

	// The runner's contract gate (validateRepositoryWorkarea) still refuses
	// a read-only-bearing declaration on this harness: ValidateFor demands
	// isolated-read-only-v1 enforcement whenever any repository is
	// read-only, which this manifest deliberately leaves unset. Admission
	// alone cannot express that — it compares the single enforcement
	// string — so this half of the gate is pinned at the runner layer in
	// runner/workarea_protocol_gate_test.go, not here.
	withReadOnly := mutable
	withReadOnly.RepositoryAuthority = &agent.RepositoryAuthorityPolicy{
		Protocol: "session-root-v1", WorkareaRoot: "/work/root", SelectedPath: "/work/root/primary",
		MutablePaths: []string{"/work/root/primary"}, ReadOnlyPaths: []string{"/work/root/context"}, Enforcement: "",
	}
	if err := agent.ValidateSpecCapabilities(withReadOnly, manifest); err != nil {
		t.Fatalf("read-only-bearing authority admission = %v, want nil (the runner contract gate refuses it later)", err)
	}
}
