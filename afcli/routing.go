// Package afcli routing.go — `af routing …` Cobra commands. The
// commands target the local daemon's HTTP control API at
// /api/daemon/routing/* per
// ADR-2026-05-07-daemon-http-control-api.md §D1; they NEVER hit the
// SaaS platform and never attach an Authorization header (D2 —
// localhost-only).
package afcli

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/spf13/cobra"

	"github.com/RenseiAI/agentfactory-tui/afclient"
	"github.com/RenseiAI/agentfactory-tui/afview/routing"
)

// routingDaemonClient is the subset of *afclient.DaemonClient used by
// the routing subcommands. Defining it here lets tests inject a fake
// without depending on httptest. Mirrors the providerDaemonClient
// pattern in provider.go.
type routingDaemonClient interface {
	GetRoutingConfig() (*afclient.RoutingConfigResponse, error)
	ExplainRouting(sessionID string) (*afclient.RoutingExplainResponse, error)
}

// routingClientFactory builds a routingDaemonClient from a DaemonConfig.
// Injected per command-tree for test isolation; the production default
// returns a real *afclient.DaemonClient.
type routingClientFactory func(cfg afclient.DaemonConfig) routingDaemonClient

// defaultRoutingClientFactory is the production factory.
func defaultRoutingClientFactory(cfg afclient.DaemonConfig) routingDaemonClient {
	return afclient.NewDaemonClient(cfg)
}

// newRoutingCmd returns the `af routing` subcommand tree. The ds
// argument is accepted for signature consistency with the rest of
// afcli's command factories but is not used — routing commands target
// the local daemon, not the platform.
func newRoutingCmd(_ func() afclient.DataSource) *cobra.Command {
	return newRoutingCmdWithFactory(defaultRoutingClientFactory)
}

// newRoutingCmdWithFactory is the injectable variant used in tests.
func newRoutingCmdWithFactory(factory routingClientFactory) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "routing",
		Short: "Inspect 2D routing decisions (LLM × sandbox)",
		Long: `Commands for inspecting the cross-provider scheduler on the local af daemon.

Routing decisions are queried from the daemon's HTTP control API at
http://127.0.0.1:7734 by default. Set ` + providerEnvDaemonURL + ` to override.

Subcommands:
  show       Display current routing config and recent decisions
  explain    Show the full decision trace for a specific session

This wave (Wave 9) ships read-only inspection. The decision trace is
sourced from the daemon's in-process ring buffer; sessions whose
decisions have aged out return "not found".`,
		SilenceUsage: true,
	}
	cmd.AddCommand(newRoutingShowCmd(factory))
	cmd.AddCommand(newRoutingExplainCmd(factory))
	return cmd
}

// ─── show ─────────────────────────────────────────────────────────────────────

// newRoutingShowCmd builds the `af routing show` subcommand.
func newRoutingShowCmd(factory routingClientFactory) *cobra.Command {
	var (
		jsonOut  bool
		plainOut bool
	)
	cmd := &cobra.Command{
		Use:   "show",
		Short: "Display current routing config, Thompson-Sampling state, and recent decisions",
		Long: `Display the current routing configuration and Thompson-Sampling state.

Output includes:
  • Active capability filters (region, OS, arch, GPU, etc.)
  • Scoring weights (cost vs latency; default cost=70%, latency=30%)
  • Per-provider Thompson-Sampling state (alpha, beta, recent score/cost/latency)
  • 2D heatmap: LLM rows × sandbox columns, cell = mean composite score
  • Recent routing decisions table

With --json the snapshot prints as machine-readable JSON. With --plain
the deterministic plain-text form (rensei-smokes pin point) is emitted
instead of the ANSI table form.`,
		Args:         cobra.NoArgs,
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			client := factory(resolveProviderDaemonConfig())
			return runRoutingShow(cmd.OutOrStdout(), client, jsonOut, plainOut)
		},
	}
	cmd.Flags().BoolVar(&jsonOut, "json", false, "Output as JSON")
	cmd.Flags().BoolVar(&plainOut, "plain", false, "Output plain-text form (no ANSI)")
	return cmd
}

func runRoutingShow(out io.Writer, client routingDaemonClient, jsonOut, plainOut bool) error {
	resp, err := client.GetRoutingConfig()
	if err != nil {
		return fmt.Errorf("get routing config: %w", err)
	}
	if jsonOut {
		return encodeRoutingJSON(out, resp)
	}
	if plainOut {
		return routing.PlainShow(out, &resp.Config)
	}
	noColor := routing.NoColorEnv()
	return routing.RenderShow(out, &resp.Config, noColor)
}

// ─── explain ──────────────────────────────────────────────────────────────────

// newRoutingExplainCmd builds the `af routing explain <session-id>`
// subcommand.
func newRoutingExplainCmd(factory routingClientFactory) *cobra.Command {
	var (
		jsonOut  bool
		plainOut bool
	)
	cmd := &cobra.Command{
		Use:   "explain <session-id>",
		Short: "Show the decision trace for a specific session",
		Long: `Show why a session was routed to its chosen LLM and sandbox provider.

The decision trace walks through each scheduler phase in order:
  1. capability-filter  — hard constraints (os, arch, region, GPU, duration)
  2. tenant-policy      — Layer 6 hooks and preferredProviders / forbiddenProviders
  3. capacity-filter    — providers above 90% maxConcurrent or reporting unhealthy
  4. score              — Thompson-Sampling composite score (cost + latency)

For each phase, the trace shows which providers were eliminated and why.
The final step shows the chosen provider and its composite score.

Sessions whose decisions have aged out of the daemon's in-process ring
buffer return "not found".`,
		Args:         cobra.ExactArgs(1),
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			sessionID := strings.TrimSpace(args[0])
			if sessionID == "" {
				return errors.New("session-id must not be empty")
			}
			client := factory(resolveProviderDaemonConfig())
			return runRoutingExplain(cmd.OutOrStdout(), client, sessionID, jsonOut, plainOut)
		},
	}
	cmd.Flags().BoolVar(&jsonOut, "json", false, "Output as JSON")
	cmd.Flags().BoolVar(&plainOut, "plain", false, "Output plain-text form (no ANSI)")
	return cmd
}

func runRoutingExplain(out io.Writer, client routingDaemonClient, sessionID string, jsonOut, plainOut bool) error {
	resp, err := client.ExplainRouting(sessionID)
	if err != nil {
		if errors.Is(err, afclient.ErrNotFound) {
			return fmt.Errorf("routing decision not found for session %q", sessionID)
		}
		return fmt.Errorf("explain routing for session %q: %w", sessionID, err)
	}
	if jsonOut {
		return encodeRoutingJSON(out, resp)
	}
	if plainOut {
		return routing.PlainExplain(out, resp)
	}
	noColor := routing.NoColorEnv()
	return routing.RenderExplain(out, resp, noColor)
}

// encodeRoutingJSON writes v as pretty-printed JSON to out.
func encodeRoutingJSON(out io.Writer, v any) error {
	enc := json.NewEncoder(out)
	enc.SetIndent("", "  ")
	if err := enc.Encode(v); err != nil {
		return fmt.Errorf("encode JSON: %w", err)
	}
	return nil
}
