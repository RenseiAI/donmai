package afcli

import (
	"bytes"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/RenseiAI/agentfactory-tui/afclient"
	"github.com/RenseiAI/agentfactory-tui/worker"
)

// TestFleetParentHelp verifies the fleet parent command exposes all four
// subcommands via --help.
func TestFleetParentHelp(t *testing.T) {
	t.Parallel()

	cmd := newFleetCmd(func() afclient.DataSource { return afclient.NewMockClient() })
	buf := &bytes.Buffer{}
	cmd.SetOut(buf)
	cmd.SetErr(buf)
	cmd.SetArgs([]string{"--help"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("execute: %v", err)
	}
	for _, want := range []string{"start", "stop", "status", "scale"} {
		if !strings.Contains(buf.String(), want) {
			t.Errorf("fleet --help missing subcommand %q; got:\n%s", want, buf.String())
		}
	}
}

// TestFleetStartRequiresCount verifies the --count flag is required.
func TestFleetStartRequiresCount(t *testing.T) {
	t.Parallel()

	// cobra's MarkFlagRequired reports missing required flags via Execute.
	cmd := newFleetStartCmd()
	cmd.SetArgs(nil)
	buf := &bytes.Buffer{}
	cmd.SetOut(buf)
	cmd.SetErr(buf)
	if err := cmd.Execute(); err == nil {
		t.Fatal("expected error for missing --count")
	}
}

// TestFleetStartInvalidCount exercises the --count <= 0 error path.
func TestFleetStartInvalidCount(t *testing.T) {
	t.Parallel()

	cmd := newFleetStartCmd()
	cmd.SetArgs([]string{"--count", "0"})
	buf := &bytes.Buffer{}
	cmd.SetOut(buf)
	cmd.SetErr(buf)
	err := cmd.Execute()
	if err == nil {
		t.Fatal("expected error for --count=0")
	}
	if !strings.Contains(err.Error(), "count must be > 0") {
		t.Errorf("error missing expected phrase: %v", err)
	}
}

// TestBuildWorkerChildArgs verifies the argv assembly for children.
func TestBuildWorkerChildArgs(t *testing.T) {
	t.Parallel()

	flags := &fleetStartFlags{
		provisioningToken: "token-xyz", // #nosec G101 -- fixture, not a credential
		baseURL:           "https://coord.example",
		maxAgents:         3,
		pollInterval:      5 * time.Second,
		heartbeatInterval: 15 * time.Second,
		capabilities:      []string{"claude", "codex"},
	}
	args := buildWorkerChildArgs(flags)

	joined := strings.Join(args, " ")
	for _, want := range []string{
		"worker start",
		"--provisioning-token token-xyz",
		"--base-url https://coord.example",
		"--max-agents 3",
		"--poll-interval 5s",
		"--heartbeat-interval 15s",
		"--capabilities claude",
		"--capabilities codex",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("args missing %q; got: %s", want, joined)
		}
	}
}

// TestBuildWorkerChildArgsMinimal verifies that omitted flags do not
// produce empty-value arguments.
func TestBuildWorkerChildArgsMinimal(t *testing.T) {
	t.Parallel()

	args := buildWorkerChildArgs(&fleetStartFlags{})
	if len(args) != 2 || args[0] != "worker" || args[1] != "start" {
		t.Errorf("minimal args should be [worker start]; got: %v", args)
	}
}

// TestFleetStopNoPIDFile verifies a clear error when no PID file exists.
func TestFleetStopNoPIDFile(t *testing.T) {
	// Redirect the fleet PID path to a tempdir so we don't touch the
	// user's real config directory.
	dir := t.TempDir()
	t.Setenv("AGENTFACTORY_FLEET_PID_FILE", filepath.Join(dir, "fleet.pids"))

	cmd := newFleetStopCmd()
	buf := &bytes.Buffer{}
	cmd.SetOut(buf)
	cmd.SetErr(buf)
	cmd.SetArgs(nil)
	err := cmd.Execute()
	if err == nil {
		t.Fatal("expected error for missing PID file")
	}
	if !strings.Contains(err.Error(), "no fleet PID file") {
		t.Errorf("error missing expected phrase: %v", err)
	}
}

// TestFleetStatusNoPIDFile verifies status prints a clear not-running
// message without error when no PID file exists.
func TestFleetStatusNoPIDFile(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("AGENTFACTORY_FLEET_PID_FILE", filepath.Join(dir, "fleet.pids"))

	cmd := newFleetStatusCmd()
	buf := &bytes.Buffer{}
	cmd.SetOut(buf)
	cmd.SetErr(buf)
	cmd.SetArgs(nil)
	if err := cmd.Execute(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(buf.String(), "Fleet is not running") {
		t.Errorf("expected 'Fleet is not running'; got:\n%s", buf.String())
	}
}

// TestFleetStatusWithPIDs verifies the status table renders PIDs and a
// STATE column. The PID we inject is our own (so it's definitely
// running) plus one known-dead PID.
func TestFleetStatusWithPIDs(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("AGENTFACTORY_FLEET_PID_FILE", filepath.Join(dir, "fleet.pids"))

	// Own PID plus a fake one that should be "dead".
	if err := worker.WriteFleetPIDs([]int{1}); err != nil {
		t.Fatalf("write pids: %v", err)
	}

	cmd := newFleetStatusCmd()
	buf := &bytes.Buffer{}
	cmd.SetOut(buf)
	cmd.SetErr(buf)
	cmd.SetArgs(nil)
	if err := cmd.Execute(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	out := buf.String()
	for _, want := range []string{"PID", "STATE"} {
		if !strings.Contains(out, want) {
			t.Errorf("status output missing %q; got:\n%s", want, out)
		}
	}
}

// TestFleetScaleNotSupported verifies scale returns the documented stub
// error rather than silently succeeding.
func TestFleetScaleNotSupported(t *testing.T) {
	t.Parallel()

	cmd := newFleetScaleCmd()
	buf := &bytes.Buffer{}
	cmd.SetOut(buf)
	cmd.SetErr(buf)
	cmd.SetArgs([]string{"--count", "3"})
	err := cmd.Execute()
	if err == nil {
		t.Fatal("expected error from unsupported scale")
	}
	if !strings.Contains(err.Error(), "not yet supported") {
		t.Errorf("error missing expected phrase: %v", err)
	}
}

// TestFleetScaleInvalidCount verifies --count <= 0 errors before the
// stub message.
func TestFleetScaleInvalidCount(t *testing.T) {
	t.Parallel()

	cmd := newFleetScaleCmd()
	buf := &bytes.Buffer{}
	cmd.SetOut(buf)
	cmd.SetErr(buf)
	cmd.SetArgs([]string{"--count", "0"})
	err := cmd.Execute()
	if err == nil {
		t.Fatal("expected error from --count=0")
	}
	if !strings.Contains(err.Error(), "count must be > 0") {
		t.Errorf("error missing expected phrase: %v", err)
	}
}
