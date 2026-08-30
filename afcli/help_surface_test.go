package afcli

// help_surface_test.go — what `--help` ADVERTISES versus what the binary still
// ACCEPTS. Those are two different contracts and this file pins both.
//
// A Cobra command carrying .Deprecated is excluded from the rendered
// `Available Commands:` block (cobra.Command.IsAvailableCommand). That is the
// intended effect of marking `worker`/`fleet` deprecated: they stop being
// advertised public surface. It is NOT a removal — both stay registered behind
// EnableLegacyWorkerFleet, resolve, and print their deprecation notice until
// the removal version they name.
//
// The existing per-command tests (TestWorkerIsDeprecated, TestFleetIsDeprecated)
// assert the .Deprecated FIELD. Nothing asserted the consequence — the rendered
// help surface — which is precisely the contract that moved, and precisely what
// a downstream help-shape guard reads. This file closes that gap on the side
// that owns the decision.

import (
	"bytes"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/RenseiAI/donmai/afclient"
)

// standaloneTree builds the command tree exactly as the standalone binary does
// (cmd/donmai/main.go): legacy worker/fleet, dashboard, and formal A2A on.
func standaloneTree(t *testing.T) *cobra.Command {
	t.Helper()
	root := &cobra.Command{Use: "donmai"}
	RegisterCommands(root, Config{
		BinaryName:              "donmai",
		ClientFactory:           func() afclient.DataSource { return afclient.NewMockClient() },
		EnableDashboard:         true,
		EnableLegacyWorkerFleet: true,
		EnableA2AClient:         true,
	})
	if len(root.Commands()) == 0 {
		t.Fatal("RegisterCommands registered nothing — test fixture is stale")
	}
	return root
}

// runTree executes args against a freshly built tree and returns everything the
// user would see. Cobra writes both help and the deprecation notice through the
// command's out writer, so one buffer captures both.
func runTree(t *testing.T, args ...string) string {
	t.Helper()
	root := standaloneTree(t)
	var buf bytes.Buffer
	root.SetOut(&buf)
	root.SetErr(&buf)
	root.SetArgs(args)
	if err := root.Execute(); err != nil {
		t.Fatalf("Execute(%v): %v\n%s", args, err, buf.String())
	}
	return buf.String()
}

// advertisedCommands parses the `Available Commands:` block out of rendered
// help — the same block a user reads and a help-shape guard scrapes.
func advertisedCommands(t *testing.T, help string) map[string]bool {
	t.Helper()
	names := map[string]bool{}
	var inBlock bool
	for _, line := range strings.Split(help, "\n") {
		if strings.HasPrefix(line, "Available Commands:") {
			inBlock = true
			continue
		}
		if !inBlock {
			continue
		}
		if strings.TrimSpace(line) == "" {
			break
		}
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		names[fields[0]] = true
	}
	if len(names) == 0 {
		t.Fatalf("no Available Commands block parsed — the parser is broken, so every "+
			"absence assertion below would be vacuous\n--- help ---\n%s", help)
	}
	// Anti-vacuity anchor: `host` is the replacement surface and must always be
	// advertised. If it is missing, the parse is wrong, not the tree.
	if !names["host"] {
		t.Fatalf("parsed Available Commands %v does not contain `host`; parser is unreliable\n"+
			"--- help ---\n%s", sortedNames(names), help)
	}
	return names
}

func sortedNames(set map[string]bool) []string {
	out := make([]string, 0, len(set))
	for name := range set {
		out = append(out, name)
	}
	return out
}

// TestHelpAdvertisesExactlyTheSupportedSurface pins the invariant behind the
// help listing: `--help` advertises every supported top-level command and no
// hidden or deprecated one. Stated as an invariant over the live tree rather
// than a hardcoded roster, so the next command to be deprecated is covered the
// day it is marked, with no baseline to update.
func TestHelpAdvertisesExactlyTheSupportedSurface(t *testing.T) {
	t.Parallel()

	help := runTree(t, "--help")
	advertised := advertisedCommands(t, help)

	var checked int
	for _, cmd := range standaloneTree(t).Commands() {
		name := cmd.Name()
		unadvertised := cmd.Hidden || cmd.Deprecated != ""
		if unadvertised && advertised[name] {
			t.Errorf("%q is hidden=%v deprecated=%q but is still advertised in Available Commands",
				name, cmd.Hidden, cmd.Deprecated)
		}
		if !unadvertised && !advertised[name] {
			t.Errorf("%q is supported public surface but is missing from Available Commands\n"+
				"--- help ---\n%s", name, help)
		}
		checked++
	}
	if checked < 15 {
		t.Fatalf("only %d top-level commands checked; the fixture stopped building the real tree", checked)
	}
}

// TestDeprecatedLegacyCommandsAreUnadvertisedButStillWork is the compatibility
// contract for the retired `worker`/`fleet` nouns, both halves in one place:
// they are gone from the advertised surface, and they still run and say so.
// Deleting either command outright before its declared removal version fails
// the resolve half; un-deprecating it fails the advertise half.
func TestDeprecatedLegacyCommandsAreUnadvertisedButStillWork(t *testing.T) {
	t.Parallel()

	advertised := advertisedCommands(t, runTree(t, "--help"))

	for _, name := range []string{"worker", "fleet"} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			if advertised[name] {
				t.Errorf("%q is deprecated and must not be advertised in Available Commands", name)
			}

			cmd, _, err := standaloneTree(t).Find([]string{name})
			if err != nil {
				t.Fatalf("Find(%s): %v — deprecation must not remove the command before %s",
					name, err, legacyWorkerFleetRemovalVersion)
			}
			if cmd.Name() != name {
				t.Fatalf("Find(%s) resolved to %q", name, cmd.Name())
			}
			if cmd.Deprecated == "" {
				t.Fatalf("%q must carry a Deprecated notice", name)
			}

			out := runTree(t, name, "--help")
			if !strings.Contains(out, "is deprecated") {
				t.Errorf("`donmai %s --help` printed no deprecation notice:\n%s", name, out)
			}
			if !strings.Contains(out, legacyWorkerFleetRemovalVersion) {
				t.Errorf("`donmai %s --help` notice does not name the removal version %q:\n%s",
					name, legacyWorkerFleetRemovalVersion, out)
			}
			if !strings.Contains(out, "host") {
				t.Errorf("`donmai %s --help` notice does not point at the replacement `host`:\n%s", name, out)
			}
		})
	}
}
