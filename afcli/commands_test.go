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
			name: "default_config_omits_dashboard_and_legacy_worker_fleet",
			cfg: Config{
				ClientFactory: func() afclient.DataSource { return afclient.NewMockClient() },
			},
			wantPresent: []string{"admin", "agent", "daemon", "governor", "logs", "session", "status"},
			wantAbsent:  []string{"dashboard", "fleet", "worker"},
		},
		{
			name: "enable_dashboard_registers_dashboard_without_legacy_worker_fleet",
			cfg: Config{
				ClientFactory:   func() afclient.DataSource { return afclient.NewMockClient() },
				EnableDashboard: true,
			},
			wantPresent: []string{"admin", "agent", "daemon", "dashboard", "governor", "logs", "session", "status"},
			wantAbsent:  []string{"fleet", "worker"},
		},
		{
			name: "enable_legacy_worker_fleet_registers_standalone_process_commands",
			cfg: Config{
				ClientFactory:           func() afclient.DataSource { return afclient.NewMockClient() },
				EnableLegacyWorkerFleet: true,
			},
			wantPresent: []string{"admin", "agent", "daemon", "fleet", "governor", "logs", "session", "status", "worker"},
			wantAbsent:  []string{"dashboard"},
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

// TestScopedClientFactoryAppliesOrgAndProject pins the per-invocation
// scope wiring: when OrgFunc / ProjectFunc are configured, the wrapped
// factory must set the matching fields on each produced *Client. This
// is the seam that fixes the multi-org misroute (afcli-imported
// commands inheriting whatever WorkOS org_id the access token was
// minted with) — without it, every release of rensei-tui that pulled
// afcli would silently send commands to the wrong org for any operator
// holding a stale token.
func TestScopedClientFactoryAppliesOrgAndProject(t *testing.T) {
	t.Parallel()

	cfg := Config{
		ClientFactory: func() afclient.DataSource {
			return afclient.NewAuthenticatedClient("https://example.test", "rsk_test")
		},
		OrgFunc:     func() string { return "org_supaku" },
		ProjectFunc: func() string { return "yuisei" },
	}
	ds := scopedClientFactory(cfg)()
	c, ok := ds.(*afclient.Client)
	if !ok {
		t.Fatalf("scoped factory did not return *afclient.Client; got %T", ds)
	}
	if c.OrgScope != "org_supaku" {
		t.Errorf("OrgScope = %q, want org_supaku", c.OrgScope)
	}
	if c.ProjectScope != "yuisei" {
		t.Errorf("ProjectScope = %q, want yuisei", c.ProjectScope)
	}
}

// TestScopedClientFactoryReevaluatesPerCall confirms that each call to
// the wrapped factory re-runs OrgFunc / ProjectFunc, so per-invocation
// `--org` / `--project` flag overrides take effect without the embedder
// rebuilding the command tree. Multi-agent / multi-shell concurrency on
// a single host depends on this — each agent process passes its own
// scope per command without mutating shared state.
func TestScopedClientFactoryReevaluatesPerCall(t *testing.T) {
	t.Parallel()

	var orgCallNum int
	cfg := Config{
		ClientFactory: func() afclient.DataSource {
			return afclient.NewAuthenticatedClient("https://example.test", "rsk_test")
		},
		OrgFunc: func() string {
			orgCallNum++
			if orgCallNum == 1 {
				return "org_a"
			}
			return "org_b"
		},
	}
	factory := scopedClientFactory(cfg)
	first := factory().(*afclient.Client)
	second := factory().(*afclient.Client)
	if first.OrgScope != "org_a" {
		t.Errorf("first OrgScope = %q, want org_a", first.OrgScope)
	}
	if second.OrgScope != "org_b" {
		t.Errorf("second OrgScope = %q, want org_b", second.OrgScope)
	}
}

// TestScopedClientFactoryEmptyScopeNoHeader confirms that returning an
// empty string from OrgFunc / ProjectFunc leaves the scope field empty
// (and therefore omits the header on the wire — the Client.set headers
// helper is the unit-tested half of that contract). Single-org users
// who don't configure scope keep the pre-change behaviour.
func TestScopedClientFactoryEmptyScopeNoHeader(t *testing.T) {
	t.Parallel()

	cfg := Config{
		ClientFactory: func() afclient.DataSource {
			return afclient.NewAuthenticatedClient("https://example.test", "rsk_test")
		},
		OrgFunc:     func() string { return "" },
		ProjectFunc: func() string { return "" },
	}
	c := scopedClientFactory(cfg)().(*afclient.Client)
	if c.OrgScope != "" {
		t.Errorf("OrgScope = %q, want empty", c.OrgScope)
	}
	if c.ProjectScope != "" {
		t.Errorf("ProjectScope = %q, want empty", c.ProjectScope)
	}
}

// TestScopedClientFactoryNonClientPassthrough confirms that the wrapper
// does not try to set scope on a DataSource that isn't an *afclient.Client
// — e.g. MockClient in tests. The test harness should keep working
// unchanged after the wrapping change.
func TestScopedClientFactoryNonClientPassthrough(t *testing.T) {
	t.Parallel()

	cfg := Config{
		ClientFactory: func() afclient.DataSource { return afclient.NewMockClient() },
		OrgFunc:       func() string { return "org_supaku" },
	}
	// Should not panic and should return whatever ClientFactory produced.
	ds := scopedClientFactory(cfg)()
	if _, ok := ds.(*afclient.MockClient); !ok {
		t.Errorf("expected MockClient passthrough, got %T", ds)
	}
}
