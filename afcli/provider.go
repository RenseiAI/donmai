// Package afcli provider.go — `af provider …` Cobra commands. The
// commands target the local daemon's HTTP control API at
// /api/daemon/providers* per ADR-2026-05-07-daemon-http-control-api.md
// §D1; they NEVER hit the SaaS platform and never attach an
// Authorization header (D2 — localhost-only).
package afcli

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"github.com/RenseiAI/agentfactory-tui/afclient"
	"github.com/RenseiAI/agentfactory-tui/afview/provider"
)

// providerDaemonClient is the subset of *afclient.DaemonClient used by
// the provider subcommands. Defining it here lets tests inject a fake
// without depending on httptest. Mirrors the daemonDoer pattern in
// daemon.go.
type providerDaemonClient interface {
	ListProviders() (*afclient.ListProvidersResponse, error)
	GetProvider(id string) (*afclient.ProviderEnvelope, error)
}

// providerClientFactory builds a providerDaemonClient from a
// DaemonConfig. Injected per command-tree for test isolation; the
// production default returns a real *afclient.DaemonClient.
type providerClientFactory func(cfg afclient.DaemonConfig) providerDaemonClient

// defaultProviderClientFactory is the production factory.
func defaultProviderClientFactory(cfg afclient.DaemonConfig) providerDaemonClient {
	return afclient.NewDaemonClient(cfg)
}

// providerEnvDaemonURL names the env var that overrides the daemon
// address for `af provider …` invocations. Mirrors `af agent run`'s
// RENSEI_DAEMON_URL convention.
const providerEnvDaemonURL = "RENSEI_DAEMON_URL"

// resolveProviderDaemonConfig builds a DaemonConfig honouring the
// RENSEI_DAEMON_URL env override. Empty => default (127.0.0.1:7734).
// We keep this in the same shape as the rest of the daemon-targeted
// surface so the four Wave-9 commands feel uniform.
func resolveProviderDaemonConfig() afclient.DaemonConfig {
	cfg := afclient.DefaultDaemonConfig()
	if override := strings.TrimSpace(os.Getenv(providerEnvDaemonURL)); override != "" {
		// RENSEI_DAEMON_URL is a full URL; parse host:port out of it
		// best-effort. The full URL form ("http://host:port") is the
		// public contract — fall back to defaults if parsing fails.
		if h, p, ok := splitHTTPHostPort(override); ok {
			cfg.Host = h
			cfg.Port = p
		}
	}
	return cfg
}

// splitHTTPHostPort extracts host and port from an http(s)://host:port
// URL. Returns ok=false on any malformed input — callers fall back to
// defaults. Pure helper, no side effects.
func splitHTTPHostPort(rawURL string) (host string, port int, ok bool) {
	s := rawURL
	switch {
	case strings.HasPrefix(s, "http://"):
		s = s[len("http://"):]
	case strings.HasPrefix(s, "https://"):
		s = s[len("https://"):]
	}
	s = strings.TrimRight(s, "/")
	colon := strings.LastIndexByte(s, ':')
	if colon <= 0 || colon == len(s)-1 {
		return "", 0, false
	}
	h := s[:colon]
	pstr := s[colon+1:]
	var p int
	if _, err := fmt.Sscanf(pstr, "%d", &p); err != nil {
		return "", 0, false
	}
	if p <= 0 {
		return "", 0, false
	}
	return h, p, true
}

// newProviderCmd returns the `af provider` subcommand tree. The ds
// argument is accepted for signature consistency with the rest of
// afcli's command factories but is not used — provider commands
// target the local daemon, not the platform.
func newProviderCmd(_ func() afclient.DataSource) *cobra.Command {
	return newProviderCmdWithFactory(defaultProviderClientFactory)
}

// newProviderCmdWithFactory is the injectable variant used in tests.
func newProviderCmdWithFactory(factory providerClientFactory) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "provider",
		Short: "Inspect registered providers on the local daemon",
		Long: `Commands for inspecting registered providers on the local af daemon.

Providers are queried from the daemon's HTTP control API at
http://127.0.0.1:7734 by default. Set ` + providerEnvDaemonURL + ` to override.

The local daemon currently surfaces only the AgentRuntime family
(claude/codex/ollama/opencode/gemini/amp/stub). The seven other
Provider Families return as empty until per-family registries land
in a future wave; the response carries a partialCoverage flag so
this is honest rather than silent.`,
		SilenceUsage: true,
	}
	cmd.AddCommand(newProviderListCmd(factory))
	cmd.AddCommand(newProviderShowCmd(factory))
	return cmd
}

// newProviderListCmd builds the `af provider list` subcommand.
func newProviderListCmd(factory providerClientFactory) *cobra.Command {
	var jsonOut bool
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List registered providers grouped by family",
		Long: `List all registered providers grouped by family.

Families: Sandbox, Workarea, AgentRuntime, VCS, IssueTracker, Deployment,
AgentRegistry, Kit.

Each row shows: name, version, scope, status. Pass --json to get
machine-readable output.`,
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			client := factory(resolveProviderDaemonConfig())
			return runProviderList(cmd.OutOrStdout(), client, jsonOut)
		},
	}
	cmd.Flags().BoolVar(&jsonOut, "json", false, "Output as JSON")
	return cmd
}

func runProviderList(out io.Writer, client providerDaemonClient, jsonOut bool) error {
	resp, err := client.ListProviders()
	if err != nil {
		return fmt.Errorf("list providers: %w", err)
	}
	if jsonOut {
		return encodeProviderJSON(out, resp)
	}
	noColor := provider.NoColorEnv()
	return provider.RenderList(out, resp.Providers, noColor)
}

// newProviderShowCmd builds the `af provider show <id>` subcommand.
func newProviderShowCmd(factory providerClientFactory) *cobra.Command {
	var jsonOut bool
	cmd := &cobra.Command{
		Use:   "show <id>",
		Short: "Show details for a single provider",
		Long: `Show full details for a registered provider by its ID.

Displays: name, family, version, scope, status, source, trust state
(signed/unsigned/verified), signer identity, and capability flags.

Trust symbols:
  ✅  signed and verified
  ⚠   signed but unverified
  🔓  unsigned

Plain ASCII symbols are used when NO_COLOR=1 is set.`,
		Args:         cobra.ExactArgs(1),
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			client := factory(resolveProviderDaemonConfig())
			return runProviderShow(cmd.OutOrStdout(), client, args[0], jsonOut)
		},
	}
	cmd.Flags().BoolVar(&jsonOut, "json", false, "Output as JSON")
	return cmd
}

func runProviderShow(out io.Writer, client providerDaemonClient, id string, jsonOut bool) error {
	env, err := client.GetProvider(id)
	if err != nil {
		if errors.Is(err, afclient.ErrNotFound) {
			return fmt.Errorf("provider not found: %s", id)
		}
		return fmt.Errorf("get provider: %w", err)
	}
	if jsonOut {
		return encodeProviderJSON(out, &env.Provider)
	}
	noColor := provider.NoColorEnv()
	return provider.RenderShow(out, &env.Provider, noColor)
}

// encodeProviderJSON writes v as pretty-printed JSON to out.
func encodeProviderJSON(out io.Writer, v any) error {
	enc := json.NewEncoder(out)
	enc.SetIndent("", "  ")
	if err := enc.Encode(v); err != nil {
		return fmt.Errorf("encode JSON: %w", err)
	}
	return nil
}
