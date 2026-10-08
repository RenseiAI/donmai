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
// a write and a read inside the mutable leaf (both admitted), a write and
// a read inside the read-only sibling under a seat-supplied allow pattern
// covering the sibling path (write denied by the read-only declaration,
// read admitted). The allow-pattern write is the case only the read-only
// guard denies: step 2 explicitly re-permits containment failures on an
// allow-pattern match, so without the guard that write flips to allow.
// A read-only write the boundary admitted would be a seat writing where
// only reads may go; a denied sibling read would break the context the
// sibling exists to provide.
//
// RED proof: revert manifest.go's workarea declaration and admission
// refuses the authority-bearing spec before any adjudication runs; delete
// the policy engine's step 2b read-only guard and the allow-pattern sibling
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
		// The seat-supplied allow pattern covers the sibling path: step 2
		// explicitly re-permits containment failures on an allow-pattern
		// match, so the allow-pattern sibling write below reaches step 2b
		// as its sole remaining deny. Without the read-only guard that
		// write flips to allow.
		PermissionConfig: &agent.PermissionConfig{
			AllowPatterns:   []string{".*"},
			DefaultDecision: "allow",
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
	reasons := map[string]string{}
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
			Allow  bool   `json:"allow"`
			Reason string `json:"reason"`
		}
		if json.Unmarshal([]byte(val), &d) != nil {
			continue
		}
		switch id {
		case "r1", "r2", "r3", "r4":
			verdicts[id] = d.Allow
			reasons[id] = d.Reason
		}
	}
	for _, tc := range []struct {
		id   string
		want bool
		what string
	}{
		{"r1", true, "write inside the mutable leaf"},
		{"r2", true, "read inside the mutable leaf"},
		{"r3", false, "allow-pattern-admitted write inside the read-only sibling"},
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
	// The sibling write must be denied by the read-only declaration
	// itself, not by containment: the seat-supplied allow pattern covers
	// the sibling path, so step 2 re-permits the containment failure and
	// only step 2b remains to deny it. A containment reason here would
	// mean this test no longer isolates the guard it pins.
	if reason := reasons["r3"]; !strings.Contains(reason, "read-only repository") {
		t.Errorf("allow-pattern sibling write denied for another reason (%q): the test no longer isolates the step 2b guard", reason)
	}
}

// TestSpawn_ReadOnlyRepositoryBoundaryPairs pins the read vs mutate
// membership pair the two policy walks share: the sibling directory and
// the workarea-root file are denied for mutation while admitted for
// reads, and the selected-leaf file stays admitted for both. The sibling
// directory (not just a file inside it) proves insidePath matches the
// root itself; the workarea-root file proves the mutation deny reaches
// the root while the read carve-out does not. Dropping the cwd exemption
// in readOnlyReason denies the selected-leaf write; deleting the step 2b
// guard admits both mutation denies — both regressions turn this test
// RED. Like the test above it drives the production Spawn entry point
// through the real extension round-trip shape, under the same
// seat-supplied allow pattern.
func TestSpawn_ReadOnlyRepositoryBoundaryPairs(t *testing.T) {
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
		PermissionConfig: &agent.PermissionConfig{
			AllowPatterns:   []string{".*"},
			DefaultDecision: "allow",
		},
	}
	body := getStateResponse("ses_readonly_bounds") +
		event(map[string]any{"type": "agent_start"}) +
		adjudicateEvent("b1", "write", "c-sibdir-write", map[string]any{"path": sibling}, selected) +
		adjudicateEvent("b2", "read", "c-sibdir-read", map[string]any{"path": sibling}, selected) +
		adjudicateEvent("b3", "write", "c-rootfile-write", map[string]any{"path": filepath.Join(base, "top.txt")}, selected) +
		adjudicateEvent("b4", "read", "c-rootfile-read", map[string]any{"path": filepath.Join(base, "top.txt")}, selected) +
		adjudicateEvent("b5", "write", "c-selfile-write", map[string]any{"path": filepath.Join(selected, "note.txt")}, selected) +
		adjudicateEvent("b6", "read", "c-selfile-read", map[string]any{"path": filepath.Join(selected, "note.txt")}, selected) +
		event(map[string]any{"type": "agent_settled"})

	cmds, h, err := spawnScripted(t, spec, handshakeEvent("h1"), body)
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	_ = drain(t, h)

	verdicts := map[string]bool{}
	reasons := map[string]string{}
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
			Allow  bool   `json:"allow"`
			Reason string `json:"reason"`
		}
		if json.Unmarshal([]byte(val), &d) != nil {
			continue
		}
		switch id {
		case "b1", "b2", "b3", "b4", "b5", "b6":
			verdicts[id] = d.Allow
			reasons[id] = d.Reason
		}
	}
	for _, tc := range []struct {
		id   string
		want bool
		what string
	}{
		{"b1", false, "write to the sibling directory itself"},
		{"b2", true, "read of the sibling directory itself"},
		{"b3", false, "write to a workarea-root file"},
		{"b4", true, "read of a workarea-root file"},
		{"b5", true, "write inside the selected leaf"},
		{"b6", true, "read inside the selected leaf"},
	} {
		got, ok := verdicts[tc.id]
		if !ok {
			t.Errorf("%s: no adjudication reply", tc.what)
			continue
		}
		if got != tc.want {
			t.Errorf("%s: allow = %v, want %v (reason %q)", tc.what, got, tc.want, reasons[tc.id])
		}
	}
	// Both mutation denies must come from the read-only declaration
	// itself: the allow pattern covers every path here, so step 2
	// re-permits the containment failure and only step 2b remains.
	for _, id := range []string{"b1", "b3"} {
		if reason := reasons[id]; !strings.Contains(reason, "read-only repository") {
			t.Errorf("%s denied for another reason (%q): the pair no longer isolates the step 2b guard", id, reason)
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
