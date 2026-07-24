// Package afcli provides Cobra command factories for the Donmai CLI.
// Downstream projects can import this package and call
// RegisterCommands to add the shared donmai subcommands to their own root command.
package afcli

import (
	"github.com/RenseiAI/donmai/afcli/credentials"
	"github.com/RenseiAI/donmai/afcli/linearcmd"
	"github.com/RenseiAI/donmai/afclient"
	"github.com/spf13/cobra"
)

// Config controls how af commands are wired into a parent CLI.
type Config struct {
	// ClientFactory returns an afclient.DataSource for API calls.
	// Required.
	ClientFactory func() afclient.DataSource

	// DefaultURLFunc is a lazy URL resolution function (for flag-based callers).
	// Optional. Checked before DefaultURL.
	DefaultURLFunc func() string

	// DefaultURL is the fallback API base URL if DefaultURLFunc is nil.
	DefaultURL string

	// EnableDashboard registers the dashboard command when true.
	EnableDashboard bool

	// EnableLegacyWorkerFleet registers the legacy worker/fleet process
	// commands when true. These commands remain available to the standalone
	// OSS af binary for local debugging, but embedders should usually expose
	// daemon as the host lifecycle surface instead.
	EnableLegacyWorkerFleet bool

	// ProjectFunc returns the active project slug (or ID) used to scope
	// list endpoints that support filtering AND to populate the
	// `X-Rensei-Project` header on every request (see OrgFunc note).
	// Returning an empty string means fleet-wide (no scope), preserving
	// the default behavior. Optional. When nil, all commands behave
	// fleet-wide.
	ProjectFunc func() string

	// OrgFunc returns the active org id (or slug, or WorkOS org id)
	// the embedding binary wants every afcli-imported command to scope
	// to. When non-empty, the value is sent as `X-Rensei-Org` on every
	// HTTP request the wrapped ClientFactory produces.
	//
	// Why this matters: the platform's CLI user-token auth selects the
	// caller's org membership from the WorkOS access token's `org_id`
	// claim, which is frozen to whichever org the user happened to be
	// in at token-mint time. With multiple humans + agents on a single
	// host running across many orgs concurrently, that frozen claim
	// silently misroutes commands to the wrong org. Sending an explicit
	// `X-Rensei-Org` per invocation makes the scope authoritative
	// (after a server-side membership check) and removes the implicit
	// dependency on token state.
	//
	// Optional. When nil OR returns an empty string, no header is
	// sent and the server falls back to its own resolution (single-org
	// users keep working unchanged).
	OrgFunc func() string

	// HostBinaryVersion is the version string the embedding binary
	// reports (typically injected via -ldflags into the main package).
	// When non-empty, `daemon run` passes it to daemon.Options.Version
	// so /api/daemon/status reports the running binary's version
	// instead of donmai's vendored package default. Empty
	// falls back to the daemon package's own Version var.
	HostBinaryVersion string

	// BinaryName is the user-facing binary name to embed in error messages,
	// usage hints, and remediation instructions. Defaults to "donmai" when empty.
	// Embedders (e.g. rensei-tui) set this to "rensei".
	BinaryName string
}

// scopedClientFactory wraps cfg.ClientFactory so every produced Client
// carries the OrgScope / ProjectScope derived from OrgFunc / ProjectFunc
// at call time. Per-invocation: each call to the returned factory
// re-evaluates the funcs, so an embedder that exposes per-command --org
// flags can override scope without mutating global state. Non-Client
// DataSources (e.g. MockClient in tests) pass through unmodified —
// scope is a no-op for them.
func scopedClientFactory(cfg Config) func() afclient.DataSource {
	return func() afclient.DataSource {
		ds := cfg.ClientFactory()
		c, ok := ds.(*afclient.Client)
		if !ok {
			return ds
		}
		if cfg.OrgFunc != nil {
			if v := cfg.OrgFunc(); v != "" {
				c.OrgScope = v
			}
		}
		if cfg.ProjectFunc != nil {
			if v := cfg.ProjectFunc(); v != "" {
				c.ProjectScope = v
			}
		}
		return c
	}
}

// resolveURL returns the base URL to use, checking DefaultURLFunc first,
// then DefaultURL, then "http://localhost:3000".
func (c Config) resolveURL() string {
	if c.DefaultURLFunc != nil {
		if u := c.DefaultURLFunc(); u != "" {
			return u
		}
	}
	if c.DefaultURL != "" {
		return c.DefaultURL
	}
	return "http://localhost:3000"
}

// RegisterCommands adds the shared Donmai subcommands to the given root
// command. Optional local/debug surfaces are controlled by Config. The commands
// use cfg to resolve API clients and defaults.
//
// This is the primary integration point for downstream CLIs that want
// to embed Donmai functionality under their own root command
// (e.g. `mycli agent list`, `mycli daemon status`, etc.).
func RegisterCommands(root *cobra.Command, cfg Config) {
	// Wrap ClientFactory so every produced client carries the active
	// org/project scope as `X-Rensei-Org` / `X-Rensei-Project` headers.
	// Subcommands consume `ds` exactly as before — the wrapping is
	// transparent to them.
	ds := scopedClientFactory(cfg)
	root.AddCommand(newStatusCmd(ds))
	root.AddCommand(newAgentCmd(ds, cfg.ProjectFunc, cfg))
	root.AddCommand(newSessionCmd(ds, cfg.ProjectFunc))
	root.AddCommand(newGovernorCmd(ds, cfg))
	if cfg.EnableLegacyWorkerFleet {
		root.AddCommand(newWorkerCmd(ds, cfg))
		root.AddCommand(newFleetCmd(ds, cfg))
	}
	root.AddCommand(newDaemonCmd(cfg))
	root.AddCommand(newProjectCmd(cfg))
	root.AddCommand(newOrchestratorCmd(cfg))
	root.AddCommand(newCodeCmd(cfg))
	root.AddCommand(newMCPCmd(cfg))
	root.AddCommand(newEvalCmd(cfg))
	root.AddCommand(newArchCmd(cfg))
	root.AddCommand(linearcmd.New(ds, binaryName(cfg)))
	root.AddCommand(newGitHubCmd(ds, cfg))
	root.AddCommand(newLogsCmd())
	root.AddCommand(newAdminCmd())
	root.AddCommand(credentials.NewCmd())
	root.AddCommand(newProviderCmd(ds))
	root.AddCommand(newKitCmd(ds))
	root.AddCommand(newRoutingCmd(ds))
	root.AddCommand(newWorkareaCmd(ds))
	root.AddCommand(newHostWatchCmd())
	if cfg.EnableDashboard {
		root.AddCommand(newDashboardCmd(cfg))
	}
}
