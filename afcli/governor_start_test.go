package afcli

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/RenseiAI/agentfactory-tui/internal/governor"
)

// fakeRunnable is a no-op governorRunnable used in tests.
type fakeRunnable struct {
	runErr error
}

func (f *fakeRunnable) Run(_ context.Context) error { return f.runErr }

// fakeFactory returns a factory function that captures the governor.Config
// passed to it and uses the provided fakeRunnable.
func fakeFactory(runnable governorRunnable, capturedCfg *governor.Config) func(
	cfg governor.Config, apiKey, redisURL string, logger *slog.Logger,
) (governorRunnable, func() error, error) {
	return func(cfg governor.Config, apiKey, redisURL string, logger *slog.Logger) (governorRunnable, func() error, error) {
		if capturedCfg != nil {
			*capturedCfg = cfg
		}
		return runnable, func() error { return nil }, nil
	}
}

// fakeFactoryErr returns a factory that always returns the given error.
func fakeFactoryErr(err error) func(
	cfg governor.Config, apiKey, redisURL string, logger *slog.Logger,
) (governorRunnable, func() error, error) {
	return func(_ governor.Config, _, _ string, _ *slog.Logger) (governorRunnable, func() error, error) {
		return nil, nil, err
	}
}

// runCmd is a helper that executes a fresh governor start command with the
// given args and returns (output, error).
func runCmd(args []string) (string, error) {
	cmd := newGovernorStartCmd()
	var buf bytes.Buffer
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs(args)
	err := cmd.Execute()
	return buf.String(), err
}

// installFakeFactory replaces governorRunnerFactory with fn and restores the
// original on test cleanup.
func installFakeFactory(t *testing.T, fn func(governor.Config, string, string, *slog.Logger) (governorRunnable, func() error, error)) {
	t.Helper()
	orig := governorRunnerFactory
	governorRunnerFactory = fn
	t.Cleanup(func() { governorRunnerFactory = orig })
}

// ── Flag / help tests ─────────────────────────────────────────────────────────

func TestGovernorStartFlags(t *testing.T) {
	t.Run("help_lists_all_flags", func(t *testing.T) {
		out, err := runCmd([]string{"--help"})
		if err != nil {
			t.Fatalf("--help returned error: %v", err)
		}
		wantFlags := []string{
			"--project",
			"--scan-interval",
			"--max-dispatches",
			"--once",
			"--mode",
			"--foreground",
			"--no-auto-research",
			"--no-auto-backlog-creation",
			"--no-auto-development",
			"--no-auto-qa",
			"--no-auto-acceptance",
		}
		for _, flag := range wantFlags {
			if !strings.Contains(out, flag) {
				t.Errorf("help output missing flag %q; got:\n%s", flag, out)
			}
		}
		wantDefaults := []string{"60s", "3", "poll-only"}
		for _, d := range wantDefaults {
			if !strings.Contains(out, d) {
				t.Errorf("help output missing default %q; got:\n%s", d, out)
			}
		}
	})

	t.Run("flag_defaults_via_lookup", func(t *testing.T) {
		cmd := newGovernorStartCmd()
		cases := []struct {
			flag string
			want string
		}{
			{"scan-interval", "60s"},
			{"max-dispatches", "3"},
			{"mode", "poll-only"},
			{"once", "false"},
			{"foreground", "false"},
			{"no-auto-research", "false"},
			{"no-auto-backlog-creation", "false"},
			{"no-auto-development", "false"},
			{"no-auto-qa", "false"},
			{"no-auto-acceptance", "false"},
		}
		for _, tc := range cases {
			f := cmd.Flag(tc.flag)
			if f == nil {
				t.Errorf("flag --%s not found", tc.flag)
				continue
			}
			if got := f.DefValue; got != tc.want {
				t.Errorf("--%s default = %q, want %q", tc.flag, got, tc.want)
			}
		}
	})
}

// ── Preflight / env tests ─────────────────────────────────────────────────────

func TestGovernorStartPreflights(t *testing.T) {
	t.Run("missing_linear_api_key", func(t *testing.T) {
		t.Setenv("LINEAR_API_KEY", "")
		t.Setenv("GOVERNOR_PROJECTS", "proj1")

		_, err := runCmd([]string{})
		if err == nil {
			t.Fatal("expected error when LINEAR_API_KEY is unset, got nil")
		}
		if !strings.Contains(err.Error(), "LINEAR_API_KEY") {
			t.Errorf("error should mention LINEAR_API_KEY; got: %v", err)
		}
	})

	t.Run("missing_project_flag_and_env", func(t *testing.T) {
		t.Setenv("LINEAR_API_KEY", "test-key")
		t.Setenv("GOVERNOR_PROJECTS", "")

		_, err := runCmd([]string{})
		if err == nil {
			t.Fatal("expected error when no project specified, got nil")
		}
		if !strings.Contains(err.Error(), "project") {
			t.Errorf("error should mention 'project'; got: %v", err)
		}
	})

	t.Run("governor_projects_env_fallback", func(t *testing.T) {
		t.Setenv("LINEAR_API_KEY", "x")
		t.Setenv("GOVERNOR_PROJECTS", "alpha,beta,gamma")
		t.Setenv("REDIS_URL", "redis://localhost:6379")

		var captured governor.Config
		installFakeFactory(t, fakeFactory(&fakeRunnable{}, &captured))

		// Use --once + --foreground so the runner exits immediately without
		// blocking or daemonizing.
		if _, err := runCmd([]string{"--once", "--foreground"}); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(captured.Projects) != 3 {
			t.Fatalf("expected 3 projects; got %v", captured.Projects)
		}
		for _, want := range []string{"alpha", "beta", "gamma"} {
			found := false
			for _, p := range captured.Projects {
				if p == want {
					found = true
					break
				}
			}
			if !found {
				t.Errorf("project %q not in captured.Projects %v", want, captured.Projects)
			}
		}
	})

	t.Run("missing_redis_url", func(t *testing.T) {
		t.Setenv("LINEAR_API_KEY", "x")
		t.Setenv("GOVERNOR_PROJECTS", "proj1")
		t.Setenv("REDIS_URL", "")

		// Use the real factory — it should fail with REDIS_URL error.
		orig := governorRunnerFactory
		governorRunnerFactory = defaultGovernorRunnerFactory
		t.Cleanup(func() { governorRunnerFactory = orig })

		_, err := runCmd([]string{"--project", "foo", "--once", "--foreground"})
		if err == nil {
			t.Fatal("expected error when REDIS_URL is unset, got nil")
		}
		if !strings.Contains(err.Error(), "REDIS_URL") {
			t.Errorf("error should mention REDIS_URL; got: %v", err)
		}
	})
}

// ── Mode validation ───────────────────────────────────────────────────────────

func TestGovernorStartMode(t *testing.T) {
	t.Setenv("LINEAR_API_KEY", "x")
	t.Setenv("GOVERNOR_PROJECTS", "proj1")

	t.Run("invalid_mode", func(t *testing.T) {
		_, err := runCmd([]string{"--project", "foo", "--mode", "banana"})
		if err == nil {
			t.Fatal("expected error for invalid --mode, got nil")
		}
		if !strings.Contains(err.Error(), "invalid --mode") {
			t.Errorf("error should contain 'invalid --mode'; got: %v", err)
		}
		if !strings.Contains(err.Error(), "banana") {
			t.Errorf("error should contain the invalid value; got: %v", err)
		}
	})

	t.Run("valid_modes_accepted", func(t *testing.T) {
		t.Setenv("REDIS_URL", "redis://localhost:6379")
		installFakeFactory(t, fakeFactory(&fakeRunnable{}, nil))

		for _, m := range []string{"event-driven", "poll-only"} {
			m := m
			t.Run(m, func(t *testing.T) {
				_, err := runCmd([]string{"--project", "foo", "--mode", m, "--once", "--foreground"})
				if err != nil {
					t.Errorf("mode %q should be valid, got error: %v", m, err)
				}
			})
		}
	})
}

// ── Factory cfg-build correctness ─────────────────────────────────────────────

func TestGovernorStartCfgBuild(t *testing.T) {
	t.Setenv("LINEAR_API_KEY", "x")
	t.Setenv("REDIS_URL", "redis://localhost:6379")

	var captured governor.Config
	installFakeFactory(t, fakeFactory(&fakeRunnable{}, &captured))

	_, err := runCmd([]string{
		"--project", "p1",
		"--project", "p2",
		"--scan-interval", "30s",
		"--max-dispatches", "5",
		"--once",
		"--mode", "event-driven",
		"--no-auto-research",
		"--no-auto-backlog-creation",
		"--foreground",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	wantProjects := []string{"p1", "p2"}
	if len(captured.Projects) != len(wantProjects) {
		t.Fatalf("Projects = %v, want %v", captured.Projects, wantProjects)
	}
	for i, p := range wantProjects {
		if captured.Projects[i] != p {
			t.Errorf("Projects[%d] = %q, want %q", i, captured.Projects[i], p)
		}
	}

	if captured.ScanInterval != 30*time.Second {
		t.Errorf("ScanInterval = %v, want 30s", captured.ScanInterval)
	}
	if captured.MaxDispatches != 5 {
		t.Errorf("MaxDispatches = %d, want 5", captured.MaxDispatches)
	}
	if !captured.Once {
		t.Error("Once = false, want true")
	}
	if captured.Mode != governor.ModeEventDriven {
		t.Errorf("Mode = %q, want event-driven", captured.Mode)
	}
	// --no-auto-research → AutoResearch = false
	if captured.AutoResearch {
		t.Error("AutoResearch = true, want false (--no-auto-research was set)")
	}
	// --no-auto-backlog-creation → AutoBacklogCreation = false
	if captured.AutoBacklogCreation {
		t.Error("AutoBacklogCreation = true, want false (--no-auto-backlog-creation was set)")
	}
	// remaining toggles should default to true
	if !captured.AutoDevelopment {
		t.Error("AutoDevelopment = false, want true")
	}
	if !captured.AutoQA {
		t.Error("AutoQA = false, want true")
	}
	if !captured.AutoAcceptance {
		t.Error("AutoAcceptance = false, want true")
	}
}

// ── once + foreground succeeds ────────────────────────────────────────────────

func TestGovernorStartOnceForeground(t *testing.T) {
	t.Setenv("LINEAR_API_KEY", "x")
	t.Setenv("REDIS_URL", "redis://localhost:6379")

	installFakeFactory(t, fakeFactory(&fakeRunnable{runErr: nil}, nil))

	_, err := runCmd([]string{"--project", "foo", "--once", "--foreground"})
	if err != nil {
		t.Errorf("expected nil, got %v", err)
	}
}

// ── runner Run() error is propagated ─────────────────────────────────────────

func TestGovernorStartRunError(t *testing.T) {
	t.Setenv("LINEAR_API_KEY", "x")
	t.Setenv("REDIS_URL", "redis://localhost:6379")

	runErr := fmt.Errorf("scan exploded")
	installFakeFactory(t, fakeFactory(&fakeRunnable{runErr: runErr}, nil))

	_, err := runCmd([]string{"--project", "foo", "--once", "--foreground"})
	if err == nil {
		t.Fatal("expected error from runner, got nil")
	}
	if !strings.Contains(err.Error(), "scan exploded") {
		t.Errorf("error should contain runner error; got: %v", err)
	}
}

// ── factory error is propagated ───────────────────────────────────────────────

func TestGovernorStartFactoryError(t *testing.T) {
	t.Setenv("LINEAR_API_KEY", "x")
	t.Setenv("REDIS_URL", "redis://localhost:6379")

	factoryErr := fmt.Errorf("could not connect")
	installFakeFactory(t, fakeFactoryErr(factoryErr))

	_, err := runCmd([]string{"--project", "foo", "--once", "--foreground"})
	if err == nil {
		t.Fatal("expected error from factory, got nil")
	}
	if !strings.Contains(err.Error(), "could not connect") {
		t.Errorf("error should contain factory error; got: %v", err)
	}
}

// ── invalid scan-interval ─────────────────────────────────────────────────────

func TestGovernorStartInvalidScanInterval(t *testing.T) {
	t.Setenv("LINEAR_API_KEY", "x")
	t.Setenv("GOVERNOR_PROJECTS", "proj1")

	_, err := runCmd([]string{"--project", "foo", "--scan-interval", "notaduration"})
	if err == nil {
		t.Fatal("expected error for invalid --scan-interval, got nil")
	}
	if !strings.Contains(err.Error(), "scan-interval") {
		t.Errorf("error should mention scan-interval; got: %v", err)
	}
}

// ── background / daemon path tests ───────────────────────────────────────────

// installFakeDaemonize replaces governorDaemonize and restores it on cleanup.
func installFakeDaemonize(t *testing.T, isChild bool, childPID int, err error) {
	t.Helper()
	orig := governorDaemonize
	governorDaemonize = func() (bool, int, error) { return isChild, childPID, err }
	t.Cleanup(func() { governorDaemonize = orig })
}

func TestGovernorStartBackgroundPath(t *testing.T) {
	t.Setenv("LINEAR_API_KEY", "x")
	t.Setenv("REDIS_URL", "redis://localhost:6379")

	t.Run("parent_writes_pid_and_returns", func(t *testing.T) {
		installFakeFactory(t, fakeFactory(&fakeRunnable{}, nil))
		// Simulate parent process: isChild=false, childPID=12345
		installFakeDaemonize(t, false, 12345, nil)

		_, err := runCmd([]string{"--project", "foo"})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
	})

	t.Run("child_runs_loop_and_returns", func(t *testing.T) {
		installFakeFactory(t, fakeFactory(&fakeRunnable{runErr: nil}, nil))
		// Simulate child process: isChild=true
		installFakeDaemonize(t, true, 0, nil)

		_, err := runCmd([]string{"--project", "foo"})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
	})

	t.Run("daemonize_error_propagates", func(t *testing.T) {
		installFakeFactory(t, fakeFactory(&fakeRunnable{}, nil))
		daemonErr := fmt.Errorf("fork failed")
		installFakeDaemonize(t, false, 0, daemonErr)

		_, err := runCmd([]string{"--project", "foo"})
		if err == nil {
			t.Fatal("expected error from daemonize, got nil")
		}
		if !strings.Contains(err.Error(), "fork failed") {
			t.Errorf("error should mention 'fork failed'; got: %v", err)
		}
	})
}

// ── defaultGovernorRunnerFactory unit tests ───────────────────────────────────

func TestDefaultGovernorRunnerFactory(t *testing.T) {
	validCfg := governor.Config{
		Projects:      []string{"proj1"},
		ScanInterval:  60 * time.Second,
		MaxDispatches: 3,
		Mode:          governor.ModePollOnly,
	}
	logger := slog.New(slog.NewTextHandler(bytes.NewBuffer(nil), nil))

	t.Run("empty_api_key_returns_error", func(t *testing.T) {
		_, _, err := defaultGovernorRunnerFactory(validCfg, "", "redis://localhost:6379", logger)
		if err == nil {
			t.Fatal("expected error for empty apiKey")
		}
		if !strings.Contains(err.Error(), "governor start") {
			t.Errorf("error should be wrapped with 'governor start'; got: %v", err)
		}
	})

	t.Run("empty_redis_url_returns_error", func(t *testing.T) {
		_, _, err := defaultGovernorRunnerFactory(validCfg, "test-api-key", "", logger)
		if err == nil {
			t.Fatal("expected error for empty redisURL")
		}
		if !strings.Contains(err.Error(), "REDIS_URL") {
			t.Errorf("error should mention REDIS_URL; got: %v", err)
		}
	})

	t.Run("malformed_redis_url_returns_error", func(t *testing.T) {
		_, _, err := defaultGovernorRunnerFactory(validCfg, "test-api-key", "not-a-redis-url", logger)
		if err == nil {
			t.Fatal("expected error for malformed redis URL")
		}
		if !strings.Contains(err.Error(), "governor start") {
			t.Errorf("error should be wrapped with 'governor start'; got: %v", err)
		}
	})

	t.Run("unreachable_redis_ping_fails", func(t *testing.T) {
		// Port 1 should always be refused.
		_, _, err := defaultGovernorRunnerFactory(validCfg, "test-api-key", "redis://127.0.0.1:1", logger)
		if err == nil {
			t.Fatal("expected error when redis is unreachable")
		}
		if !strings.Contains(err.Error(), "governor start") {
			t.Errorf("error should be wrapped with 'governor start'; got: %v", err)
		}
	})
}
