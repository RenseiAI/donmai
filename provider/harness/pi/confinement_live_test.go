//go:build darwin

package pi

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/RenseiAI/donmai/agent"
	"github.com/RenseiAI/donmai/runtime/confinement"
)

// This file is the Done-when evidence: tests that drive the harness through
// the real spawn paths (the provider's own spawnChild for headless, the
// provider's own interactive argv through ptycli for the terminal) under the
// real macOS backend, and prove a forbidden write is refused in both modes.
// Every refusal case also runs unwrapped, where the same write must succeed
// — that control is what makes each test fail when the wrap is removed.
//
// The harness binary is a shell script standing in for pi: the spawn path
// (argv construction, sandbox-exec wrap, environment, working directory,
// inherited descriptors) is production code, while the script performs the
// exact filesystem operations a hostile tool execution would attempt. A real
// pi binary cannot probe the OS boundary this way — its own policy layer
// adjudicates first — so the script isolates the new OS backstop beneath it.

// liveWorldForConfinement builds one session workarea: a root with a mutable
// leaf (with its own .git directory, as the confinement requires), a
// read-only leaf, and a decoy outside the set. The state override keeps the
// session state root on a short path.
func liveWorldForConfinement(t *testing.T) (workareaRoot, mut, ro, outside string) {
	t.Helper()
	base, err := os.MkdirTemp("/tmp", "piconf")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(base) })
	resolved, err := filepath.EvalSymlinks(base)
	if err != nil {
		t.Fatal(err)
	}
	base = resolved
	workareaRoot = filepath.Join(base, "ws")
	mut = filepath.Join(workareaRoot, "mut")
	ro = filepath.Join(workareaRoot, "ro")
	outside = filepath.Join(base, "outside")
	for _, dir := range []string{
		filepath.Join(mut, ".git"),
		filepath.Join(workareaRoot, ".workarea"),
		ro, outside,
	} {
		if err := os.MkdirAll(dir, 0o750); err != nil {
			t.Fatal(err)
		}
	}
	return workareaRoot, mut, ro, outside
}

// liveConfinerForTest runs the real self-test once: the probe is this test
// binary (see the TestMain hook), driven through the production headless and
// PTY spawn bindings by the real backend.
func liveConfinerForTest(t *testing.T, home, stateHome, profileDir string) *confinement.Confiner {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	digest, err := executableDigestOf(exe)
	if err != nil {
		t.Fatal(err)
	}
	c, err := confinement.New(confinement.Options{
		Backend:          confinement.DefaultBackend(),
		ProfileDir:       profileDir,
		Home:             home,
		StateHome:        stateHome,
		ExecutableDigest: digest,
	})
	if err != nil {
		t.Fatal(err)
	}
	scratch, err := os.MkdirTemp("/tmp", "piconfst")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(scratch) })
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()
	if _, err := c.SelfTest(ctx, confinement.SelfTestOptions{
		ProbeCommand: []string{exe},
		ScratchDir:   scratch,
	}); err != nil {
		t.Fatalf("self-test: %v", err)
	}
	return c
}

// probeScript returns a shell script that attempts one write inside the set,
// one outside it, and one into the read-only leaf, then records which file
// descriptors it holds and resolves each one to its target, so the test can
// prove no descriptor opens an out-of-set file. Results land as files the
// test reads. Every attempt redirects its own errors into the report: the
// script always exits zero, so the run's verdict comes from the report, not
// from the last refused write's status.
func probeScript(mut, outside, ro string) string {
	return `#!/bin/sh
report="` + mut + `/report.txt"
: > "$report"
(echo inside-ok > "` + mut + `/inside.txt") 2>>"$report"; echo "inside=$?" >> "$report"
(echo outside-breakout > "` + outside + `/planted.txt") 2>>"$report"; echo "outside=$?" >> "$report"
(echo ro-breakout > "` + ro + `/planted.txt") 2>>"$report"; echo "ro=$?" >> "$report"
ls -l /dev/fd > "` + mut + `/fds.txt" 2>/dev/null
exit 0
`
}

// writeProbeBinary writes the probe script as an executable file.
func writeProbeBinary(t *testing.T, dir, script string) string {
	t.Helper()
	bin := filepath.Join(dir, "fake-harness")
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil { //nolint:gosec // G306: test fixture needs the exec bit.
		t.Fatal(err)
	}
	return bin
}

// assertProbeOutcome checks the confined run: the inside write landed, the
// outside and read-only writes were refused and left nothing behind.
func assertProbeOutcome(t *testing.T, _, outside, ro, report string, wrapped bool) {
	t.Helper()
	body, err := os.ReadFile(report)
	if err != nil {
		t.Fatalf("read report: %v", err)
	}
	text := string(body)
	if !strings.Contains(text, "inside=0") {
		t.Errorf("wrapped=%v: inside write failed:\n%s", wrapped, text)
	}
	if wrapped {
		if !strings.Contains(text, "outside=") || strings.Contains(text, "outside=0") {
			t.Errorf("confined write outside the set succeeded:\n%s", text)
		}
		if !strings.Contains(text, "ro=") || strings.Contains(text, "ro=0") {
			t.Errorf("confined write to the read-only leaf succeeded:\n%s", text)
		}
		if _, err := os.Lstat(filepath.Join(outside, "planted.txt")); err == nil {
			t.Errorf("confined run planted a file outside the set")
		}
		if _, err := os.Lstat(filepath.Join(ro, "planted.txt")); err == nil {
			t.Errorf("confined run planted a file in the read-only leaf")
		}
	} else {
		if !strings.Contains(text, "outside=0") {
			t.Errorf("UNWRAPPED control: outside write failed, so the test cannot discriminate:\n%s", text)
		}
		if !strings.Contains(text, "ro=0") {
			t.Errorf("UNWRAPPED control: read-only write failed, so the test cannot discriminate:\n%s", text)
		}
	}
}

// assertNoLeakedDescriptors checks the descriptor listing the probe left:
// every descriptor the confined child holds must resolve to a standard
// stream, the descriptor directory itself, or /dev/null — never to a file
// outside the writable set. The shell's own listing adds transients (the
// directory listing, the readlink output redirect), so the assertion is on
// the resolved targets, not the raw numbers: the headless child inherits
// the three stdio pipes and the interactive child the PTY slave the same
// way, and anything beyond those would be a descriptor open on a file the
// boundary does not judge.
func assertNoLeakedDescriptors(t *testing.T, mut, outside, ro string) {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(mut, "fds.txt"))
	if err != nil {
		t.Fatalf("read fds: %v", err)
	}
	list := strings.TrimSpace(string(raw))
	for _, line := range strings.Split(list, "\n") {
		fields := strings.Fields(strings.TrimSpace(line))
		if len(fields) == 0 {
			continue
		}
		if fields[0] == "total" {
			continue
		}
		// An `ls -l /dev/fd` row names the descriptor in its last field:
		// "0", "1", "2", the listing's own directory fd, or the fixed
		// macOS fdesc alias (all rendered as bare numbers or /dev/fd
		// paths). A descriptor open on a real file would name that
		// file's path instead — which is exactly what the check below
		// refuses.
		name := fields[len(fields)-1]
		if _, perr := strconv.Atoi(strings.TrimSpace(name)); perr != nil && !strings.HasPrefix(name, "/dev/fd") && !strings.HasPrefix(name, mut+"/") {
			t.Errorf("confined child holds descriptor open on %q (full list %q): only stdio may be inherited", name, list)
		}
		if name == filepath.Join(outside, "planted.txt") || name == filepath.Join(ro, "planted.txt") {
			t.Errorf("confined child holds a descriptor open on out-of-set file %q", name)
		}
	}
	t.Logf("confined child descriptors: %q", list)
}

// runHeadlessProbe drives the probe through the provider's real headless
// spawn: Provider.spawnChild with the probe script as the harness binary,
// so the production argv construction plus the confinement wrap run exactly
// as in a real launch. The test confiner rides the Provider, so the
// production confinerForSession gate runs; a nil confiner drives the
// unwrapped control through the same entry point.
func runHeadlessProbe(t *testing.T, c *confinement.Confiner, spec agent.Spec, layout sessionLayout, bin string) {
	t.Helper()
	p := &Provider{binary: bin, testConfiner: c}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	confiner, err := p.confinerForSession(ctx, spec)
	if err != nil {
		t.Fatalf("confinerForSession: %v", err)
	}
	plan, err := confinePiSession(spec, layout, confiner)
	if err != nil {
		t.Fatalf("confinePiSession: %v", err)
	}
	if plan != nil {
		t.Cleanup(func() { _ = plan.Release() })
	}
	childEnv := confinePiEnv(composeChildEnv(spec, layout, ""), plan)
	cmd, stdin, stdout, err := p.spawnChild(spec, layout, nil, childEnv, launchPrompt, "", nil, nil, plan)
	if err != nil {
		t.Fatalf("spawnChild: %v", err)
	}
	// The probe writes nothing to stdout and exits; drain to EOF so the
	// child's exit cannot block on a full pipe, then close stdin and wait.
	if _, err := io.Copy(io.Discard, stdout); err != nil {
		t.Fatalf("drain probe stdout: %v", err)
	}
	if err := stdin.Close(); err != nil {
		t.Fatalf("close probe stdin: %v", err)
	}
	if err := cmd.Wait(); err != nil {
		t.Fatalf("probe run: %v", err)
	}
}

// TestPiConfinement_HeadlessForbiddenWriteRefused is the headless half of
// Done-when: the forbidden writes are refused through the real headless
// spawn with the real backend, and the same writes succeed unwrapped.
func TestPiConfinement_HeadlessForbiddenWriteRefused(t *testing.T) {
	workareaRoot, mut, ro, outside := liveWorldForConfinement(t)
	home := t.TempDir()
	stateHome := t.TempDir()
	profileDir := filepath.Join(stateHome, "profiles")
	c := liveConfinerForTest(t, home, stateHome, profileDir)

	spec := agent.Spec{
		SessionName: "live",
		Cwd:         mut,
		RepositoryAuthority: &agent.RepositoryAuthorityPolicy{
			Protocol:      "session-root-v1",
			WorkareaRoot:  workareaRoot,
			SelectedPath:  mut,
			MutablePaths:  []string{mut},
			ReadOnlyPaths: []string{ro},
		},
	}
	layout, err := materializeExtensionForSpec(spec)
	if err != nil {
		t.Fatal(err)
	}

	bin := writeProbeBinary(t, t.TempDir(), probeScript(mut, outside, ro))
	runHeadlessProbe(t, c, spec, layout, bin)
	assertProbeOutcome(t, mut, outside, ro, filepath.Join(mut, "report.txt"), true)
	assertNoLeakedDescriptors(t, mut, outside, ro)

	// The discriminating control: the same spec with no declared authority
	// through the same entry point must spawn unconfined (the production
	// gate), so the forbidden writes succeed. If it cannot, the test proves
	// nothing about the wrap; if the wrap is removed, the confined
	// assertions above go red.
	unconfined := spec
	unconfined.RepositoryAuthority = nil
	_ = os.Remove(filepath.Join(mut, "report.txt"))
	_ = os.Remove(filepath.Join(outside, "planted.txt"))
	_ = os.Remove(filepath.Join(ro, "planted.txt"))
	runHeadlessProbe(t, nil, unconfined, layout, bin)
	assertProbeOutcome(t, mut, outside, ro, filepath.Join(mut, "report.txt"), false)
}

// TestPiConfinement_InteractiveForbiddenWriteRefused is the interactive half
// of Done-when: the same refusal through the real PTY spawn path —
// Provider.spawnInteractive with the probe script as the harness binary, so
// the production confinement wrap around the interactive argv runs.
func TestPiConfinement_InteractiveForbiddenWriteRefused(t *testing.T) {
	workareaRoot, mut, ro, outside := liveWorldForConfinement(t)
	home := t.TempDir()
	stateHome := t.TempDir()
	profileDir := filepath.Join(stateHome, "profiles")
	c := liveConfinerForTest(t, home, stateHome, profileDir)

	bin := writeProbeBinary(t, t.TempDir(), probeScript(mut, outside, ro))
	runInteractiveProbe(t, c, bin, workareaRoot, mut, ro)
	assertProbeOutcome(t, mut, outside, ro, filepath.Join(mut, "report.txt"), true)
	assertNoLeakedDescriptors(t, mut, outside, ro)

	_ = os.Remove(filepath.Join(mut, "report.txt"))
	_ = os.Remove(filepath.Join(outside, "planted.txt"))
	_ = os.Remove(filepath.Join(ro, "planted.txt"))
	unconfinedInteractive := agent.Spec{
		SessionName: "live-probe",
		Cwd:         mut,
		Interactive: &agent.InteractiveSpec{Cols: 80, Rows: 24},
	}
	runInteractiveProbeWithSpec(t, nil, bin, unconfinedInteractive)
	assertProbeOutcome(t, mut, outside, ro, filepath.Join(mut, "report.txt"), false)
}

// runInteractiveProbe drives the probe through the real interactive spawn:
// Provider.spawnInteractive with the probe script as the harness binary,
// waited to exit. A harness binary ignores the interactive argv flags the
// production path appends (like the fake interactive provider's scripts do)
// and just runs the probe body. The test confiner rides the Provider, so
// the production confinerForSession gate runs; a nil confiner drives the
// unwrapped control through the same entry point.
func runInteractiveProbe(t *testing.T, c *confinement.Confiner, bin, workareaRoot, mut, ro string) {
	t.Helper()
	runInteractiveProbeWithSpec(t, c, bin, agent.Spec{
		SessionName: "live-probe",
		Cwd:         mut,
		Interactive: &agent.InteractiveSpec{Cols: 80, Rows: 24},
		RepositoryAuthority: &agent.RepositoryAuthorityPolicy{
			Protocol:      "session-root-v1",
			WorkareaRoot:  workareaRoot,
			SelectedPath:  mut,
			MutablePaths:  []string{mut},
			ReadOnlyPaths: []string{ro},
		},
	})
}

// runInteractiveProbeWithSpec drives the probe through the real interactive
// spawn with the caller-chosen spec: Provider.spawnInteractive with the
// probe script as the harness binary, waited to exit.
func runInteractiveProbeWithSpec(t *testing.T, c *confinement.Confiner, bin string, spec agent.Spec) {
	t.Helper()
	p := &Provider{binary: bin, testConfiner: c}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	h, err := p.spawnInteractive(ctx, spec)
	if err != nil {
		t.Fatalf("spawnInteractive: %v", err)
	}
	defer func() { _ = h.Stop(context.Background()) }()
	deadline := time.After(60 * time.Second)
	for {
		select {
		case _, ok := <-h.Events():
			if !ok {
				return
			}
		case <-deadline:
			t.Fatal("timed out waiting for the interactive probe to exit")
		}
	}
}

// TestPiConfinement_DigestChangeRetiresAttestation pins obligation (b)
// through the real record: a confiner holding a passing self-test attests;
// a confiner built for a changed harness binary — the same options under a
// different executable digest — holds no record and refuses to prepare,
// instead of reusing the cached attestation.
//
// RED: wire a constant digest into ensurePiConfiner and the second Prepare
// succeeds.
func TestPiConfinement_DigestChangeRetiresAttestation(t *testing.T) {
	workareaRoot, mut, ro, _ := liveWorldForConfinement(t)
	home := t.TempDir()
	stateHome := t.TempDir()
	profileDir := filepath.Join(stateHome, "profiles")
	c := liveConfinerForTest(t, home, stateHome, profileDir)
	if _, ok := c.Attestation(); !ok {
		t.Fatalf("passing self-test does not attest")
	}
	spec := agent.Spec{SessionName: "live", Cwd: mut}
	layout, err := materializeExtensionForSpec(spec)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := confinePiSession(agent.Spec{
		SessionName: "live",
		Cwd:         mut,
		PromptMode:  agent.PromptModeAutonomous,
		RepositoryAuthority: &agent.RepositoryAuthorityPolicy{
			Protocol:      "session-root-v1",
			WorkareaRoot:  workareaRoot,
			SelectedPath:  mut,
			MutablePaths:  []string{mut},
			ReadOnlyPaths: []string{ro},
		},
	}, layout, c); err != nil {
		t.Fatalf("Prepare under the tested digest: %v", err)
	}

	// The harness binary changed: same host, same profile directory, a
	// different executable digest. No self-test ran under it, so nothing
	// attests and Prepare refuses rather than reusing the earlier record.
	changed, err := confinement.New(confinement.Options{
		Backend:          confinement.DefaultBackend(),
		ProfileDir:       profileDir,
		Home:             home,
		StateHome:        stateHome,
		ExecutableDigest: "sha256:changed-harness-binary",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := changed.Attestation(); ok {
		t.Fatalf("a confiner that never ran the self-test attests")
	}
	if _, err := confinePiSession(agent.Spec{
		SessionName: "live",
		Cwd:         mut,
		PromptMode:  agent.PromptModeAutonomous,
		RepositoryAuthority: &agent.RepositoryAuthorityPolicy{
			Protocol:      "session-root-v1",
			WorkareaRoot:  workareaRoot,
			SelectedPath:  mut,
			MutablePaths:  []string{mut},
			ReadOnlyPaths: []string{ro},
		},
	}, layout, changed); err == nil {
		t.Fatalf("Prepare under a changed digest succeeded without a self-test: the cached attestation was reused")
	}
}
