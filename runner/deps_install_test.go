package runner

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/RenseiAI/donmai/agent"
	"github.com/RenseiAI/donmai/provider/harness/stub"
)

// recordingDepsExecer is a DepsExecer fake: it records every call and
// returns a scripted exit code / error without running anything.
type recordingDepsExecer struct {
	mu       sync.Mutex
	calls    []depsCall
	exitCode int
	err      error
}

type depsCall struct {
	dir     string
	command string
	env     map[string]string
}

func (f *recordingDepsExecer) Exec(_ context.Context, dir, command string, env map[string]string) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, depsCall{dir: dir, command: command, env: env})
	return f.exitCode, f.err
}

func (f *recordingDepsExecer) recorded() []depsCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]depsCall(nil), f.calls...)
}

func depsTestRunner(t *testing.T, execer DepsExecer) *Runner {
	t.Helper()
	h := newRunnerHarness(t)
	h.runner.depsExecer = execer
	return h.runner
}

// depsTestLevels stamps the levels the Run-level tests carry so the
// install gate exercises its stamp-reading path end to end: network
// open and fileWrite host are the unstamped-equivalent open surface the
// stub harness renders, so the run passes the pre-spawn refusal and the
// install still runs. The gate's skip halves (a withheld fileWrite,
// network none) are covered by the unit tests below, which call the
// step directly without a harness.
func depsTestLevels() *agent.ExecutionSecurity {
	return &agent.ExecutionSecurity{Version: 1, Levels: agent.IndexZeroExecutionSecurityLevels()}
}

func depsTestQueuedWork(identifier string) QueuedWork {
	qw := QueuedWork{QueuedWork: queuedWorkBase(identifier)}
	qw.ExecutionSecurity = depsTestLevels()
	return qw
}

func TestInstallSessionDependencies_PnpmBeforeGo(t *testing.T) {
	t.Parallel()
	execer := &recordingDepsExecer{}
	r := depsTestRunner(t, execer)

	wpath := t.TempDir()
	writeFile(t, wpath, "pnpm-lock.yaml", "lockfileVersion: 9.0\n")
	writeFile(t, wpath, "go.mod", "module example.test/seed\n\ngo 1.24\n")

	r.installSessionDependencies(context.Background(), depsTestQueuedWork("DEPS-ORDER"), wpath, false)

	calls := execer.recorded()
	if len(calls) != 2 {
		t.Fatalf("install calls = %d; want 2 (pnpm then go)", len(calls))
	}
	if calls[0].command != pnpmDepsInstallCommand {
		t.Errorf("first call = %q; want %q", calls[0].command, pnpmDepsInstallCommand)
	}
	if calls[1].command != goDepsInstallCommand {
		t.Errorf("second call = %q; want %q", calls[1].command, goDepsInstallCommand)
	}
	for i, call := range calls {
		if call.dir != wpath {
			t.Errorf("call %d dir = %q; want worktree %q", i, call.dir, wpath)
		}
	}
}

func TestInstallSessionDependencies_SkipsPnpmWhenNodeModulesPresent(t *testing.T) {
	t.Parallel()
	execer := &recordingDepsExecer{}
	r := depsTestRunner(t, execer)

	wpath := t.TempDir()
	writeFile(t, wpath, "pnpm-lock.yaml", "lockfileVersion: 9.0\n")
	writeFile(t, wpath, filepath.Join("node_modules", ".bin", "keep"), "")

	r.installSessionDependencies(context.Background(), depsTestQueuedWork("DEPS-SKIP"), wpath, false)

	if calls := execer.recorded(); len(calls) != 0 {
		t.Fatalf("install calls = %v; want none (kit hook already installed)", calls)
	}
}

func TestInstallSessionDependencies_SkipsOnlyInstalledEcosystem(t *testing.T) {
	t.Parallel()
	execer := &recordingDepsExecer{}
	r := depsTestRunner(t, execer)

	wpath := t.TempDir()
	writeFile(t, wpath, "pnpm-lock.yaml", "lockfileVersion: 9.0\n")
	writeFile(t, wpath, filepath.Join("node_modules", ".bin", "keep"), "")
	writeFile(t, wpath, "go.mod", "module example.test/seed\n\ngo 1.24\n")

	r.installSessionDependencies(context.Background(), depsTestQueuedWork("DEPS-PARTIAL"), wpath, false)

	calls := execer.recorded()
	if len(calls) != 1 {
		t.Fatalf("install calls = %v; want only the go install", calls)
	}
	if calls[0].command != goDepsInstallCommand || calls[0].dir != wpath {
		t.Errorf("call = %+v; want go install in %q", calls[0], wpath)
	}
}

func TestInstallSessionDependencies_NoManifestsRunsNothing(t *testing.T) {
	t.Parallel()
	execer := &recordingDepsExecer{}
	r := depsTestRunner(t, execer)

	wpath := t.TempDir()
	writeFile(t, wpath, "README.md", "# no manifests here\n")

	r.installSessionDependencies(context.Background(), depsTestQueuedWork("DEPS-NONE"), wpath, false)

	if calls := execer.recorded(); len(calls) != 0 {
		t.Fatalf("install calls = %v; want none", calls)
	}
}

func TestInstallSessionDependencies_ReadOnlyCheckoutSkips(t *testing.T) {
	t.Parallel()
	execer := &recordingDepsExecer{}
	r := depsTestRunner(t, execer)

	wpath := t.TempDir()
	writeFile(t, wpath, "pnpm-lock.yaml", "lockfileVersion: 9.0\n")
	writeFile(t, wpath, "go.mod", "module example.test/seed\n\ngo 1.24\n")

	r.installSessionDependencies(context.Background(), depsTestQueuedWork("DEPS-RO"), wpath, true)

	if calls := execer.recorded(); len(calls) != 0 {
		t.Fatalf("install calls = %v; want none on a read-only checkout", calls)
	}
}

func TestInstallSessionDependencies_FailureContinuesToNextEcosystem(t *testing.T) {
	t.Parallel()
	execer := &recordingDepsExecer{exitCode: 1}
	r := depsTestRunner(t, execer)

	wpath := t.TempDir()
	writeFile(t, wpath, "pnpm-lock.yaml", "lockfileVersion: 9.0\n")
	writeFile(t, wpath, "go.mod", "module example.test/seed\n\ngo 1.24\n")

	// Must not panic, abort, or return an error: a failed install is
	// logged and the run continues with whatever is on disk.
	r.installSessionDependencies(context.Background(), depsTestQueuedWork("DEPS-FAIL"), wpath, false)

	calls := execer.recorded()
	if len(calls) != 2 {
		t.Fatalf("install calls = %d; want 2 (failure must not skip go)", len(calls))
	}
}

func TestInstallSessionDependencies_ExecErrorContinues(t *testing.T) {
	t.Parallel()
	execer := &recordingDepsExecer{err: errors.New("binary missing")}
	r := depsTestRunner(t, execer)

	wpath := t.TempDir()
	writeFile(t, wpath, "go.mod", "module example.test/seed\n\ngo 1.24\n")

	r.installSessionDependencies(context.Background(), depsTestQueuedWork("DEPS-ERR"), wpath, false)

	if calls := execer.recorded(); len(calls) != 1 {
		t.Fatalf("install calls = %d; want 1", len(calls))
	}
}

// TestInstallSessionDependencies_SkipsWhenNetworkNone pins the level
// gate's network half: a stamp that withholds the network skips the
// install without invoking the execer, even with both manifests on
// disk. Reverting the gate to run unconditionally turns this red (2
// calls instead of 0).
func TestInstallSessionDependencies_SkipsWhenNetworkNone(t *testing.T) {
	t.Parallel()
	execer := &recordingDepsExecer{}
	r := depsTestRunner(t, execer)

	wpath := t.TempDir()
	writeFile(t, wpath, "pnpm-lock.yaml", "lockfileVersion: 9.0\n")
	writeFile(t, wpath, "go.mod", "module example.test/seed\n\ngo 1.24\n")

	qw := depsTestQueuedWork("DEPS-NET-NONE")
	qw.ExecutionSecurity.Levels.Network = agent.NetworkNone
	r.installSessionDependencies(context.Background(), qw, wpath, false)

	if calls := execer.recorded(); len(calls) != 0 {
		t.Fatalf("install calls = %v; want none (network none withholds the fetch)", calls)
	}
}

// TestInstallSessionDependencies_SkipsWhenFileWriteUnknown pins the
// level gate's closed shape: a level off the ladder refuses the step
// instead of running it on a guess. Reverting the gate to permit
// unknown levels turns this red.
func TestInstallSessionDependencies_SkipsWhenFileWriteUnknown(t *testing.T) {
	t.Parallel()
	execer := &recordingDepsExecer{}
	r := depsTestRunner(t, execer)

	wpath := t.TempDir()
	writeFile(t, wpath, "go.mod", "module example.test/seed\n\ngo 1.24\n")

	qw := depsTestQueuedWork("DEPS-BAD-LEVEL")
	qw.ExecutionSecurity.Levels.FileWrite = agent.ExecutionSecurityLevel("everywhere")
	r.installSessionDependencies(context.Background(), qw, wpath, false)

	if calls := execer.recorded(); len(calls) != 0 {
		t.Fatalf("install calls = %v; want none (unknown level refuses the step)", calls)
	}
}

// TestInstallSessionDependencies_PnpmDisablesScripts pins the script
// hardening: the pnpm step carries --ignore-scripts and
// --ignore-pnpmfile so repository lifecycle hooks and hooks files never
// execute during the fetch. Removing either flag turns this red.
func TestInstallSessionDependencies_PnpmDisablesScripts(t *testing.T) {
	t.Parallel()
	for _, flag := range []string{"--ignore-scripts", "--ignore-pnpmfile"} {
		if !strings.Contains(pnpmDepsInstallCommand, flag) {
			t.Errorf("pnpm install command = %q; want flag %q", pnpmDepsInstallCommand, flag)
		}
		found := false
		for _, arg := range pnpmDepsInstallArgv {
			if arg == flag {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("pnpm install argv = %q; want flag %q", pnpmDepsInstallArgv, flag)
		}
	}
}

// TestInstallSessionDependencies_MinimalEnvDropsCredentials pins the
// minimal-env boundary: the install env carries PATH/HOME and the
// caches but never the worker bearer, session identifiers, or provider
// credentials, even when the session env is saturated with them.
// Widening the allow list to pass a credential through turns this red.
func TestInstallSessionDependencies_MinimalEnvDropsCredentials(t *testing.T) {
	t.Parallel()
	seatCaps := map[string]string{
		"PATH":                "/tool/bin:/usr/bin:/bin",
		"WORKER_AUTH_TOKEN":   "worker-bearer",
		"GOMAXPROCS":          "4",
		"DONMAI_SESSION_ID":   "session-123",
		"ANTHROPIC_API_KEY":   "provider-key",
		"OPENAI_API_KEY":      "provider-key-2",
		"HTTP_PROXY":          "http://proxy.internal:8080",
		"NPM_CONFIG_REGISTRY": "https://mirror.internal/",
	}
	host := map[string]string{"PATH": "/usr/bin:/bin", "HOME": "/home/daemon"}
	env := depsInstallEnv("/home/daemon", seatCaps, host, nil)
	for _, leaked := range []string{
		"WORKER_AUTH_TOKEN", "DONMAI_SESSION_ID", "ANTHROPIC_API_KEY", "OPENAI_API_KEY",
		"HTTP_PROXY", "NPM_CONFIG_REGISTRY",
	} {
		if value, ok := env[leaked]; ok {
			t.Errorf("install env carries %s=%q; want it dropped", leaked, value)
		}
	}
	if env["PATH"] != "/tool/bin:/usr/bin:/bin" {
		t.Errorf("install PATH = %q; want the seat PATH", env["PATH"])
	}
	if env["HOME"] != "/home/daemon" {
		t.Errorf("install HOME = %q; want the daemon home", env["HOME"])
	}
	if env["GOMAXPROCS"] != "4" {
		t.Errorf("install GOMAXPROCS = %q; want the seat share", env["GOMAXPROCS"])
	}
}

// TestInstallSessionDependencies_FakeSeesMinimalEnv pins the boundary
// at the execer seam: the env map the step hands the execer carries no
// credential, so a fake asserting on it proves the wiring, not just
// the helper.
func TestInstallSessionDependencies_FakeSeesMinimalEnv(t *testing.T) {
	t.Parallel()
	execer := &recordingDepsExecer{}
	r := depsTestRunner(t, execer)

	wpath := t.TempDir()
	writeFile(t, wpath, "go.mod", "module example.test/seed\n\ngo 1.24\n")

	qw := depsTestQueuedWork("DEPS-ENV-SEAM")
	qw.AuthToken = "worker-bearer-should-not-propagate"
	r.installSessionDependencies(context.Background(), qw, wpath, false)

	calls := execer.recorded()
	if len(calls) != 1 {
		t.Fatalf("install calls = %d; want 1", len(calls))
	}
	for _, leaked := range []string{"WORKER_AUTH_TOKEN", "DONMAI_SESSION_ID", "ANTHROPIC_API_KEY"} {
		if value, ok := calls[0].env[leaked]; ok {
			t.Errorf("execer env carries %s=%q; want it dropped", leaked, value)
		}
	}
	if calls[0].command != goDepsInstallCommand {
		t.Errorf("call command = %q; want %q", calls[0].command, goDepsInstallCommand)
	}
}

// orderLog is a mutex-guarded event log shared between the fake
// dependency execer and the spawn-recording provider wrapper so a
// Run-level test can assert the install ran before spawn.
type orderLog struct {
	mu     sync.Mutex
	events []string
}

func (l *orderLog) add(event string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.events = append(l.events, event)
}

func (l *orderLog) snapshot() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]string(nil), l.events...)
}

type orderRecordingDepsExecer struct {
	log *orderLog
}

func (f *orderRecordingDepsExecer) Exec(_ context.Context, dir, command string, _ map[string]string) (int, error) {
	f.log.add("exec:" + command + " dir=" + dir)
	return 0, nil
}

// spawnRecordingProvider delegates to the stub provider while
// recording the Spawn call on the shared order log.
type spawnRecordingProvider struct {
	agent.Provider
	log *orderLog
}

func (p *spawnRecordingProvider) Spawn(ctx context.Context, spec agent.Spec) (agent.Handle, error) {
	p.log.add("spawn")
	return p.Provider.Spawn(ctx, spec)
}

// TestRun_InstallsDependenciesBeforeSpawn is the revert-RED test for
// the post-acquire dependency install step: the fake execer records
// the exact install command and working directory, and the shared log
// proves the install ran before the provider spawned. Removing the
// installSessionDependencies call from runLoop leaves the log at
// ["spawn"] and this test fails.
func TestRun_InstallsDependenciesBeforeSpawn(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	h := newRunnerHarness(t)
	log := &orderLog{}
	h.runner.depsExecer = &orderRecordingDepsExecer{log: log}

	stubProvider, err := stub.New()
	if err != nil {
		t.Fatalf("stub.New: %v", err)
	}
	if err := h.runner.registry.Register(&spawnRecordingProvider{Provider: stubProvider, log: log}); err != nil {
		t.Fatalf("Register: %v", err)
	}

	qw := h.queuedWork("DEPS-BEFORE-SPAWN")
	qw.ExecutionSecurity = depsTestLevels()
	seedManifestsInBareRepo(t, h.bareRepo)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	res, err := h.runner.Run(ctx, qw)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Status != "completed" {
		t.Fatalf("Status = %q; want completed (FailureMode=%q, Error=%q)", res.Status, res.FailureMode, res.Error)
	}

	events := log.snapshot()
	wantPnpm := "exec:" + pnpmDepsInstallCommand + " dir=" + res.WorktreePath
	wantGo := "exec:" + goDepsInstallCommand + " dir=" + res.WorktreePath
	switch {
	case len(events) != 3:
		t.Fatalf("order log = %v; want [%q %q spawn]", events, wantPnpm, wantGo)
	case events[0] != wantPnpm:
		t.Errorf("first event = %q; want %q (pnpm runs before go)", events[0], wantPnpm)
	case events[1] != wantGo:
		t.Errorf("second event = %q; want %q", events[1], wantGo)
	case events[2] != "spawn":
		t.Errorf("third event = %q; want spawn (install runs before spawn)", events[2])
	}
}

// TestRun_DependencyInstallFailureStillSpawns proves the install step
// never fails the session: a non-zero install exit still leads to a
// completed run.
func TestRun_DependencyInstallFailureStillSpawns(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	h := newRunnerHarness(t)
	h.runner.depsExecer = &recordingDepsExecer{exitCode: 1}

	qw := h.queuedWork("DEPS-FAIL-SPAWNS")
	qw.ExecutionSecurity = depsTestLevels()
	seedManifestsInBareRepo(t, h.bareRepo)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	res, err := h.runner.Run(ctx, qw)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Status != "completed" {
		t.Errorf("Status = %q; want completed (install failure must not fail the session)", res.Status)
	}
}

// TestDepsInstallExecerRunsNoShell pins the direct-exec shape: the
// production execer resolves the binary on the call env's PATH and
// passes the remaining words as argv, never through a shell — so a
// repository file can only feed arguments, never a command of its
// own. The fake pnpm records the argv it received; --ignore-scripts
// and --ignore-pnpmfile ride along so repository lifecycle hooks stay
// disabled even where the binary would otherwise run them.
func TestDepsInstallExecerRunsNoShell(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	fakes := t.TempDir()
	report := filepath.Join(t.TempDir(), "argv")
	fakePnpm := "#!/bin/sh\nprintf '%s' \"$*\" > \"$REPORT_PATH\"\nexit 0\n"
	if err := os.WriteFile(filepath.Join(fakes, "pnpm"), []byte(fakePnpm), 0o700); err != nil { //nolint:gosec // G306: test-owned fake binary; the exec bit is the point (PATH lookup + exec need +x)
		t.Fatal(err)
	}

	execer := depsInstallExecer{}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	code, err := execer.Exec(ctx, dir, pnpmDepsInstallCommand, map[string]string{
		"PATH": fakes + ":/usr/bin:/bin", "HOME": t.TempDir(), "REPORT_PATH": report,
	})
	if err != nil || code != 0 {
		t.Fatalf("Exec = (%d, %v); want (0, nil)", code, err)
	}
	raw, err := os.ReadFile(report)
	if err != nil {
		t.Fatalf("fake pnpm never ran (no argv record): %v", err)
	}
	want := "install --prefer-offline --frozen-lockfile --ignore-scripts --ignore-pnpmfile"
	if strings.TrimSpace(string(raw)) != want {
		t.Errorf("pnpm argv = %q; want %q (no shell, scripts disabled)", raw, want)
	}
}

// TestDepsInstallExecerCapturesOutputOnFailure pins the bounded-output
// contract: a failing install's combined output tail reaches the log
// so a failure is diagnosable without failing the session. The fake
// exits non-zero after printing a marker; reverting the capture to
// discard the output leaves the log without it and turns this red.
func TestDepsInstallExecerCapturesOutputOnFailure(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	fakes := t.TempDir()
	fakeGo := "#!/bin/sh\necho 'marker-fetch-failed-timeline'\nexit 1\n"
	if err := os.WriteFile(filepath.Join(fakes, "go"), []byte(fakeGo), 0o700); err != nil { //nolint:gosec // G306: test-owned fake binary; the exec bit is the point (PATH lookup + exec need +x)
		t.Fatal(err)
	}

	rec := &sliceWarnLogger{}
	execer := depsInstallExecer{logger: rec}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	code, err := execer.Exec(ctx, dir, goDepsInstallCommand, map[string]string{
		"PATH": fakes + ":/usr/bin:/bin", "HOME": t.TempDir(),
	})
	if err != nil || code != 1 {
		t.Fatalf("Exec = (%d, %v); want (1, nil)", code, err)
	}
	rec.mu.Lock()
	defer rec.mu.Unlock()
	for _, line := range rec.lines {
		if strings.Contains(line, "marker-fetch-failed-timeline") {
			return
		}
	}
	t.Errorf("failure log = %q; want the install output tail", rec.lines)
}

// sliceWarnLogger is a depsInstallExecer logger fake that records Warn lines.
type sliceWarnLogger struct {
	mu    sync.Mutex
	lines []string
}

func (l *sliceWarnLogger) Warn(_ string, args ...any) {
	joined := make([]string, 0, len(args))
	for _, arg := range args {
		joined = append(joined, sprintWarnArg(arg))
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.lines = append(l.lines, strings.Join(joined, " "))
}

func sprintWarnArg(v any) string {
	switch typed := v.(type) {
	case string:
		return typed
	case error:
		return typed.Error()
	case interface{ String() string }:
		return typed.String()
	default:
		return ""
	}
}

// TestDepsInstallExecerDecodesOnlyKnownSteps pins the closed argv
// decode: only the two runner-owned steps execute; anything else —
// including a shell metacharacter smuggled into the command string —
// is refused before any process starts. Reverting to a shell split
// (strings.Fields) lets `go mod download; touch PWNED` resolve to a
// `go` binary and turns the second half of this red.
func TestDepsInstallExecerDecodesOnlyKnownSteps(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	execer := depsInstallExecer{}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	// PATH carries only the test fakes plus the shell: even a
	// shell-split decode can only resolve what is on PATH, so the
	// fake `go` (which records its argv) proves exactly what would
	// have executed.
	fakes := t.TempDir()
	argvRecord := filepath.Join(t.TempDir(), "decode-argv")
	fakeGo := "#!/bin/sh\nprintf '%s' \"$*\" > \"$DECODE_ARGV\"\nexit 0\n"
	if err := os.WriteFile(filepath.Join(fakes, "go"), []byte(fakeGo), 0o700); err != nil { //nolint:gosec // G306: test-owned fake binary; the exec bit is the point (PATH lookup + exec need +x)
		t.Fatal(err)
	}
	callEnv := map[string]string{"PATH": fakes + ":/usr/bin:/bin", "HOME": t.TempDir(), "DECODE_ARGV": argvRecord}
	if _, err := execer.Exec(ctx, dir, "go mod download; touch PWNED", callEnv); err == nil {
		t.Errorf("injected command ran without an error; want a decode refusal")
	}
	if _, statErr := os.Stat(filepath.Join(dir, "PWNED")); statErr == nil {
		t.Errorf("injected side effect executed; the decode must refuse before starting anything")
	}
	if raw, readErr := os.ReadFile(argvRecord); readErr == nil {
		t.Errorf("fake go ran with argv %q; the decode must refuse before starting anything", raw)
	}
	if _, err := execer.Exec(ctx, dir, "pnpm install --prefer-offline", callEnv); err == nil {
		t.Errorf("prefix-shortened pnpm command ran without an error; want a decode refusal")
	}
}

// seedManifestsInBareRepo commits a pnpm lockfile and a go.mod into
// the harness's bare source repo so clones carry both manifests.
func seedManifestsInBareRepo(t *testing.T, bareRepo string) {
	t.Helper()
	work := t.TempDir()
	runGitFixture(t, "", "clone", "-q", bareRepo, work)
	writeFile(t, work, "pnpm-lock.yaml", "lockfileVersion: 9.0\n")
	writeFile(t, work, "go.mod", "module example.test/seed\n\ngo 1.24\n")
	for _, args := range [][]string{
		{"-C", work, "config", "user.email", "test@example.com"},
		{"-C", work, "config", "user.name", "test"},
		{"-C", work, "add", "pnpm-lock.yaml", "go.mod"},
		{"-C", work, "commit", "-q", "-m", "seed dependency manifests"},
		{"-C", work, "push", "-q", "origin", "HEAD:main"},
	} {
		runGitFixture(t, "", args...)
	}
}

func runGitFixture(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...) //nolint:gosec // test fixture with caller-supplied args
	if dir != "" {
		cmd.Dir = dir
	}
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}
