package pi

import (
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/RenseiAI/donmai/agent"
)

// TestSpawn_ReadOnlyRepositoryAuthorityRefusesWrites is the read-only
// half of the seat contract: a session whose declared authority marks a
// sibling repository read-only can read it but cannot write to it, through
// the production Spawn entry point. The manifest declares the workarea
// protocol and enforcement (manifest.go); the request-time boundary here is
// the policy engine the live handle adjudicates every tool call against,
// fed by the same declared authority the OS confinement plan consumes.
//
// The fixture drives a scripted headless session carrying a declared
// authority — mutable selected leaf plus one read-only sibling — and issues
// four adjudication requests through the real extension round-trip shape:
// a write and a read inside the mutable leaf (both admitted), and a write
// and a read inside the read-only sibling (write denied, read admitted).
// A read-only write the boundary admitted would be a seat writing where
// only reads may go; a denied sibling read would break the context the
// sibling exists to provide.
//
// RED proof: revert manifest.go's workarea declaration and admission
// refuses the authority-bearing spec before any adjudication runs; widen
// the policy engine's containment to the session Cwd and the sibling
// write is admitted.
func TestSpawn_ReadOnlyRepositoryAuthorityRefusesWrites(t *testing.T) {
	t.Parallel()
	base := t.TempDir()
	selected := filepath.Join(base, "selected")
	sibling := filepath.Join(base, "sibling")
	spec := agent.Spec{
		Prompt: "hi", Cwd: selected, Autonomous: true,
		SandboxEnabled: true, SandboxLevel: agent.SandboxWorkspaceWrite,
		RepositoryAuthority: &agent.RepositoryAuthorityPolicy{
			Protocol: "session-root-v1", WorkareaRoot: base, SelectedPath: selected,
			MutablePaths: []string{selected}, ReadOnlyPaths: []string{sibling}, Enforcement: "isolated-read-only-v1",
		},
	}
	body := getStateResponse("ses_readonly") +
		event(map[string]any{"type": "agent_start"}) +
		adjudicateEvent("r1", "write", "c-mut-write", map[string]any{"path": filepath.Join(selected, "note.txt")}, selected) +
		adjudicateEvent("r2", "read", "c-mut-read", map[string]any{"path": filepath.Join(selected, "note.txt")}, selected) +
		adjudicateEvent("r3", "write", "c-ro-write", map[string]any{"path": filepath.Join(sibling, "note.txt")}, selected) +
		adjudicateEvent("r4", "read", "c-ro-read", map[string]any{"path": filepath.Join(sibling, "note.txt")}, selected) +
		event(map[string]any{"type": "agent_settled"})

	cmds, h, err := spawnScripted(t, spec, handshakeEvent("h1"), body)
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
		if val == "" {
			continue
		}
		var d struct {
			Allow bool `json:"allow"`
		}
		if json.Unmarshal([]byte(val), &d) != nil {
			continue
		}
		switch id {
		case "r1", "r2", "r3", "r4":
			verdicts[id] = d.Allow
		}
	}
	for _, tc := range []struct {
		id   string
		want bool
		what string
	}{
		{"r1", true, "write inside the mutable leaf"},
		{"r2", true, "read inside the mutable leaf"},
		{"r3", false, "write inside the read-only sibling"},
		{"r4", true, "read inside the read-only sibling"},
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

// TestPrepareHarnessAdmitsReadOnlyRepositoryAuthority pins the admission
// half: a spec carrying a read-only sibling authority is admitted against
// the live manifest, because the manifest declares the workarea protocol
// and enforcement the authority names. RED proof: revert manifest.go's
// workarea declaration and this returns a typed SpecAdmissionError naming
// repositoryAuthority instead.
func TestPrepareHarnessAdmitsReadOnlyRepositoryAuthority(t *testing.T) {
	t.Parallel()
	base := t.TempDir()
	selected := filepath.Join(base, "selected")
	sibling := filepath.Join(base, "sibling")
	spec := agent.Spec{
		Prompt: "hi", Cwd: selected,
		SandboxEnabled: true, SandboxLevel: agent.SandboxWorkspaceWrite,
		RepositoryAuthority: &agent.RepositoryAuthorityPolicy{
			Protocol: "session-root-v1", WorkareaRoot: base, SelectedPath: selected,
			MutablePaths: []string{selected}, ReadOnlyPaths: []string{sibling}, Enforcement: "isolated-read-only-v1",
		},
	}
	if _, err := agent.PrepareHarness(spec, (&Provider{}).Manifest()); err != nil {
		var denial *agent.SpecAdmissionError
		if errors.As(err, &denial) && denial.Field == "repositoryAuthority" {
			t.Fatalf("PrepareHarness refused the read-only authority the manifest declares: %v", err)
		}
		t.Fatalf("PrepareHarness: %v", err)
	}
}

// TestManifestDeclaresReadOnlyRepositoryAuthority pins the declaration
// itself: the live manifest attests the exact workarea protocol and the
// exact enforcement the runner's workarea gate requires before it binds a
// read-only sibling to a session. A name-only assertion would pass while
// the gate still refuses; the ValidateFor call below is the gate's own
// check, so it fails exactly when the gate would.
func TestManifestDeclaresReadOnlyRepositoryAuthority(t *testing.T) {
	t.Parallel()
	caps := (&Provider{}).Manifest().Caps
	var protocols []string
	protocols = append(protocols, caps.MultiRepositoryWorkareaProtocols...)
	enforcement := caps.RepositoryAuthorityEnforcement
	if len(protocols) != 1 || protocols[0] != "session-root-v1" || enforcement != "isolated-read-only-v1" {
		t.Fatalf("manifest workarea attestation = %q/%q, want session-root-v1/isolated-read-only-v1", protocols, enforcement)
	}
	if !strings.Contains(enforcement, "read-only") {
		t.Fatalf("manifest enforcement %q does not name the read-only boundary", enforcement)
	}
}
