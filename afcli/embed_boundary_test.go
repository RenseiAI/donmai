package afcli

// embed_boundary_test.go — regression guard for the embed-boundary debrand.
//
// When an embedder sets BinaryName (e.g. "rensei"), no user-facing help or
// error-hint string emitted by RegisterCommands' subtree should contain a
// hardcoded "`donmai <verb>`" command invocation.  The pattern "`donmai "
// (backtick + "donmai ") is the canonical marker: it means a help string is
// still directing the user to run a non-existent binary.
//
// The test:
//  1. Registers all afcli commands under a root with BinaryName="testhost".
//  2. Walks the entire command tree recursively.
//  3. For each command, collects Use/Short/Long/Example strings.
//  4. Asserts none of them contains the pattern "`donmai " (backtick followed
//     by the word "donmai" followed by a space — the exact shape that appears
//     in user-actionable command hints).
//
// It does NOT check for the bare word "donmai" in prose (e.g. env var names
// like DONMAI_DAEMON_URL, path prefixes like .donmai/, or comments); only the
// invocation pattern matters for embed-boundary correctness.

import (
	"fmt"
	"strings"
	"testing"

	"github.com/RenseiAI/donmai/afclient"
	"github.com/spf13/cobra"
)

// backtickDonmaiVerb is the exact pattern that indicates a hardcoded
// command invocation that will be wrong when embedded as another binary.
const backtickDonmaiVerb = "`donmai "

// collectCommandStrings gathers all user-facing text fields for a command.
func collectCommandStrings(cmd *cobra.Command) []string {
	return []string{
		cmd.Use,
		cmd.Short,
		cmd.Long,
		cmd.Example,
	}
}

// walkCommandTree visits cmd and all its sub-commands recursively.
// For each command it calls fn with the command's full path and the
// collected user-facing strings.
func walkCommandTree(cmd *cobra.Command, path string, fn func(path string, fields []string)) {
	fn(path, collectCommandStrings(cmd))
	for _, sub := range cmd.Commands() {
		subPath := path + " " + sub.Name()
		walkCommandTree(sub, subPath, fn)
	}
}

// TestRegisterCommands_NoBinaryNameLeakWhenEmbedded asserts that when
// BinaryName is set to a non-"donmai" value, no help/error string in the
// command tree contains a hardcoded "`donmai <verb>" invocation hint.
func TestRegisterCommands_NoBinaryNameLeakWhenEmbedded(t *testing.T) {
	t.Parallel()

	const embedBinary = "testhost"

	root := &cobra.Command{
		Use:   embedBinary,
		Short: "embed boundary test root",
	}

	// Register all afcli commands with a non-default BinaryName.
	// EnableLegacyWorkerFleet ensures fleet/worker subtree is included.
	RegisterCommands(root, Config{
		BinaryName:              embedBinary,
		EnableLegacyWorkerFleet: true,
		EnableDashboard:         false,
		ClientFactory: func() afclient.DataSource {
			return afclient.NewMockClient()
		},
	})

	type leak struct {
		path  string
		field string
	}
	var leaks []leak

	walkCommandTree(root, embedBinary, func(path string, fields []string) {
		for _, f := range fields {
			if strings.Contains(f, backtickDonmaiVerb) {
				leaks = append(leaks, leak{path: path, field: f})
			}
		}
	})

	if len(leaks) > 0 {
		msgs := make([]string, 0, len(leaks))
		for _, l := range leaks {
			msgs = append(msgs, fmt.Sprintf("  command %q contains %q in:\n    %s", l.path, backtickDonmaiVerb, l.field))
		}
		t.Errorf("embed-boundary leak: %d command(s) contain hardcoded `donmai <verb>` invocations"+
			" when BinaryName=%q:\n%s\n\nFix: replace hardcoded `donmai` with the binaryName(cfg) seam.",
			len(leaks), embedBinary, strings.Join(msgs, "\n"))
	}
}

// TestRegisterCommands_StandaloneBinaryNameIsDefault asserts that the
// default (empty BinaryName) resolves to "donmai" — i.e. the seam works
// for the standalone case and doesn't accidentally suppress binary name
// hints entirely.
func TestRegisterCommands_StandaloneBinaryNameIsDefault(t *testing.T) {
	t.Parallel()

	cfg := Config{}
	if got := binaryName(cfg); got != "donmai" {
		t.Errorf("binaryName(Config{}) = %q, want %q", got, "donmai")
	}
}
