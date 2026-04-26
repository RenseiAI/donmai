package afcli

import (
	"bytes"
	"strings"
	"testing"
)

// newGovernorStartTestCmd returns a fresh governor start command wired to
// the provided output buffer so we can inspect --help output.
func newGovernorStartTestCmd(args []string) (*bytes.Buffer, error) {
	cmd := newGovernorStartCmd()
	var buf bytes.Buffer
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs(args)
	err := cmd.Execute()
	return &buf, err
}

func TestGovernorStartCommand(t *testing.T) {
	t.Run("help_lists_all_flags", func(t *testing.T) {
		buf, err := newGovernorStartTestCmd([]string{"--help"})
		if err != nil {
			t.Fatalf("--help returned error: %v", err)
		}
		out := buf.String()
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
				t.Errorf("help output missing default marker %q; got:\n%s", d, out)
			}
		}
	})

	t.Run("missing_linear_api_key_fails", func(t *testing.T) {
		t.Setenv("LINEAR_API_KEY", "")
		t.Setenv("GOVERNOR_PROJECTS", "proj1")

		cmd := newGovernorStartCmd()
		var buf bytes.Buffer
		cmd.SetOut(&buf)
		cmd.SetErr(&buf)
		cmd.SetArgs([]string{})
		err := cmd.Execute()
		if err == nil {
			t.Fatal("expected error when LINEAR_API_KEY is unset, got nil")
		}
		if !strings.Contains(err.Error(), "LINEAR_API_KEY") {
			t.Errorf("error should mention LINEAR_API_KEY; got: %v", err)
		}
	})

	t.Run("no_project_fails", func(t *testing.T) {
		t.Setenv("LINEAR_API_KEY", "test-key")
		t.Setenv("GOVERNOR_PROJECTS", "")

		cmd := newGovernorStartCmd()
		var buf bytes.Buffer
		cmd.SetOut(&buf)
		cmd.SetErr(&buf)
		cmd.SetArgs([]string{})
		err := cmd.Execute()
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

		var capturedBin string
		var capturedArgs []string

		origFinder := governorBinaryFinder
		origBackground := governorBackgroundRunner
		t.Cleanup(func() {
			governorBinaryFinder = origFinder
			governorBackgroundRunner = origBackground
		})

		governorBinaryFinder = func() (string, error) { return "/fake/governor", nil }
		governorBackgroundRunner = func(binPath string, args []string) error {
			capturedBin = binPath
			capturedArgs = args
			return nil
		}

		cmd := newGovernorStartCmd()
		var buf bytes.Buffer
		cmd.SetOut(&buf)
		cmd.SetErr(&buf)
		cmd.SetArgs([]string{})
		if err := cmd.Execute(); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}

		if capturedBin != "/fake/governor" {
			t.Errorf("expected bin /fake/governor; got %q", capturedBin)
		}

		argsStr := strings.Join(capturedArgs, " ")
		for _, proj := range []string{"alpha", "beta", "gamma"} {
			needle := "--project " + proj
			if !strings.Contains(argsStr, needle) {
				t.Errorf("child args missing %q; args: %v", needle, capturedArgs)
			}
		}
	})

	t.Run("repeatable_project_flag", func(t *testing.T) {
		t.Setenv("LINEAR_API_KEY", "x")

		var capturedArgs []string
		origFinder := governorBinaryFinder
		origBackground := governorBackgroundRunner
		t.Cleanup(func() {
			governorBinaryFinder = origFinder
			governorBackgroundRunner = origBackground
		})

		governorBinaryFinder = func() (string, error) { return "/fake/governor", nil }
		governorBackgroundRunner = func(_ string, args []string) error {
			capturedArgs = args
			return nil
		}

		cmd := newGovernorStartCmd()
		var buf bytes.Buffer
		cmd.SetOut(&buf)
		cmd.SetErr(&buf)
		cmd.SetArgs([]string{"--project", "p1", "--project", "p2"})
		if err := cmd.Execute(); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}

		argsStr := strings.Join(capturedArgs, " ")
		for _, proj := range []string{"p1", "p2"} {
			needle := "--project " + proj
			if !strings.Contains(argsStr, needle) {
				t.Errorf("child args missing %q; args: %v", needle, capturedArgs)
			}
		}
	})

	t.Run("defaults_forwarded", func(t *testing.T) {
		t.Setenv("LINEAR_API_KEY", "x")

		var capturedArgs []string
		origFinder := governorBinaryFinder
		origBackground := governorBackgroundRunner
		t.Cleanup(func() {
			governorBinaryFinder = origFinder
			governorBackgroundRunner = origBackground
		})

		governorBinaryFinder = func() (string, error) { return "/fake/governor", nil }
		governorBackgroundRunner = func(_ string, args []string) error {
			capturedArgs = args
			return nil
		}

		cmd := newGovernorStartCmd()
		var buf bytes.Buffer
		cmd.SetOut(&buf)
		cmd.SetErr(&buf)
		cmd.SetArgs([]string{"--project", "foo"})
		if err := cmd.Execute(); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}

		wantPairs := [][2]string{
			{"--scan-interval", "60s"},
			{"--max-dispatches", "3"},
			{"--mode", "poll-only"},
		}
		for _, pair := range wantPairs {
			found := false
			for i, a := range capturedArgs {
				if a == pair[0] && i+1 < len(capturedArgs) && capturedArgs[i+1] == pair[1] {
					found = true
					break
				}
			}
			if !found {
				t.Errorf("child args missing %q %q; args: %v", pair[0], pair[1], capturedArgs)
			}
		}
	})

	t.Run("once_flag_forwarded", func(t *testing.T) {
		t.Setenv("LINEAR_API_KEY", "x")

		var capturedArgs []string
		origFinder := governorBinaryFinder
		origBackground := governorBackgroundRunner
		t.Cleanup(func() {
			governorBinaryFinder = origFinder
			governorBackgroundRunner = origBackground
		})

		governorBinaryFinder = func() (string, error) { return "/fake/governor", nil }
		governorBackgroundRunner = func(_ string, args []string) error {
			capturedArgs = args
			return nil
		}

		cmd := newGovernorStartCmd()
		var buf bytes.Buffer
		cmd.SetOut(&buf)
		cmd.SetErr(&buf)
		cmd.SetArgs([]string{"--project", "foo", "--once"})
		if err := cmd.Execute(); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}

		found := false
		for _, a := range capturedArgs {
			if a == "--once" {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("child args missing --once; args: %v", capturedArgs)
		}
	})

	t.Run("feature_toggles_forwarded", func(t *testing.T) {
		t.Setenv("LINEAR_API_KEY", "x")

		toggleCases := []struct {
			flag string
			want string
		}{
			{"--no-auto-research", "--no-auto-research"},
			{"--no-auto-backlog-creation", "--no-auto-backlog-creation"},
			{"--no-auto-development", "--no-auto-development"},
			{"--no-auto-qa", "--no-auto-qa"},
			{"--no-auto-acceptance", "--no-auto-acceptance"},
		}

		for _, tc := range toggleCases {
			tc := tc
			t.Run(tc.flag, func(t *testing.T) {
				var capturedArgs []string
				origFinder := governorBinaryFinder
				origBackground := governorBackgroundRunner
				t.Cleanup(func() {
					governorBinaryFinder = origFinder
					governorBackgroundRunner = origBackground
				})

				governorBinaryFinder = func() (string, error) { return "/fake/governor", nil }
				governorBackgroundRunner = func(_ string, args []string) error {
					capturedArgs = args
					return nil
				}

				cmd := newGovernorStartCmd()
				var buf bytes.Buffer
				cmd.SetOut(&buf)
				cmd.SetErr(&buf)
				cmd.SetArgs([]string{"--project", "foo", tc.flag})
				if err := cmd.Execute(); err != nil {
					t.Fatalf("unexpected error: %v", err)
				}

				found := false
				for _, a := range capturedArgs {
					if a == tc.want {
						found = true
						break
					}
				}
				if !found {
					t.Errorf("child args missing %q; args: %v", tc.want, capturedArgs)
				}
			})
		}

		// Also test multiple toggles at once.
		t.Run("multiple_toggles", func(t *testing.T) {
			var capturedArgs []string
			origFinder := governorBinaryFinder
			origBackground := governorBackgroundRunner
			t.Cleanup(func() {
				governorBinaryFinder = origFinder
				governorBackgroundRunner = origBackground
			})

			governorBinaryFinder = func() (string, error) { return "/fake/governor", nil }
			governorBackgroundRunner = func(_ string, args []string) error {
				capturedArgs = args
				return nil
			}

			cmd := newGovernorStartCmd()
			var buf bytes.Buffer
			cmd.SetOut(&buf)
			cmd.SetErr(&buf)
			cmd.SetArgs([]string{
				"--project", "foo",
				"--no-auto-research",
				"--no-auto-development",
				"--no-auto-qa",
			})
			if err := cmd.Execute(); err != nil {
				t.Fatalf("unexpected error: %v", err)
			}

			for _, want := range []string{"--no-auto-research", "--no-auto-development", "--no-auto-qa"} {
				found := false
				for _, a := range capturedArgs {
					if a == want {
						found = true
						break
					}
				}
				if !found {
					t.Errorf("child args missing %q; args: %v", want, capturedArgs)
				}
			}
		})
	})

	t.Run("invalid_mode_fails", func(t *testing.T) {
		t.Setenv("LINEAR_API_KEY", "x")
		t.Setenv("GOVERNOR_PROJECTS", "proj1")

		cmd := newGovernorStartCmd()
		var buf bytes.Buffer
		cmd.SetOut(&buf)
		cmd.SetErr(&buf)
		cmd.SetArgs([]string{"--project", "foo", "--mode", "banana"})
		err := cmd.Execute()
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

	t.Run("valid_modes", func(t *testing.T) {
		t.Setenv("LINEAR_API_KEY", "x")

		for _, validMode := range []string{"event-driven", "poll-only"} {
			validMode := validMode
			t.Run(validMode, func(t *testing.T) {
				origFinder := governorBinaryFinder
				origBackground := governorBackgroundRunner
				t.Cleanup(func() {
					governorBinaryFinder = origFinder
					governorBackgroundRunner = origBackground
				})

				governorBinaryFinder = func() (string, error) { return "/fake/governor", nil }
				governorBackgroundRunner = func(_ string, _ []string) error { return nil }

				cmd := newGovernorStartCmd()
				var buf bytes.Buffer
				cmd.SetOut(&buf)
				cmd.SetErr(&buf)
				cmd.SetArgs([]string{"--project", "foo", "--mode", validMode})
				if err := cmd.Execute(); err != nil {
					t.Errorf("mode %q should be valid, got error: %v", validMode, err)
				}
			})
		}
	})

	t.Run("foreground_flag_routes_to_foreground_runner", func(t *testing.T) {
		t.Setenv("LINEAR_API_KEY", "x")

		foregroundCalled := false
		backgroundCalled := false

		origFinder := governorBinaryFinder
		origForeground := governorForegroundRunner
		origBackground := governorBackgroundRunner
		t.Cleanup(func() {
			governorBinaryFinder = origFinder
			governorForegroundRunner = origForeground
			governorBackgroundRunner = origBackground
		})

		governorBinaryFinder = func() (string, error) { return "/fake/governor", nil }
		governorForegroundRunner = func(_ string, _ []string) error {
			foregroundCalled = true
			return nil
		}
		governorBackgroundRunner = func(_ string, _ []string) error {
			backgroundCalled = true
			return nil
		}

		cmd := newGovernorStartCmd()
		var buf bytes.Buffer
		cmd.SetOut(&buf)
		cmd.SetErr(&buf)
		cmd.SetArgs([]string{"--project", "foo", "--foreground"})
		if err := cmd.Execute(); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}

		if !foregroundCalled {
			t.Error("expected governorForegroundRunner to be called")
		}
		if backgroundCalled {
			t.Error("governorBackgroundRunner should NOT have been called")
		}
	})
}
