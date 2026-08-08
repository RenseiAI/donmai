package afcli

import (
	"bytes"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"

	"github.com/RenseiAI/donmai/afclient"
	"github.com/RenseiAI/donmai/worker"
)

// TestFleetParentHelp verifies the fleet parent command exposes all three
// subcommands via --help.
func TestFleetParentHelp(t *testing.T) {
	t.Parallel()

	cmd := newFleetCmd(func() afclient.DataSource { return afclient.NewMockClient() }, Config{})
	buf := &bytes.Buffer{}
	cmd.SetOut(buf)
	cmd.SetErr(buf)
	cmd.SetArgs([]string{"--help"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("execute: %v", err)
	}
	for _, want := range []string{"start", "stop", "status"} {
		if !strings.Contains(buf.String(), want) {
			t.Errorf("fleet --help missing subcommand %q; got:\n%s", want, buf.String())
		}
	}
}

// TestFleetScaleRemoved is the D3 acceptance: `fleet scale` — a subcommand
// that only ever returned an error, never a capability with a deprecation
// cost — is deleted outright, not deprecated. `donmai fleet scale` must fail
// cobra's own subcommand resolution rather than reach a stub RunE.
func TestFleetScaleRemoved(t *testing.T) {
	t.Parallel()

	root := &cobra.Command{Use: "donmai"}
	root.AddCommand(newFleetCmd(func() afclient.DataSource { return afclient.NewMockClient() }, Config{}))

	// cobra's Find does not error on an unmatched trailing arg — it just
	// stops descending and hands the extra arg back as a positional to the
	// deepest command it DID match. So the meaningful assertion is which
	// command Find stopped at, not whether it errored.
	found, _, err := root.Find([]string{"fleet", "scale"})
	if err != nil {
		t.Fatalf("Find(fleet scale): %v", err)
	}
	if found.Name() == "scale" {
		t.Fatal("`fleet scale` still resolves to a leaf command — it must be deleted, not merely undocumented")
	}
	if found.Name() != "fleet" {
		t.Fatalf("Find(fleet scale) resolved to %q, want it to stop at \"fleet\"", found.Name())
	}

	cmd := newFleetCmd(func() afclient.DataSource { return afclient.NewMockClient() }, Config{})
	for _, c := range cmd.Commands() {
		if c.Name() == "scale" {
			t.Fatalf("fleet still registers a %q child command", c.Name())
		}
	}
}

// TestFleetIsDeprecated is the D3 acceptance for `fleet`: the parent command
// carries a Cobra Deprecated marker naming `host` as the replacement and a
// concrete removal version, never an unfalsifiable "next release" promise —
// see legacyWorkerFleetRemovalVersion's comment for why that promise failed
// the previous alias generation.
func TestFleetIsDeprecated(t *testing.T) {
	t.Parallel()

	cmd := newFleetCmd(func() afclient.DataSource { return afclient.NewMockClient() }, Config{BinaryName: "donmai"})

	if cmd.Deprecated == "" {
		t.Fatal("fleet must carry a Deprecated string")
	}
	if !strings.Contains(cmd.Deprecated, legacyWorkerFleetRemovalVersion) {
		t.Errorf("Deprecated %q must name the removal version %q", cmd.Deprecated, legacyWorkerFleetRemovalVersion)
	}
	if !strings.Contains(cmd.Deprecated, "host") {
		t.Errorf("Deprecated %q must point at `host`", cmd.Deprecated)
	}
	for _, banned := range []string{"next release", "a future release", "soon"} {
		if strings.Contains(strings.ToLower(cmd.Deprecated), banned) {
			t.Errorf("Deprecated %q must not say %q — name a version", cmd.Deprecated, banned)
		}
	}
	if !strings.HasPrefix(legacyWorkerFleetRemovalVersion, "v") {
		t.Errorf("removal version %q must be a concrete vX.Y.Z tag", legacyWorkerFleetRemovalVersion)
	}
}

// TestFleetStartRequiresCount verifies the --count flag is required.
func TestFleetStartRequiresCount(t *testing.T) {
	t.Parallel()

	// cobra's MarkFlagRequired reports missing required flags via Execute.
	cmd := newFleetStartCmd("donmai")
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

	cmd := newFleetStartCmd("donmai")
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

	cmd := newFleetStatusCmd("donmai")
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

	cmd := newFleetStatusCmd("donmai")
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
