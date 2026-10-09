package pi

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/RenseiAI/donmai/agent"
	"github.com/RenseiAI/donmai/runtime/confinement"
)

// authorityWorld is a declared session root with two mutable repositories
// (the selected one and a second), one read-only repository, and a path
// outside the root.
type authorityWorld struct {
	root, selected, second, context, outside string
}

func newAuthorityWorld(t *testing.T) authorityWorld {
	t.Helper()
	base := t.TempDir()
	root := filepath.Join(base, "root")
	return authorityWorld{
		root:     root,
		selected: filepath.Join(root, "selected"),
		second:   filepath.Join(root, "second"),
		context:  filepath.Join(root, "context"),
		outside:  filepath.Join(base, "outside"),
	}
}

func (w authorityWorld) authority() *agent.RepositoryAuthorityPolicy {
	return &agent.RepositoryAuthorityPolicy{
		Protocol: "session-root-v1", WorkareaRoot: w.root, SelectedPath: w.selected,
		MutablePaths: []string{w.selected, w.second}, ReadOnlyPaths: []string{w.context},
		Enforcement: "isolated-read-only-v1",
	}
}

func (w authorityWorld) spec() agent.Spec {
	return agent.Spec{
		Prompt: "hi", Cwd: w.selected, Autonomous: true,
		SandboxEnabled: true, SandboxLevel: agent.SandboxWorkspaceWrite,
		RepositoryAuthority: w.authority(),
	}
}

// TestPolicyEngine_RepositoryAuthorityContainment pins pi's own file tools
// against a declared repository authority: reads and writes work in every
// declared mutable repository, not only the selected one; a declared
// read-only repository is readable and never writable; the workarea root's
// own entries and anything outside the root stay refused. Without an
// authority the selected working directory is the only root, as before.
func TestPolicyEngine_RepositoryAuthorityContainment(t *testing.T) {
	t.Parallel()
	w := newAuthorityWorld(t)
	declared := NewPolicyEngine(w.spec())
	bare := w.spec()
	bare.RepositoryAuthority = nil
	legacy := NewPolicyEngine(bare)

	cases := []struct {
		name       string
		engine     *PolicyEngine
		kind       ToolKind
		path       string
		wantAllow  bool
		wantReason string
	}{
		{name: "write in the selected repository", engine: declared, kind: ToolWrite, path: filepath.Join(w.selected, "a.txt"), wantAllow: true},
		{name: "read in the selected repository", engine: declared, kind: ToolRead, path: filepath.Join(w.selected, "a.txt"), wantAllow: true},
		{name: "write in the second mutable repository", engine: declared, kind: ToolWrite, path: filepath.Join(w.second, "a.txt"), wantAllow: true},
		{name: "edit in the second mutable repository", engine: declared, kind: ToolEdit, path: filepath.Join(w.second, "a.txt"), wantAllow: true},
		{name: "read in the second mutable repository", engine: declared, kind: ToolRead, path: filepath.Join(w.second, "a.txt"), wantAllow: true},
		{name: "read in the read-only repository", engine: declared, kind: ToolRead, path: filepath.Join(w.context, "a.txt"), wantAllow: true},
		{name: "write in the read-only repository", engine: declared, kind: ToolWrite, path: filepath.Join(w.context, "a.txt"), wantReason: "outside the worktree"},
		{name: "edit in the read-only repository", engine: declared, kind: ToolEdit, path: filepath.Join(w.context, "a.txt"), wantReason: "outside the worktree"},
		{name: "write a workarea root entry", engine: declared, kind: ToolWrite, path: filepath.Join(w.root, "top.txt"), wantReason: "outside the worktree"},
		{name: "read a workarea root entry", engine: declared, kind: ToolRead, path: filepath.Join(w.root, "top.txt"), wantReason: "outside the worktree"},
		{name: "write outside the root", engine: declared, kind: ToolWrite, path: filepath.Join(w.outside, "a.txt"), wantReason: "outside the worktree"},
		{name: "read outside the root", engine: declared, kind: ToolRead, path: filepath.Join(w.outside, "a.txt"), wantReason: "outside the worktree"},
		{name: "git metadata in the second mutable repository", engine: declared, kind: ToolWrite, path: filepath.Join(w.second, ".git", "config"), wantReason: ".git directory"},
		{name: "no authority: second repository write", engine: legacy, kind: ToolWrite, path: filepath.Join(w.second, "a.txt"), wantReason: "outside the worktree"},
		{name: "no authority: second repository read", engine: legacy, kind: ToolRead, path: filepath.Join(w.second, "a.txt"), wantReason: "outside the worktree"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := tc.engine.Evaluate(ToolCall{Kind: tc.kind, Path: tc.path, Cwd: w.selected})
			if got.Allow != tc.wantAllow {
				t.Fatalf("Evaluate(%s %s) = %+v, want allow=%v", tc.kind, tc.path, got, tc.wantAllow)
			}
			if !tc.wantAllow && !strings.Contains(got.Reason, tc.wantReason) {
				t.Fatalf("Evaluate(%s %s) reason = %q, want it to contain %q", tc.kind, tc.path, got.Reason, tc.wantReason)
			}
		})
	}
}

// TestPolicyEngine_RepositoryAuthorityBeatsAllowPatterns pins step 2b: an
// allow pattern re-permits an out-of-tree path, but never a mutation of a
// declared read-only repository or of the workarea root's own entries.
// Reads there stay allowed, and the mutable repositories stay writable.
func TestPolicyEngine_RepositoryAuthorityBeatsAllowPatterns(t *testing.T) {
	t.Parallel()
	w := newAuthorityWorld(t)
	spec := w.spec()
	spec.PermissionConfig = &agent.PermissionConfig{AllowPatterns: []string{".*"}, DefaultDecision: "allow"}
	engine := NewPolicyEngine(spec)

	cases := []struct {
		name       string
		kind       ToolKind
		path       string
		wantAllow  bool
		wantReason string
	}{
		{name: "write in the read-only repository", kind: ToolWrite, path: filepath.Join(w.context, "a.txt"), wantReason: "read-only repository"},
		{name: "write the read-only repository itself", kind: ToolWrite, path: w.context, wantReason: "read-only repository"},
		{name: "write a workarea root entry", kind: ToolWrite, path: filepath.Join(w.root, "top.txt"), wantReason: "workarea root"},
		{name: "read in the read-only repository", kind: ToolRead, path: filepath.Join(w.context, "a.txt"), wantAllow: true},
		{name: "write in the second mutable repository", kind: ToolWrite, path: filepath.Join(w.second, "a.txt"), wantAllow: true},
		{name: "write outside the root, allowed by the pattern", kind: ToolWrite, path: filepath.Join(w.outside, "a.txt"), wantAllow: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := engine.Evaluate(ToolCall{Kind: tc.kind, Path: tc.path, Cwd: w.selected})
			if got.Allow != tc.wantAllow {
				t.Fatalf("Evaluate(%s %s) = %+v, want allow=%v", tc.kind, tc.path, got, tc.wantAllow)
			}
			if !tc.wantAllow && !strings.Contains(got.Reason, tc.wantReason) {
				t.Fatalf("Evaluate(%s %s) reason = %q, want it to contain %q", tc.kind, tc.path, got.Reason, tc.wantReason)
			}
		})
	}
}

// TestSpawn_RepositoryAuthorityFileToolsReachEveryMutableRepository drives
// the same containment through the production Spawn entry point and the
// real extension adjudication round trip: a write and a read in the second
// mutable repository and a read in the read-only repository are admitted,
// a write in the read-only repository is refused.
func TestSpawn_RepositoryAuthorityFileToolsReachEveryMutableRepository(t *testing.T) {
	t.Parallel()
	w := newAuthorityWorld(t)
	body := getStateResponse("ses_authority") +
		event(map[string]any{"type": "agent_start"}) +
		adjudicateEvent("m1", "write", "c-second-write", map[string]any{"path": filepath.Join(w.second, "note.txt")}, w.selected) +
		adjudicateEvent("m2", "read", "c-second-read", map[string]any{"path": filepath.Join(w.second, "note.txt")}, w.selected) +
		adjudicateEvent("m3", "read", "c-context-read", map[string]any{"path": filepath.Join(w.context, "note.txt")}, w.selected) +
		adjudicateEvent("m4", "write", "c-context-write", map[string]any{"path": filepath.Join(w.context, "note.txt")}, w.selected) +
		event(map[string]any{"type": "agent_settled"})

	cmds, h, err := spawnScripted(t, w.spec(), handshakeEvent("h1"), body)
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	_ = drain(t, h)

	verdicts := map[string]bool{}
	for _, c := range cmds.commands() {
		if c["type"] != "extension_ui_response" {
			continue
		}
		id, _ := c["id"].(string)
		val, _ := c["value"].(string)
		var d struct {
			Allow bool `json:"allow"`
		}
		if val == "" || json.Unmarshal([]byte(val), &d) != nil {
			continue
		}
		verdicts[id] = d.Allow
	}
	for _, tc := range []struct {
		id   string
		want bool
		what string
	}{
		{"m1", true, "write in the second mutable repository"},
		{"m2", true, "read in the second mutable repository"},
		{"m3", true, "read in the read-only repository"},
		{"m4", false, "write in the read-only repository"},
	} {
		got, ok := verdicts[tc.id]
		if !ok {
			t.Errorf("%s: no adjudication reply", tc.what)
			continue
		}
		if got != tc.want {
			t.Errorf("%s: allow = %v, want %v", tc.what, got, tc.want)
		}
	}
}

// TestManifest_DeclaresHostProvenWorkareaAttestation pins the declaration:
// pi declares session-root-v1 with isolated-read-only-v1 as a ceiling its
// host must prove, and admits a read-only-bearing authority against it.
func TestManifest_DeclaresHostProvenWorkareaAttestation(t *testing.T) {
	t.Parallel()
	caps := (&Provider{}).Manifest().Caps
	if len(caps.MultiRepositoryWorkareaProtocols) != 1 || caps.MultiRepositoryWorkareaProtocols[0] != "session-root-v1" ||
		caps.RepositoryAuthorityEnforcement != "isolated-read-only-v1" || caps.SupportsReadOnlySelectedCWD {
		t.Fatalf("manifest workarea declaration = %q/%q/readOnlyCWD=%v, want session-root-v1/isolated-read-only-v1/false",
			caps.MultiRepositoryWorkareaProtocols, caps.RepositoryAuthorityEnforcement, caps.SupportsReadOnlySelectedCWD)
	}
	if !caps.WorkareaAttestationNeedsHostProof {
		t.Fatal("manifest workarea declaration does not need a host proof; pi's boundary is the executor's confinement, which each host must prove")
	}
	w := newAuthorityWorld(t)
	if _, err := agent.PrepareHarness(w.spec(), (&Provider{}).Manifest()); err != nil {
		t.Fatalf("PrepareHarness refused the authority the manifest declares: %v", err)
	}
}

// TestProveWorkareaHost_WithheldWithoutConfinementBackend pins the host
// proof on a host with no backend (a Linux host without the namespace
// tooling, or any OS without one): the proof fails with the typed
// backend_absent reason and the attestation is withheld whole.
func TestProveWorkareaHost_WithheldWithoutConfinementBackend(t *testing.T) {
	old := piConfinementBackend
	piConfinementBackend = func() confinement.Backend { return nil }
	t.Cleanup(func() { piConfinementBackend = old })
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	p := &Provider{binary: exe, opts: Options{confinementDirs: &piConfinementDirs{
		profileDir: t.TempDir(), home: t.TempDir(), stateHome: t.TempDir(), scratchDir: t.TempDir(),
	}}}
	err = p.ProveWorkareaHost(context.Background())
	if reason, ok := confinement.ReasonOf(err); !ok || reason != confinement.ReasonBackendAbsent {
		t.Fatalf("ProveWorkareaHost = %v, want the backend_absent reason", err)
	}
	attestation, withheld := agent.ProvenWorkareaAttestation(context.Background(), p)
	if !attestation.Empty() || withheld == nil {
		t.Fatalf("ProvenWorkareaAttestation = %+v, %v; want nothing attested and a withheld reason", attestation, withheld)
	}
}

// TestProveWorkareaHost_ProcessLessProviderWithheld: a process-less
// provider spawns nothing to confine, so it proves nothing.
func TestProveWorkareaHost_ProcessLessProviderWithheld(t *testing.T) {
	t.Parallel()
	p, err := New(Options{skipProcess: true})
	if err != nil {
		t.Fatal(err)
	}
	if err := p.ProveWorkareaHost(context.Background()); err == nil {
		t.Fatal("ProveWorkareaHost on a process-less provider = nil, want a withheld proof")
	}
	if attestation, withheld := agent.ProvenWorkareaAttestation(context.Background(), p); !attestation.Empty() || withheld == nil {
		t.Fatalf("ProvenWorkareaAttestation = %+v, %v; want nothing attested", attestation, withheld)
	}
}
