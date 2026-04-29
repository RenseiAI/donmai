package afcli

import (
	"bytes"
	"sort"
	"sync/atomic"
	"testing"

	"github.com/spf13/cobra"

	"github.com/RenseiAI/agentfactory-tui/afclient"
)

// subcommandNames returns the Use names of root's immediate children,
// sorted for deterministic comparison. Only the first whitespace-
// separated token is kept so subcommands like `show <id>` compare as
// `show`.
func subcommandNames(root *cobra.Command) []string {
	children := root.Commands()
	names := make([]string, 0, len(children))
	for _, c := range children {
		names = append(names, c.Name())
	}
	sort.Strings(names)
	return names
}

// hasSubcommand reports whether root has an immediate child named name.
func hasSubcommand(root *cobra.Command, name string) bool {
	for _, c := range root.Commands() {
		if c.Name() == name {
			return true
		}
	}
	return false
}

// TestRegisterCommandsWiring verifies that RegisterCommands attaches the
// expected set of subcommands to the parent command, gated by Config.
// This is the integration point downstream CLIs rely on; a silent
// regression here would only surface in those consumers.
func TestRegisterCommandsWiring(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		cfg         Config
		wantPresent []string
		wantAbsent  []string
	}{
		{
			name: "default_config_omits_dashboard",
			cfg: Config{
				ClientFactory: func() afclient.DataSource { return afclient.NewMockClient() },
			},
			wantPresent: []string{"admin", "agent", "fleet", "governor", "logs", "session", "status", "worker"},
			wantAbsent:  []string{"dashboard"},
		},
		{
			name: "enable_dashboard_registers_dashboard",
			cfg: Config{
				ClientFactory:   func() afclient.DataSource { return afclient.NewMockClient() },
				EnableDashboard: true,
			},
			wantPresent: []string{"admin", "agent", "dashboard", "fleet", "governor", "logs", "session", "status", "worker"},
			wantAbsent:  nil,
		},
	}

	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			root := &cobra.Command{Use: "af"}
			RegisterCommands(root, tc.cfg)

			for _, want := range tc.wantPresent {
				if !hasSubcommand(root, want) {
					t.Errorf("missing subcommand %q; got children: %v",
						want, subcommandNames(root))
				}
			}
			for _, reject := range tc.wantAbsent {
				if hasSubcommand(root, reject) {
					t.Errorf("unexpected subcommand %q registered; got children: %v",
						reject, subcommandNames(root))
				}
			}
		})
	}
}

// TestRegisterCommandsClientFactoryLazy verifies that ClientFactory is
// not invoked during RegisterCommands. Downstream CLIs register the
// commands before flags are parsed, so eager evaluation would read
// stale or unparsed configuration. The factory must only be called
// when a subcommand's RunE actually executes.
func TestRegisterCommandsClientFactoryLazy(t *testing.T) {
	t.Parallel()

	var calls atomic.Int32
	factory := func() afclient.DataSource {
		calls.Add(1)
		return afclient.NewMockClient()
	}

	root := &cobra.Command{Use: "af"}
	RegisterCommands(root, Config{
		ClientFactory:   factory,
		EnableDashboard: true,
	})

	if got := calls.Load(); got != 0 {
		t.Fatalf("ClientFactory called %d times during RegisterCommands; want 0", got)
	}

	// Executing a command that consumes the DataSource must trigger
	// the factory. `agent list --json` calls ds() inside its RunE
	// and writes its output via cmd.OutOrStdout(), so we can route
	// output to a buffer instead of swapping os.Stdout (which would
	// race with other parallel tests in this package).
	buf := &bytes.Buffer{}
	root.SetOut(buf)
	root.SetErr(buf)
	root.SetArgs([]string{"agent", "list", "--json"})
	if err := root.Execute(); err != nil {
		t.Fatalf("root.Execute: %v", err)
	}

	if got := calls.Load(); got < 1 {
		t.Errorf("ClientFactory called %d times after executing `agent list`; want >= 1", got)
	}
}
