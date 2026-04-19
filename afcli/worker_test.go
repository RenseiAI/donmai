package afcli

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/RenseiAI/agentfactory-tui/afclient"
)

// TestWorkerParentHelp verifies the worker parent command exposes the
// start subcommand via --help.
func TestWorkerParentHelp(t *testing.T) {
	t.Parallel()

	cmd := newWorkerCmd(func() afclient.DataSource { return afclient.NewMockClient() })
	buf := &bytes.Buffer{}
	cmd.SetOut(buf)
	cmd.SetErr(buf)
	cmd.SetArgs([]string{"--help"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("execute: %v", err)
	}
	if !strings.Contains(buf.String(), "start") {
		t.Errorf("worker --help missing 'start' subcommand; got:\n%s", buf.String())
	}
}

// TestWorkerStartDefaults verifies the flag defaults on `worker start`
// match the documented contract.
func TestWorkerStartDefaults(t *testing.T) {
	t.Parallel()

	cmd := newWorkerStartCmd()

	cases := []struct {
		name string
		want string
	}{
		{"max-agents", "1"},
		{"poll-interval", "5s"},
		{"heartbeat-interval", "30s"},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := cmd.Flags().Lookup(tc.name)
			if f == nil {
				t.Fatalf("flag %q not registered", tc.name)
			}
			if f.DefValue != tc.want {
				t.Errorf("flag %q default = %q, want %q", tc.name, f.DefValue, tc.want)
			}
		})
	}
}

// TestResolveWorkerToken covers flag > env > error precedence.
func TestResolveWorkerToken(t *testing.T) {
	tests := []struct {
		name    string
		flag    string
		env     string
		want    string
		wantErr bool
	}{
		{"flag_wins", "flag-token", "env-token", "flag-token", false},
		{"env_fallback", "", "env-token", "env-token", false},
		{"both_empty_errors", "", "", "", true},
	}
	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("AF_PROVISIONING_TOKEN", tc.env)
			got, err := resolveWorkerToken(tc.flag)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected error, got nil")
				}
				if !strings.Contains(err.Error(), "provisioning token required") {
					t.Errorf("error missing expected phrase: %v", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}
}

// TestResolveWorkerBaseURL covers flag > env > default precedence.
func TestResolveWorkerBaseURL(t *testing.T) {
	tests := []struct {
		name string
		flag string
		env  string
		want string
	}{
		{"flag_wins", "https://flag.example", "https://env.example", "https://flag.example"},
		{"env_fallback", "", "https://env.example", "https://env.example"},
		{"default_fallback", "", "", defaultWorkerBaseURL},
	}
	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("AF_BASE_URL", tc.env)
			if got := resolveWorkerBaseURL(tc.flag); got != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}
}

// TestWorkerStartMissingToken verifies that invoking `worker start` with
// no token and no env var returns a helpful error without spawning any
// process.
func TestWorkerStartMissingToken(t *testing.T) {
	t.Setenv("AF_PROVISIONING_TOKEN", "")

	err := runWorkerStart(&workerStartFlags{
		pollInterval:      5 * time.Second,
		heartbeatInterval: 30 * time.Second,
	})
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "provisioning token required") {
		t.Errorf("error missing expected phrase: %v", err)
	}
}

// TestConfigureWorkerLogging is a smoke test — it must not panic for
// any flag combination.
func TestConfigureWorkerLogging(t *testing.T) {
	cases := []struct {
		name         string
		debug, quiet bool
	}{
		{"default", false, false},
		{"debug", true, false},
		{"quiet", false, true},
		{"debug_and_quiet", true, true},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(_ *testing.T) {
			configureWorkerLogging(tc.debug, tc.quiet)
		})
	}
}
