package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/RenseiAI/agentfactory-tui/afclient"
)

// executeRoot builds a fresh root command with its RunE swapped for a
// no-op (so Bubble Tea never launches), runs Cobra's full lifecycle
// with the given args, and returns the resolved flags plus any error.
func executeRoot(t *testing.T, args []string) (*rootFlags, error) {
	t.Helper()
	cmd, flags := newRootCmd()
	cmd.RunE = func(_ *cobra.Command, _ []string) error { return nil }
	cmd.SetArgs(args)
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&bytes.Buffer{})
	return flags, cmd.Execute()
}

// unsetEnv unsets the given env var and restores its prior value via
// t.Cleanup. Use this instead of t.Setenv("", "") because t.Setenv
// leaves the var *set* (to empty string) and godotenv.Load will not
// override an already-set variable.
func unsetEnv(t *testing.T, key string) {
	t.Helper()
	prev, had := os.LookupEnv(key)
	if err := os.Unsetenv(key); err != nil {
		t.Fatalf("unsetenv %s: %v", key, err)
	}
	t.Cleanup(func() {
		if had {
			_ = os.Setenv(key, prev)
		} else {
			_ = os.Unsetenv(key)
		}
	})
}

// chdirIsolated moves into an empty temp directory so stray .env files
// in the test's working directory cannot leak into the test.
func chdirIsolated(t *testing.T) {
	t.Helper()
	t.Chdir(t.TempDir())
}

func TestRootFlagResolution(t *testing.T) {
	t.Run("defaults when no env and no flags", func(t *testing.T) {
		unsetEnv(t, "WORKER_API_URL")
		chdirIsolated(t)
		flags, err := executeRoot(t, []string{})
		if err != nil {
			t.Fatalf("execute: %v", err)
		}
		if flags.mock {
			t.Errorf("mock = true, want false")
		}
		if flags.url != defaultBaseURL {
			t.Errorf("url = %q, want %q", flags.url, defaultBaseURL)
		}
	})

	t.Run("env var overrides default", func(t *testing.T) {
		chdirIsolated(t)
		t.Setenv("WORKER_API_URL", "http://from-env:9999")
		flags, err := executeRoot(t, []string{})
		if err != nil {
			t.Fatalf("execute: %v", err)
		}
		if flags.url != "http://from-env:9999" {
			t.Errorf("url = %q, want %q", flags.url, "http://from-env:9999")
		}
	})

	t.Run("explicit --url wins over env var", func(t *testing.T) {
		chdirIsolated(t)
		t.Setenv("WORKER_API_URL", "http://from-env:9999")
		flags, err := executeRoot(t, []string{"--url", "http://explicit:1234"})
		if err != nil {
			t.Fatalf("execute: %v", err)
		}
		if flags.url != "http://explicit:1234" {
			t.Errorf("url = %q, want %q", flags.url, "http://explicit:1234")
		}
	})

	t.Run("--mock flag is parsed", func(t *testing.T) {
		unsetEnv(t, "WORKER_API_URL")
		chdirIsolated(t)
		flags, err := executeRoot(t, []string{"--mock"})
		if err != nil {
			t.Fatalf("execute: %v", err)
		}
		if !flags.mock {
			t.Errorf("mock = false, want true")
		}
	})
}

func TestDotenvLoadedByPreRunE(t *testing.T) {
	// Unset so the flag default resolves to defaultBaseURL; godotenv
	// will then populate WORKER_API_URL from .env and PersistentPreRunE
	// should pick it up. Note: t.Setenv("", "") does NOT work here
	// because it leaves the var *set* to empty, and godotenv refuses to
	// override already-set vars.
	unsetEnv(t, "WORKER_API_URL")

	dir := t.TempDir()
	envPath := filepath.Join(dir, ".env")
	if err := os.WriteFile(envPath, []byte("WORKER_API_URL=http://from-dotenv:5555\n"), 0o600); err != nil {
		t.Fatalf("write .env: %v", err)
	}
	t.Chdir(dir)

	flags, err := executeRoot(t, []string{})
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if flags.url != "http://from-dotenv:5555" {
		t.Errorf("url = %q, want dotenv value", flags.url)
	}
}

func TestDotenvLoadMissingFileIsNonFatal(t *testing.T) {
	unsetEnv(t, "WORKER_API_URL")
	// An empty temp dir has no .env / .env.local — load should be a no-op.
	chdirIsolated(t)

	flags, err := executeRoot(t, []string{})
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if flags.url != defaultBaseURL {
		t.Errorf("url = %q, want %q", flags.url, defaultBaseURL)
	}
}

func TestBuildDataSource(t *testing.T) {
	tests := []struct {
		name     string
		mock     bool
		url      string
		apiKey   string
		wantType string // "mock", "real", or "auth"
	}{
		{"mock true yields MockClient", true, "http://ignored", "", "mock"},
		{"mock false yields real Client", false, "http://live:8080", "", "real"},
		{"api key yields authenticated Client", false, "http://live:8080", "rsk_test", "auth"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ds := buildDataSource(&rootFlags{mock: tt.mock, url: tt.url, apiKey: tt.apiKey})
			if ds == nil {
				t.Fatal("buildDataSource returned nil")
			}
			switch tt.wantType {
			case "mock":
				if _, ok := ds.(*afclient.MockClient); !ok {
					t.Errorf("DataSource type = %T, want *afclient.MockClient", ds)
				}
			case "real":
				c, ok := ds.(*afclient.Client)
				if !ok {
					t.Errorf("DataSource type = %T, want *afclient.Client", ds)
				} else if c.APIToken != "" {
					t.Errorf("APIToken = %q, want empty", c.APIToken)
				}
			case "auth":
				c, ok := ds.(*afclient.Client)
				if !ok {
					t.Errorf("DataSource type = %T, want *afclient.Client", ds)
				} else if c.APIToken != tt.apiKey {
					t.Errorf("APIToken = %q, want %q", c.APIToken, tt.apiKey)
				}
			}
		})
	}
}

func TestHelpOutputContainsFlags(t *testing.T) {
	cmd, _ := newRootCmd()
	var buf bytes.Buffer
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{"--help"})

	if err := cmd.Execute(); err != nil {
		t.Fatalf("--help should exit 0, got error: %v", err)
	}

	out := buf.String()
	for _, want := range []string{"--mock", "--url", "--debug", "--quiet"} {
		if !strings.Contains(out, want) {
			t.Errorf("help output missing %q; got:\n%s", want, out)
		}
	}
}

func TestStandaloneAfRegistersLegacyWorkerFleetCommands(t *testing.T) {
	cmd, _ := newRootCmd()

	for _, want := range []string{"daemon", "fleet", "worker"} {
		found := false
		for _, child := range cmd.Commands() {
			if child.Name() == want {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("standalone af missing %q command", want)
		}
	}
}

// TestVersionFlag pins the cobra-auto-wired `--version` flag the user
// noticed was missing (`af --version` errored "unknown flag" while
// `rensei --version` worked). Setting `cobra.Command.Version` on the
// root makes cobra register both --version and -v automatically; the
// rendered string carries the same `version` package var that the
// daemon now also reports via Options.Version, so operator output
// across all surfaces stays consistent.
func TestVersionFlag(t *testing.T) {
	cmd, _ := newRootCmd()
	var buf bytes.Buffer
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{"--version"})

	if err := cmd.Execute(); err != nil {
		t.Fatalf("--version should exit 0, got error: %v", err)
	}

	out := buf.String()
	if !strings.Contains(out, version) {
		t.Errorf("--version output missing version %q; got:\n%s", version, out)
	}
}
