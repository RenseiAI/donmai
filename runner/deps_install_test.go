package runner

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/RenseiAI/donmai/agent"
	"github.com/RenseiAI/donmai/internal/testexec"
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
}

func (f *recordingDepsExecer) Exec(_ context.Context, dir, command string, _ map[string]string) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, depsCall{dir: dir, command: command})
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
	h.runner.logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	h.runner.depsExecer = execer
	return h.runner
}

func TestInstallSessionDependencies_PnpmBeforeGo(t *testing.T) {
	t.Parallel()
	execer := &recordingDepsExecer{}
	r := depsTestRunner(t, execer)

	wpath := t.TempDir()
	writeFile(t, wpath, "pnpm-lock.yaml", "lockfileVersion: 9.0\n")
	writeFile(t, wpath, "go.mod", "module example.test/seed\n\ngo 1.24\n")

	prep := r.installSessionDependencies(context.Background(), QueuedWork{QueuedWork: queuedWorkBase("DEPS-ORDER")}, wpath, false)

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
	if prep == nil || prep.Status != agent.DependencyPreparationReady {
		t.Fatalf("preparation = %+v; want ready", prep)
	}
	if len(prep.Ecosystems) != 2 || prep.Ecosystems[0] != "pnpm" || prep.Ecosystems[1] != "go" {
		t.Errorf("ecosystems = %v; want [pnpm go]", prep.Ecosystems)
	}
}

func TestInstallSessionDependencies_RunsPnpmDespiteNodeModulesPresent(t *testing.T) {
	t.Parallel()
	execer := &recordingDepsExecer{}
	r := depsTestRunner(t, execer)

	wpath := t.TempDir()
	writeFile(t, wpath, "pnpm-lock.yaml", "lockfileVersion: 9.0\n")
	writeFile(t, wpath, filepath.Join("node_modules", ".bin", "keep"), "")

	// A leftover .bin directory proves nothing about the checked-out
	// lockfile: the existing installer still runs before spawn. Restoring
	// the .bin skip leaves zero calls and fails this test (AC-1 RED).
	prep := r.installSessionDependencies(context.Background(), QueuedWork{QueuedWork: queuedWorkBase("DEPS-SKIP")}, wpath, false)

	calls := execer.recorded()
	if len(calls) != 1 {
		t.Fatalf("install calls = %v; want the pnpm install despite .bin", calls)
	}
	if calls[0].command != pnpmDepsInstallCommand || calls[0].dir != wpath {
		t.Errorf("call = %+v; want pnpm install in %q", calls[0], wpath)
	}
	if prep == nil || prep.Status != agent.DependencyPreparationReady {
		t.Fatalf("preparation = %+v; want ready", prep)
	}
}

func TestInstallSessionDependencies_RunsBothEcosystemsDespiteNodeModulesPresent(t *testing.T) {
	t.Parallel()
	execer := &recordingDepsExecer{}
	r := depsTestRunner(t, execer)

	wpath := t.TempDir()
	writeFile(t, wpath, "pnpm-lock.yaml", "lockfileVersion: 9.0\n")
	writeFile(t, wpath, filepath.Join("node_modules", ".bin", "keep"), "")
	writeFile(t, wpath, "go.mod", "module example.test/seed\n\ngo 1.24\n")

	prep := r.installSessionDependencies(context.Background(), QueuedWork{QueuedWork: queuedWorkBase("DEPS-PARTIAL")}, wpath, false)

	calls := execer.recorded()
	if len(calls) != 2 {
		t.Fatalf("install calls = %v; want pnpm then go despite .bin", calls)
	}
	if calls[0].command != pnpmDepsInstallCommand || calls[0].dir != wpath {
		t.Errorf("first call = %+v; want pnpm install in %q", calls[0], wpath)
	}
	if calls[1].command != goDepsInstallCommand || calls[1].dir != wpath {
		t.Errorf("second call = %+v; want go install in %q", calls[1], wpath)
	}
	if prep == nil || prep.Status != agent.DependencyPreparationReady {
		t.Fatalf("preparation = %+v; want ready", prep)
	}
}

func TestInstallSessionDependencies_NoManifestsRunsNothing(t *testing.T) {
	t.Parallel()
	execer := &recordingDepsExecer{}
	r := depsTestRunner(t, execer)

	wpath := t.TempDir()
	writeFile(t, wpath, "README.md", "# no manifests here\n")

	prep := r.installSessionDependencies(context.Background(), QueuedWork{QueuedWork: queuedWorkBase("DEPS-NONE")}, wpath, false)

	if calls := execer.recorded(); len(calls) != 0 {
		t.Fatalf("install calls = %v; want none", calls)
	}
	if prep != nil {
		t.Fatalf("preparation = %+v; want nil (omitted with no detected ecosystem)", prep)
	}
}

func TestInstallSessionDependencies_ReadOnlyCheckoutSkips(t *testing.T) {
	t.Parallel()
	execer := &recordingDepsExecer{}
	r := depsTestRunner(t, execer)

	wpath := t.TempDir()
	writeFile(t, wpath, "pnpm-lock.yaml", "lockfileVersion: 9.0\n")
	writeFile(t, wpath, "go.mod", "module example.test/seed\n\ngo 1.24\n")

	prep := r.installSessionDependencies(context.Background(), QueuedWork{QueuedWork: queuedWorkBase("DEPS-RO")}, wpath, true)

	if calls := execer.recorded(); len(calls) != 0 {
		t.Fatalf("install calls = %v; want none on a read-only checkout", calls)
	}
	if prep == nil || prep.Status != agent.DependencyPreparationSkippedReadOnly {
		t.Fatalf("preparation = %+v; want skipped_read_only", prep)
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
	prep := r.installSessionDependencies(context.Background(), QueuedWork{QueuedWork: queuedWorkBase("DEPS-FAIL")}, wpath, false)

	calls := execer.recorded()
	if len(calls) != 2 {
		t.Fatalf("install calls = %d; want 2 (failure must not skip go)", len(calls))
	}
	if prep == nil || prep.Status != agent.DependencyPreparationFailed {
		t.Fatalf("preparation = %+v; want failed", prep)
	}
}

func TestInstallSessionDependencies_ExecErrorContinues(t *testing.T) {
	t.Parallel()
	execer := &recordingDepsExecer{err: errors.New("binary missing")}
	r := depsTestRunner(t, execer)

	wpath := t.TempDir()
	writeFile(t, wpath, "go.mod", "module example.test/seed\n\ngo 1.24\n")

	prep := r.installSessionDependencies(context.Background(), QueuedWork{QueuedWork: queuedWorkBase("DEPS-ERR")}, wpath, false)

	if calls := execer.recorded(); len(calls) != 1 {
		t.Fatalf("install calls = %d; want 1", len(calls))
	}
	if prep == nil || prep.Status != agent.DependencyPreparationFailed {
		t.Fatalf("preparation = %+v; want failed", prep)
	}
}

// contextBlockingExecer blocks until ctx expires, then reports the
// context error like the production execer does on timeout.
type contextBlockingExecer struct {
	mu    sync.Mutex
	calls int
}

func (f *contextBlockingExecer) Exec(ctx context.Context, _, _ string, _ map[string]string) (int, error) {
	f.mu.Lock()
	f.calls++
	f.mu.Unlock()
	<-ctx.Done()
	return -1, ctx.Err()
}

func (f *contextBlockingExecer) recorded() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

func TestInstallSessionDependencies_ExpiredContextReportsTimedOut(t *testing.T) {
	t.Parallel()
	execer := &contextBlockingExecer{}
	r := depsTestRunner(t, execer)
	r.depsInstallTimeout = 50 * time.Millisecond

	wpath := t.TempDir()
	writeFile(t, wpath, "go.mod", "module example.test/seed\n\ngo 1.24\n")

	prep := r.installSessionDependencies(context.Background(), QueuedWork{QueuedWork: queuedWorkBase("DEPS-TIMEOUT")}, wpath, false)

	if prep == nil || prep.Status != agent.DependencyPreparationTimedOut {
		t.Fatalf("preparation = %+v; want timed_out", prep)
	}
	if n := execer.recorded(); n != 1 {
		t.Fatalf("install calls = %d; want 1 bounded attempt", n)
	}
	if len(prep.Ecosystems) != 1 || prep.Ecosystems[0] != "go" {
		t.Errorf("ecosystems = %v; want [go]", prep.Ecosystems)
	}
}

func TestInstallSessionDependencies_CancelledContextAuthorizesNoWork(t *testing.T) {
	t.Parallel()
	execer := &recordingDepsExecer{}
	r := depsTestRunner(t, execer)

	wpath := t.TempDir()
	writeFile(t, wpath, "pnpm-lock.yaml", "lockfileVersion: 9.0\n")
	writeFile(t, wpath, "go.mod", "module example.test/seed\n\ngo 1.24\n")

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	prep := r.installSessionDependencies(ctx, QueuedWork{QueuedWork: queuedWorkBase("DEPS-CANCEL")}, wpath, false)

	if calls := execer.recorded(); len(calls) != 0 {
		t.Fatalf("install calls = %v; want none: cancellation must not authorize work", calls)
	}
	if prep == nil || prep.Status != agent.DependencyPreparationTimedOut {
		t.Fatalf("preparation = %+v; want timed_out", prep)
	}
}

func TestInstallSessionDependencies_RestrictedContextReportsSkippedRestricted(t *testing.T) {
	t.Parallel()
	execer := &recordingDepsExecer{}
	r := depsTestRunner(t, execer)

	wpath := t.TempDir()
	writeFile(t, wpath, "pnpm-lock.yaml", "lockfileVersion: 9.0\n")
	writeFile(t, wpath, "go.mod", "module example.test/seed\n\ngo 1.24\n")

	qw := QueuedWork{QueuedWork: queuedWorkBase("DEPS-RESTRICTED")}
	qw.ExecutionSecurity = executionSecuritySection(nil)
	qw.ExecutionSecurity.Levels.Network = agent.NetworkLogged

	prep := r.installSessionDependencies(context.Background(), qw, wpath, false)

	if calls := execer.recorded(); len(calls) != 0 {
		t.Fatalf("install calls = %v; want none under restricted execution", calls)
	}
	if prep == nil || prep.Status != agent.DependencyPreparationSkippedRestricted {
		t.Fatalf("preparation = %+v; want skipped_restricted", prep)
	}
	if len(prep.Ecosystems) != 2 {
		t.Errorf("ecosystems = %v; want both detected ecosystems on the skipped report", prep.Ecosystems)
	}
}

func TestInjectDependencyPreparationPartial(t *testing.T) {
	t.Parallel()
	base := "# Operating protocol\nFollow the harness protocol."
	for _, tc := range []struct {
		name       string
		prep       *agent.DependencyPreparation
		wantChange bool
	}{
		{name: "nil omits", prep: nil},
		{name: "ready adds nothing", prep: &agent.DependencyPreparation{Status: agent.DependencyPreparationReady, Ecosystems: []string{"pnpm"}}},
		{name: "read-only skip adds no bypass", prep: &agent.DependencyPreparation{Status: agent.DependencyPreparationSkippedReadOnly, Ecosystems: []string{"pnpm"}}},
		{name: "restricted skip adds no bypass", prep: &agent.DependencyPreparation{Status: agent.DependencyPreparationSkippedRestricted, Ecosystems: []string{"go"}}},
		{name: "failed hands recovery", prep: &agent.DependencyPreparation{Status: agent.DependencyPreparationFailed, Ecosystems: []string{"pnpm"}}, wantChange: true},
		{name: "timed out hands recovery", prep: &agent.DependencyPreparation{Status: agent.DependencyPreparationTimedOut, Ecosystems: []string{"go"}}, wantChange: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := injectDependencyPreparationPartial(base, tc.prep)
			if !tc.wantChange {
				if got != base {
					t.Fatalf("protocol changed for %+v; want byte-identical", tc.prep)
				}
				return
			}
			if got == base {
				t.Fatalf("protocol unchanged for %+v; want recovery handoff", tc.prep)
			}
			if !strings.Contains(got, base) {
				t.Fatalf("existing protocol lost in recovery handoff")
			}
			if !strings.Contains(got, "Dependencies are not ready.") {
				t.Errorf("recovery handoff missing not-ready statement: %q", got)
			}
			if !strings.Contains(got, "report blocked with the reason") {
				t.Errorf("recovery handoff missing blocked report: %q", got)
			}
			if !strings.Contains(got, tc.prep.Status) {
				t.Errorf("recovery handoff missing bounded status %q: %q", tc.prep.Status, got)
			}
			for _, sentinel := range []string{"binary missing", "install-stdout", "exit 1", "/bin/sh"} {
				if strings.Contains(got, sentinel) {
					t.Errorf("recovery prompt leaks raw installer text %q: %q", sentinel, got)
				}
			}
		})
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
	if strings.HasPrefix(command, "pnpm install ") {
		if err := os.MkdirAll(filepath.Join(dir, "node_modules", ".bin"), 0o750); err != nil {
			return -1, err
		}
	}
	return 0, nil
}

// spawnRecordingProvider delegates to the stub provider while
// recording the Spawn call on the shared order log.
type spawnRecordingProvider struct {
	agent.Provider
	log         *orderLog
	requireBins bool
}

func (p *spawnRecordingProvider) Spawn(ctx context.Context, spec agent.Spec) (agent.Handle, error) {
	p.log.add("spawn")
	if p.requireBins {
		if info, err := os.Stat(filepath.Join(spec.Cwd, "node_modules", ".bin")); err != nil || !info.IsDir() {
			return nil, errors.New("dependency bins missing before spawn")
		}
	}
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
	h.runner.logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	log := &orderLog{}
	h.runner.depsExecer = &orderRecordingDepsExecer{log: log}

	stubProvider, err := stub.New()
	if err != nil {
		t.Fatalf("stub.New: %v", err)
	}
	if err := h.runner.registry.Register(&spawnRecordingProvider{Provider: stubProvider, log: log, requireBins: true}); err != nil {
		t.Fatalf("Register: %v", err)
	}

	qw := h.queuedWork("DEPS-BEFORE-SPAWN")
	qw.ExecutionSecurity = executionSecuritySection(nil)
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
	wantPnpm := "exec:pnpm install --prefer-offline --frozen-lockfile --ignore-scripts --ignore-pnpmfile dir=" + res.WorktreePath
	wantGo := "exec:" + goDepsInstallCommand + " dir=" + res.WorktreePath
	switch {
	case len(events) != 3:
		t.Fatalf("order log has %d events; want pnpm, go, spawn", len(events))
	case events[0] != wantPnpm:
		t.Error("first event is not the expected pnpm command and worktree directory")
	case events[1] != wantGo:
		t.Error("second event is not go mod download in the worktree directory")
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
	h.runner.logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	h.runner.depsExecer = &recordingDepsExecer{exitCode: 1}

	qw := h.queuedWork("DEPS-FAIL-SPAWNS")
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

// A manifest fixture lets the harness render restricted levels so the test
// reaches the install gate instead of stopping at runner preflight.
type depsSecurityProvider struct {
	agent.HarnessProvider
	dimension agent.ExecutionSecurityDimension
	level     agent.ExecutionSecurityLevel
}

func (p *depsSecurityProvider) Manifest() agent.HarnessManifest {
	m := p.HarnessProvider.Manifest()
	m.ExecutionSecurity.Levels = []agent.RenderedExecutionSecurityLevel{{Dimension: p.dimension, Level: p.level, Modes: []agent.PromptSessionMode{agent.PromptModeAutonomous}, Layers: []agent.EnforcingLayer{agent.LayerHarnessNative}, DenyBaseline: agent.DenyBaselineEnforced}}
	return m
}

func TestRun_DependencyInstallSecurityGate(t *testing.T) {
	for _, tc := range []struct {
		dimension agent.ExecutionSecurityDimension
		level     agent.ExecutionSecurityLevel
	}{
		{agent.ExecutionSecurityNetwork, agent.NetworkLogged},
		{agent.ExecutionSecurityFileWrite, agent.FileWriteWorkarea},
		{agent.ExecutionSecurityFileRead, agent.FileReadWorkarea},
		{agent.ExecutionSecurityCredentials, agent.CredentialsInjectedOnly},
		{agent.ExecutionSecurityIsolation, agent.IsolationOSSandbox},
		{agent.ExecutionSecurityToolApproval, agent.ToolApprovalDenyList},
	} {
		t.Run(string(tc.dimension), func(t *testing.T) {
			h := newRunnerHarness(t)
			h.runner.logger = slog.New(slog.NewTextHandler(io.Discard, nil))
			fake := &recordingDepsExecer{}
			h.runner.depsExecer = fake
			p, err := stub.New()
			if err != nil {
				t.Fatal(err)
			}
			if err := h.runner.registry.Register(&depsSecurityProvider{HarnessProvider: p.(agent.HarnessProvider), dimension: tc.dimension, level: tc.level}); err != nil {
				t.Fatal(err)
			}
			qw := h.queuedWork("dependency-security")
			qw.ExecutionSecurity = executionSecuritySection(nil)
			switch tc.dimension {
			case agent.ExecutionSecurityNetwork:
				qw.ExecutionSecurity.Levels.Network = tc.level
			case agent.ExecutionSecurityFileWrite:
				qw.ExecutionSecurity.Levels.FileWrite = tc.level
			case agent.ExecutionSecurityFileRead:
				qw.ExecutionSecurity.Levels.FileRead = tc.level
			case agent.ExecutionSecurityCredentials:
				qw.ExecutionSecurity.Levels.Credentials = tc.level
			case agent.ExecutionSecurityIsolation:
				qw.ExecutionSecurity.Levels.Isolation = tc.level
			case agent.ExecutionSecurityToolApproval:
				qw.ExecutionSecurity.Levels.ToolApproval = tc.level
			}
			seedManifestsInBareRepo(t, h.bareRepo)
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			res, err := h.runner.Run(ctx, qw)
			if err != nil || res.Status != "completed" {
				t.Fatalf("Run status=%s err=%v detail=%s", res.Status, err, res.Error)
			}
			if calls := fake.recorded(); len(calls) != 0 {
				t.Fatalf("restricted session ran %d install commands", len(calls))
			}
		})
	}
}

func fakeDepsBinary(t *testing.T, name, script string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "fixture")
	if err := testexec.WriteFile(path, []byte("#!/bin/sh\n"+script), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	chmod := exec.Command("chmod", "700", "fixture")
	chmod.Dir = dir
	if err := chmod.Run(); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(path, filepath.Join(dir, name)); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestDepsInstallExecer_MinimalEnvironment(t *testing.T) {
	t.Setenv("INSTALL_PARENT_SENTINEL", "must-stay-in-parent")
	dir := t.TempDir()
	fakeDepsBinary(t, "go", "/usr/bin/env > child-env\n")
	env := depsInstallEnv(QueuedWork{AuthToken: "session-secret"})
	env["DONMAI_CONTROL_TOKEN"] = "runner-only"
	code, err := (depsInstallExecer{}).Exec(context.Background(), dir, goDepsInstallCommand, env)
	if err != nil || code != 0 {
		t.Fatalf("Exec code=%d err=%v", code, err)
	}
	raw, err := os.ReadFile(filepath.Join(dir, "child-env"))
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, entry := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		key, value, ok := strings.Cut(entry, "=")
		if ok {
			got[key] = value
		}
	}
	// The shell fixture adds its own bookkeeping entries.
	for _, key := range []string{"PWD", "SHLVL", "_"} {
		delete(got, key)
	}
	delete(env, "DONMAI_CONTROL_TOKEN")
	if !reflect.DeepEqual(got, env) {
		t.Fatal("child environment differs from the minimal map")
	}
}

func TestRun_DependencyInstallFailureLogsBoundedOutput(t *testing.T) {
	fakeDepsBinary(t, "pnpm", "printf 'install-stdout\n'; printf 'install-stderr\n' >&2; i=0; while [ $i -lt 20000 ]; do printf x; i=$((i+1)); done; exit 1\n")
	fakeDepsBinary(t, "go", "exit 0\n")
	h := newRunnerHarness(t)
	h.runner.logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	var logs bytes.Buffer
	h.runner.logger = slog.New(slog.NewTextHandler(&logs, nil))
	qw := h.queuedWork("dependency-output")
	seedManifestsInBareRepo(t, h.bareRepo)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	res, err := h.runner.Run(ctx, qw)
	if err != nil || res.Status != "completed" {
		t.Fatalf("Run status=%s err=%v", res.Status, err)
	}
	output := logs.String()
	for _, want := range []string{"install-stdout", "install-stderr", "repository dependency install failed"} {
		if !strings.Contains(output, want) {
			t.Errorf("failure log missing %q", want)
		}
	}
	if strings.Contains(output, strings.Repeat("x", 16*1024)) {
		t.Fatal("failure output was not bounded")
	}
}

func TestDepsInstallExecer_TimeoutKillsGroupAndBoundsPipeWait(t *testing.T) {
	dir := t.TempDir()
	fakeDepsBinary(t, "go", "echo $$ > leader; sleep 60 & echo $! > child; wait\n")
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, err := (depsInstallExecer{}).Exec(ctx, dir, goDepsInstallCommand, depsInstallEnv(QueuedWork{}))
		done <- err
	}()
	leader := waitDepsPID(t, filepath.Join(dir, "leader"))
	child := waitDepsPID(t, filepath.Join(dir, "child"))
	t.Cleanup(func() { _ = syscall.Kill(-leader, syscall.SIGKILL); _ = syscall.Kill(child, syscall.SIGKILL) })
	started := time.Now()
	select {
	case err := <-done:
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Errorf("Exec error=%v; want deadline exceeded", err)
		}
	case <-time.After(2 * depsInstallKillGrace):
		t.Fatal("Exec held by a descendant output pipe past cancellation bound")
	}
	if time.Since(started) > 2*depsInstallKillGrace {
		t.Fatal("Exec exceeded cancellation bound")
	}
	// ps includes zombies on Linux until init reaps them; those have already
	// exited and cannot hold a pipe or execute work.
	deadline := time.Now().Add(time.Second)
	for {
		out, err := exec.Command("ps", "-eo", "pgid=,stat=").Output()
		if err != nil {
			t.Fatalf("inspect install process group: %v", err)
		}
		alive := false
		for _, line := range strings.Split(string(out), "\n") {
			fields := strings.Fields(line)
			if len(fields) == 2 && fields[0] == strconv.Itoa(leader) && !strings.HasPrefix(fields[1], "Z") {
				alive = true
			}
		}
		if !alive {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("install process group still has a live member after cancellation")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func waitDepsPID(t *testing.T, path string) int {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if raw, err := os.ReadFile(path); err == nil {
			if pid, err := strconv.Atoi(strings.TrimSpace(string(raw))); err == nil && pid > 0 {
				return pid
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("install child did not start")
	return 0
}

func TestDepsInstallExecer_WaitDelayBoundsEscapedPipe(t *testing.T) {
	dir := t.TempDir()
	source := `package main
import ("os"; "os/exec"; "strconv"; "syscall"; "time")
func main() {
 child := exec.Command("/bin/sleep", "60")
 child.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
 child.Stdout, child.Stderr = os.Stdout, os.Stderr
 if err := child.Start(); err != nil { panic(err) }
 if err := os.WriteFile("escaped", []byte(strconv.Itoa(child.Process.Pid)), 0600); err != nil { panic(err) }
 time.Sleep(time.Minute)
}
`
	path := filepath.Join(dir, "fixture.go")
	if err := os.WriteFile(path, []byte(source), 0o600); err != nil {
		t.Fatal(err)
	}
	build := exec.Command("go", "build", "-o", "go", "fixture.go")
	build.Dir = dir
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build pipe fixture: %v: %s", err, out)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, err := (depsInstallExecer{}).Exec(ctx, dir, goDepsInstallCommand, depsInstallEnv(QueuedWork{}))
		done <- err
	}()
	child := waitDepsPID(t, filepath.Join(dir, "escaped"))
	t.Cleanup(func() { _ = syscall.Kill(child, syscall.SIGKILL) })
	select {
	case err := <-done:
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Errorf("Exec error=%v; want deadline exceeded", err)
		}
	case <-time.After(2 * depsInstallKillGrace):
		t.Fatal("Exec held by an escaped descendant pipe past deadline bound")
	}
}

func TestRun_DependencyInstallMinimalEnvironment(t *testing.T) {
	t.Setenv("INSTALL_PARENT_SENTINEL", "must-stay-in-parent")
	fakeDepsBinary(t, "go", "if [ -n \"$INSTALL_PARENT_SENTINEL\" ] || [ -n \"$WORKER_AUTH_TOKEN\" ]; then exit 1; fi; printf clean > install-env-result\n")
	fakeDepsBinary(t, "pnpm", `if [ "$*" != "install --prefer-offline --frozen-lockfile --ignore-scripts --ignore-pnpmfile" ]; then exit 1; fi
printf safe > install-args-result
`)
	h := newRunnerHarness(t)
	h.runner.logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	qw := h.queuedWork("dependency-environment")
	seedManifestsInBareRepo(t, h.bareRepo)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	res, err := h.runner.Run(ctx, qw)
	if err != nil || res.Status != "completed" {
		t.Fatalf("Run status=%s err=%v", res.Status, err)
	}
	raw, err := os.ReadFile(filepath.Join(res.WorktreePath, "install-env-result"))
	if err != nil || string(raw) != "clean" {
		t.Fatal("production install inherited worker environment")
	}
	raw, err = os.ReadFile(filepath.Join(res.WorktreePath, "install-args-result"))
	if err != nil || string(raw) != "safe" {
		t.Fatal("production pnpm command omitted safety flags")
	}
}

func TestRun_DependencyInstallTimeoutStillSpawns(t *testing.T) {
	fakeDepsBinary(t, "pnpm", "sleep 60 & wait\n")
	fakeDepsBinary(t, "go", "exit 0\n")
	h := newRunnerHarness(t)
	var logs bytes.Buffer
	h.runner.logger = slog.New(slog.NewTextHandler(&logs, nil))
	h.runner.depsInstallTimeout = 100 * time.Millisecond
	qw := h.queuedWork("dependency-timeout")
	seedManifestsInBareRepo(t, h.bareRepo)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	res, err := h.runner.Run(ctx, qw)
	if err != nil || res.Status != "completed" {
		t.Fatalf("Run status=%s err=%v", res.Status, err)
	}
	if !strings.Contains(logs.String(), "context deadline exceeded") {
		t.Fatal("expired install was not logged")
	}
}

// pnpmFailGoBlockExecer fails the pnpm install fast and blocks the go
// install until the shared context expires. It drives the combined
// failed-plus-timed-out state so the timed_out-over-failed priority is
// pinned: swapping the switch cases reports failed instead and fails the
// test below.
type pnpmFailGoBlockExecer struct {
	mu    sync.Mutex
	calls []depsCall
}

func (f *pnpmFailGoBlockExecer) Exec(ctx context.Context, dir, command string, _ map[string]string) (int, error) {
	f.mu.Lock()
	f.calls = append(f.calls, depsCall{dir: dir, command: command})
	f.mu.Unlock()
	if strings.HasPrefix(command, "pnpm install ") {
		return 1, nil
	}
	<-ctx.Done()
	return -1, ctx.Err()
}

func (f *pnpmFailGoBlockExecer) recorded() []depsCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]depsCall(nil), f.calls...)
}

// TestInstallSessionDependencies_FailedPnpmAndTimedOutGoReportsTimedOut
// pins the timed_out-over-failed priority: a failed pnpm plus a go
// install that expires with the shared context reports timed_out, not
// failed. Swapping the switch cases in installSessionDependencies
// reports failed and fails this test.
func TestInstallSessionDependencies_FailedPnpmAndTimedOutGoReportsTimedOut(t *testing.T) {
	t.Parallel()
	execer := &pnpmFailGoBlockExecer{}
	r := depsTestRunner(t, execer)
	r.depsInstallTimeout = 200 * time.Millisecond

	wpath := t.TempDir()
	writeFile(t, wpath, "pnpm-lock.yaml", "lockfileVersion: 9.0\n")
	writeFile(t, wpath, "go.mod", "module example.test/seed\n\ngo 1.24\n")

	prep := r.installSessionDependencies(context.Background(), QueuedWork{QueuedWork: queuedWorkBase("DEPS-BOTH")}, wpath, false)

	if calls := execer.recorded(); len(calls) != 2 {
		t.Fatalf("install calls = %d; want 2 (failed pnpm then bounded go)", len(calls))
	}
	if prep == nil || prep.Status != agent.DependencyPreparationTimedOut {
		t.Fatalf("preparation = %+v; want timed_out when failed and timed_out combine", prep)
	}
	if len(prep.Ecosystems) != 2 || prep.Ecosystems[0] != "pnpm" || prep.Ecosystems[1] != "go" {
		t.Errorf("ecosystems = %v; want [pnpm go]", prep.Ecosystems)
	}
}

func TestDepsInstallExecer_RejectsShellCommands(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	code, err := (depsInstallExecer{}).Exec(context.Background(), dir, "go mod download; touch unexpected", nil)
	if err == nil || code != -1 {
		t.Fatal("unsupported command was accepted")
	}
	if _, err := os.Stat(filepath.Join(dir, "unexpected")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("unsupported shell command ran")
	}
}
