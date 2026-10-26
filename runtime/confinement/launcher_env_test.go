package confinement

// Tests for the final launch boundary: inherited engine re-exec controls
// never reach an engine child on either spawn lane, while the
// producer-owned bindings appended after filtering still do.
//
// RED: remove the launcher base filtering (overlayEnv / ptyhost composeEnv)
// and the inherited evil below reaches the child's recorded environment;
// move the trusted append before the filter and the trusted assertions fail.

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/RenseiAI/donmai/agent"
)

func readEnvFile(t *testing.T, path string) map[string]string {
	t.Helper()
	raw, err := os.ReadFile(path) //nolint:gosec // G304: the test-owned recorder file.
	if err != nil {
		t.Fatalf("read recorded child env: %v", err)
	}
	out := map[string]string{}
	for _, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		if line == "" {
			continue
		}
		if i := strings.IndexByte(line, '='); i >= 0 {
			out[line[:i]] = line[i+1:]
		}
	}
	return out
}

// TestLaunchers_StripInheritedEngineControls pins that a real child launched
// through either production launcher never observes an inherited engine
// control, while safe near-misses still do.
func TestLaunchers_StripInheritedEngineControls(t *testing.T) {
	// Not parallel: stages engine controls in the process environment.
	t.Setenv("DONMAI_CONFINEMENT_PROBE", "parent-evil-probe")
	t.Setenv("DONMAI_CONFINEMENT_LANDLOCK_STAGE", "parent-evil-stage")
	t.Setenv("RENSEI_SEATBELT_ENFORCE", "parent-evil-seatbelt")
	t.Setenv("EVIL_PROBE", "parent-evil-suffix")
	t.Setenv("FOO_PROBE_EXTRA", "parent-near-miss")
	t.Setenv("DONMAI_CONFINEMENT", "parent-bare-prefix")

	trusted := []string{"TRUSTED_CHILD_MARKER=present"}

	// Headless lane: a real child records the environment it was exec'd with.
	headlessFile := filepath.Join(t.TempDir(), "headless-env")
	headlessArgv := []string{"sh", "-c", "env > " + headlessFile + "; exit 0"}
	if code, err := HeadlessLauncher()(context.Background(), headlessArgv, trusted, t.TempDir()); err != nil || code != 0 {
		t.Fatalf("headless launch = %d, %v", code, err)
	}
	headless := readEnvFile(t, headlessFile)
	for _, evil := range []string{"DONMAI_CONFINEMENT_PROBE", "DONMAI_CONFINEMENT_LANDLOCK_STAGE", "RENSEI_SEATBELT_ENFORCE", "EVIL_PROBE"} {
		if v, ok := headless[evil]; ok {
			t.Errorf("headless child observes inherited %s=%q", evil, v)
		}
	}
	if headless["TRUSTED_CHILD_MARKER"] != "present" {
		t.Errorf("headless child misses its trusted marker: %v", headless)
	}
	if headless["FOO_PROBE_EXTRA"] != "parent-near-miss" {
		t.Errorf("headless child misses near-miss FOO_PROBE_EXTRA=%q", headless["FOO_PROBE_EXTRA"])
	}
	if headless["DONMAI_CONFINEMENT"] != "parent-bare-prefix" {
		t.Errorf("headless child misses bare prefix DONMAI_CONFINEMENT=%q", headless["DONMAI_CONFINEMENT"])
	}

	// PTY lane: the same recorder under the interactive launcher.
	ptyFile := filepath.Join(t.TempDir(), "pty-env")
	ptyArgv := []string{"sh", "-c", "env > " + ptyFile + "; exit 0"}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if code, err := InteractiveLauncher()(ctx, ptyArgv, trusted, t.TempDir()); err != nil || code != 0 {
		t.Fatalf("interactive launch = %d, %v", code, err)
	}
	ptyEnv := readEnvFile(t, ptyFile)
	for _, evil := range []string{"DONMAI_CONFINEMENT_PROBE", "DONMAI_CONFINEMENT_LANDLOCK_STAGE", "RENSEI_SEATBELT_ENFORCE", "EVIL_PROBE"} {
		if v, ok := ptyEnv[evil]; ok {
			t.Errorf("pty child observes inherited %s=%q", evil, v)
		}
	}
	if ptyEnv["TRUSTED_CHILD_MARKER"] != "present" {
		t.Errorf("pty child misses its trusted marker: %v", ptyEnv)
	}
	if ptyEnv["FOO_PROBE_EXTRA"] != "parent-near-miss" {
		// The PTY host layers its own locale/terminal defaults but must not
		// drop an unrelated near-miss the parent carried.
		t.Errorf("pty child misses near-miss FOO_PROBE_EXTRA=%q", ptyEnv["FOO_PROBE_EXTRA"])
	}
}

// TestLaunchers_TrustedPlanBindingsSurviveFiltering pins the trusted-last
// order: the plan's session-tmp and stage bindings appended after filtering
// reach the child on both lanes, even when the parent carries an evil copy
// of the same stage name.
func TestLaunchers_TrustedPlanBindingsSurviveFiltering(t *testing.T) {
	// Not parallel: stages an evil parent copy of the trusted stage name.
	t.Setenv("DONMAI_CONFINEMENT_LANDLOCK_STAGE", "parent-evil-stage")

	plan := &Plan{
		env: []string{"TMPDIR=/plan/tmp", "TMP=/plan/tmp", "TEMP=/plan/tmp"},
		applied: Applied{Environ: func() []string {
			return []string{"DONMAI_CONFINEMENT_LANDLOCK_STAGE=trusted-policy"}
		}},
	}
	// The caller env is already sanitized the way production builds it:
	// ApplyToEnv strips the parent evil, then appends the trusted bindings.
	callerEnv := plan.ApplyToEnv([]string{"PATH=/usr/bin"})

	headlessFile := filepath.Join(t.TempDir(), "headless-env")
	headlessArgv := []string{"sh", "-c", "env > " + headlessFile + "; exit 0"}
	if code, err := HeadlessLauncher()(context.Background(), headlessArgv, callerEnv, t.TempDir()); err != nil || code != 0 {
		t.Fatalf("headless launch = %d, %v", code, err)
	}
	headless := readEnvFile(t, headlessFile)
	if headless["DONMAI_CONFINEMENT_LANDLOCK_STAGE"] != "trusted-policy" {
		t.Fatalf("headless child stage = %q, want the trusted policy (env %v)", headless["DONMAI_CONFINEMENT_LANDLOCK_STAGE"], headless)
	}
	if headless["TMPDIR"] != "/plan/tmp" {
		t.Errorf("headless child TMPDIR = %q, want the plan binding", headless["TMPDIR"])
	}

	ptyFile := filepath.Join(t.TempDir(), "pty-env")
	ptyArgv := []string{"sh", "-c", "env > " + ptyFile + "; exit 0"}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if code, err := InteractiveLauncher()(ctx, ptyArgv, callerEnv, t.TempDir()); err != nil || code != 0 {
		t.Fatalf("interactive launch = %d, %v", code, err)
	}
	ptyEnv := readEnvFile(t, ptyFile)
	if ptyEnv["DONMAI_CONFINEMENT_LANDLOCK_STAGE"] != "trusted-policy" {
		t.Fatalf("pty child stage = %q, want the trusted policy", ptyEnv["DONMAI_CONFINEMENT_LANDLOCK_STAGE"])
	}
	if ptyEnv["TMPDIR"] != "/plan/tmp" {
		t.Errorf("pty child TMPDIR = %q, want the plan binding", ptyEnv["TMPDIR"])
	}
}

// TestLaunchers_InheritedProbeDoesNotTriggerProbeMode pins AC-1 with a real
// engine child: the test binary re-executed with an inherited
// DONMAI_CONFINEMENT_PROBE must not enter probe mode (no handshake) and must
// exit as a no-test run, on both lanes. Removing the final-boundary filter
// makes the child serve the probe instead (handshake appears, exit 2).
func TestLaunchers_InheritedProbeDoesNotTriggerProbeMode(t *testing.T) {
	// Not parallel: stages the probe marker in the process environment.
	exe, err := os.Executable()
	if err != nil {
		t.Fatalf("executable: %v", err)
	}
	dir := t.TempDir()
	evilPlan := filepath.Join(dir, "evil-plan.json")
	t.Setenv(ProbeEnv, evilPlan)
	handshake := evilPlan + ".handshake"
	_ = os.Remove(handshake)

	argv := []string{exe, "-test.run=^$"}

	headlessCode, err := HeadlessLauncher()(context.Background(), argv, nil, dir)
	if err != nil {
		t.Fatalf("headless probe launch: %v", err)
	}
	if headlessCode != 0 {
		t.Errorf("headless engine child with inherited probe exited %d, want 0 (no probe mode)", headlessCode)
	}
	if _, statErr := os.Stat(handshake); !os.IsNotExist(statErr) {
		t.Errorf("headless engine child entered probe mode: handshake %s exists", handshake)
	}
	_ = os.Remove(handshake)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	ptyCode, err := InteractiveLauncher()(ctx, argv, nil, dir)
	if err != nil {
		t.Fatalf("interactive probe launch: %v", err)
	}
	if ptyCode != 0 {
		t.Errorf("pty engine child with inherited probe exited %d, want 0 (no probe mode)", ptyCode)
	}
	if _, statErr := os.Stat(handshake); !os.IsNotExist(statErr) {
		t.Errorf("pty engine child entered probe mode: handshake %s exists", handshake)
	}
}

// TestLaunchers_SelfTestProbeMarkerSurvivesAsTrustedLast pins AC-3 at the
// production entry point: the self-test probe marker appended after the plan
// environment reaches the child. Moving either trusted append before the
// inherited filter would strip it, so this fails red under that mutant.
func TestLaunchers_SelfTestProbeMarkerSurvivesAsTrustedLast(t *testing.T) {
	plan := &Plan{
		env: []string{"TMPDIR=/plan/tmp", "TMP=/plan/tmp", "TEMP=/plan/tmp"},
		applied: Applied{Environ: func() []string {
			return []string{"DONMAI_CONFINEMENT_LANDLOCK_STAGE=trusted-policy"}
		}},
	}
	probePlan := filepath.Join(t.TempDir(), "plan.json")
	trustedEnv := append(plan.Environment(), ProbeEnv+"="+probePlan)

	headlessFile := filepath.Join(t.TempDir(), "headless-env")
	headlessArgv := []string{"sh", "-c", "env > " + headlessFile + "; exit 0"}
	if code, err := HeadlessLauncher()(context.Background(), headlessArgv, trustedEnv, t.TempDir()); err != nil || code != 0 {
		t.Fatalf("headless launch = %d, %v", code, err)
	}
	headless := readEnvFile(t, headlessFile)
	if headless[ProbeEnv] != probePlan {
		t.Errorf("headless child probe marker = %q, want %q", headless[ProbeEnv], probePlan)
	}
	if headless["DONMAI_CONFINEMENT_LANDLOCK_STAGE"] != "trusted-policy" {
		t.Errorf("headless child stage = %q, want the trusted policy", headless["DONMAI_CONFINEMENT_LANDLOCK_STAGE"])
	}

	_ = agent.PromptModeAutonomous
}

// TestProbePass_FiltersEvilProbeExtra pins the probe-extra guard at the
// production entry point: probePass must strip engine re-exec controls
// carried in SelfTestOptions.ProbeEnv before the trusted probe marker is
// appended last. Removing the FilterEngineControlEnv call lets an evil
// extra ride into the probe child alongside the trusted marker.
func TestProbePass_FiltersEvilProbeExtra(t *testing.T) {
	w := newSpecWorld(t)
	c := newTestConfiner(t, w, noopBackend{}, nil, "")
	var got []string
	launcher := func(_ context.Context, _ []string, env []string, _ string) (int, error) {
		got = append([]string(nil), env...)
		return 0, nil
	}
	dir := t.TempDir()
	planPath := filepath.Join(dir, "plan.json")
	resultPath := filepath.Join(dir, "result.json")
	// Not parallel: probePass reads no parent environment here; the evil
	// rides the explicit ProbeEnv session input instead.
	_, err := c.probePass(context.Background(), passInput{
		mode:     agent.PromptModeAutonomous,
		launcher: launcher,
		opts: SelfTestOptions{
			ProbeCommand: []string{"/bin/true"},
			ProbeEnv: []string{
				"DONMAI_CONFINEMENT_PROBE_DECOY=evil-decoy",
				"RENSEI_SEATBELT_X=evil-seatbelt",
				"BAR_PROBE=evil-suffix",
				"SAFE_KEEP=keep",
			},
			ProbeHandshakeTimeout: time.Second,
		},
		fx:         &fixture{mut: dir, tmp: dir},
		spec:       w.spec(),
		planPath:   planPath,
		resultPath: resultPath,
	})
	if err != nil {
		t.Fatalf("probePass: %v", err)
	}
	if got == nil {
		t.Fatal("probePass never invoked the launcher")
	}
	envMap := map[string]string{}
	for _, kv := range got {
		if i := strings.IndexByte(kv, '='); i >= 0 {
			envMap[kv[:i]] = kv[i+1:]
		}
	}
	for _, evil := range []string{"DONMAI_CONFINEMENT_PROBE_DECOY", "RENSEI_SEATBELT_X", "BAR_PROBE"} {
		if v, ok := envMap[evil]; ok {
			t.Errorf("probe child observes evil extra %s=%q", evil, v)
		}
	}
	if envMap["SAFE_KEEP"] != "keep" {
		t.Errorf("probe child misses safe extra SAFE_KEEP=%q", envMap["SAFE_KEEP"])
	}
	if envMap[ProbeEnv] != planPath {
		t.Errorf("probe child probe marker = %q, want the trusted %q", envMap[ProbeEnv], planPath)
	}
}

// TestStartDecoy_StripsBroadEngineControls pins the decoy broadening at the
// production entry point: startDecoy must strip every engine re-exec
// control from the parent, not just the explicit probe and stage names.
// Removing the shared-classifier clause lets a parent carrying a seatbelt
// or suffix control ride into the decoy re-exec.
func TestStartDecoy_StripsBroadEngineControls(t *testing.T) {
	// Not parallel: stages engine controls in the process environment.
	t.Setenv(ProbeEnv, "parent-evil-probe")
	t.Setenv(landlockStageEnv, "parent-evil-stage")
	t.Setenv("RENSEI_SEATBELT_ENFORCE", "parent-evil-seatbelt")
	t.Setenv("BAR_PROBE", "parent-evil-suffix")
	t.Setenv("FOO_PROBE_EXTRA", "parent-near-miss")
	t.Setenv("DONMAI_CONFINEMENT", "parent-bare-prefix")
	dir := t.TempDir()
	dump := filepath.Join(dir, "decoy-env")
	script := filepath.Join(dir, "decoy.sh")
	body := "#!/bin/sh\nenv > " + dump + "\nprintf \"ready\\n\"\nsleep 30\n"
	if err := os.WriteFile(script, []byte(body), 0o700); err != nil { //nolint:gosec // G306: a test-owned stand-in executable the launcher runs.
		t.Fatalf("write decoy stand-in: %v", err)
	}
	fx := &fixture{probeCommand: []string{script}, probeHandshake: 10 * time.Second, out: dir}
	t.Cleanup(fx.cleanup)
	marker := filepath.Join(dir, "marker")
	if _, err := fx.startDecoy(marker); err != nil {
		t.Fatalf("startDecoy: %v", err)
	}
	raw, err := os.ReadFile(dump) //nolint:gosec // G304: the test-owned recorder file.
	if err != nil {
		t.Fatalf("read recorded decoy env: %v", err)
	}
	got := map[string]string{}
	decoyCount := 0
	for _, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		if line == "" {
			continue
		}
		if i := strings.IndexByte(line, '='); i >= 0 {
			key, value := line[:i], line[i+1:]
			got[key] = value
			if key == probeDecoyEnv {
				decoyCount++
			}
		}
	}
	for _, evil := range []string{ProbeEnv, landlockStageEnv, "RENSEI_SEATBELT_ENFORCE", "BAR_PROBE"} {
		if v, ok := got[evil]; ok {
			t.Errorf("decoy child observes parent %s=%q", evil, v)
		}
	}
	if decoyCount != 1 {
		t.Errorf("decoy marker present %d times, want exactly once", decoyCount)
	}
	if got[probeDecoyEnv] != marker {
		t.Errorf("decoy child marker = %q, want the trusted %q", got[probeDecoyEnv], marker)
	}
	if got["FOO_PROBE_EXTRA"] != "parent-near-miss" {
		t.Errorf("decoy child misses near-miss FOO_PROBE_EXTRA=%q", got["FOO_PROBE_EXTRA"])
	}
	if got["DONMAI_CONFINEMENT"] != "parent-bare-prefix" {
		t.Errorf("decoy child misses bare prefix DONMAI_CONFINEMENT=%q", got["DONMAI_CONFINEMENT"])
	}
}
