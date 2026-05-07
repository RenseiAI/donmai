// Package afcli workarea.go — `af workarea …` Cobra commands. The
// commands target the local daemon's HTTP control API at
// /api/daemon/workareas* per ADR-2026-05-07-daemon-http-control-api.md
// §D1 + §D4a; they NEVER hit the SaaS platform and never attach an
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
	workareaview "github.com/RenseiAI/agentfactory-tui/afview/workarea"
)

// workareaDaemonClient is the subset of *afclient.DaemonClient used by
// the workarea subcommands. Mirrors providerDaemonClient — defining it
// here lets tests inject a fake without depending on httptest.
type workareaDaemonClient interface {
	ListWorkareas() (*afclient.ListWorkareasResponse, error)
	GetWorkarea(id string) (*afclient.WorkareaEnvelope, error)
	RestoreWorkarea(archiveID string, req afclient.WorkareaRestoreRequest) (*afclient.WorkareaRestoreResult, error)
	DiffWorkareas(idA, idB string) (*afclient.WorkareaDiffResult, error)
}

// workareaClientFactory builds a workareaDaemonClient from a
// DaemonConfig. Injected per command-tree for test isolation.
type workareaClientFactory func(cfg afclient.DaemonConfig) workareaDaemonClient

// defaultWorkareaClientFactory is the production factory.
func defaultWorkareaClientFactory(cfg afclient.DaemonConfig) workareaDaemonClient {
	return afclient.NewDaemonClient(cfg)
}

// workareaEnvDaemonURL — env var that overrides the daemon address for
// `af workarea …` invocations. Mirrors `RENSEI_DAEMON_URL` everywhere
// else.
const workareaEnvDaemonURL = "RENSEI_DAEMON_URL"

// resolveWorkareaDaemonConfig builds a DaemonConfig honouring the
// RENSEI_DAEMON_URL env override. Empty => default (127.0.0.1:7734).
func resolveWorkareaDaemonConfig() afclient.DaemonConfig {
	cfg := afclient.DefaultDaemonConfig()
	if override := strings.TrimSpace(os.Getenv(workareaEnvDaemonURL)); override != "" {
		if h, p, ok := splitHTTPHostPort(override); ok {
			cfg.Host = h
			cfg.Port = p
		}
	}
	return cfg
}

// newWorkareaCmd returns the `af workarea` subcommand tree. The ds
// argument is accepted for signature consistency with the rest of
// afcli's command factories but is not used — workarea commands target
// the local daemon, not the platform.
func newWorkareaCmd(_ func() afclient.DataSource) *cobra.Command {
	return newWorkareaCmdWithFactory(defaultWorkareaClientFactory)
}

// newWorkareaCmdWithFactory is the injectable variant used in tests.
func newWorkareaCmdWithFactory(factory workareaClientFactory) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "workarea",
		Short: "Inspect and manage local-daemon workareas",
		Long: `Commands for listing, inspecting, restoring, and diffing workareas
on the local af daemon.

Workareas are queried from the daemon's HTTP control API at
http://127.0.0.1:7734 by default. Set ` + workareaEnvDaemonURL + ` to override.

Active pool members and on-disk archives are surfaced through the same
list; the ` + "`kind`" + ` field on each entry disambiguates. Archives are
immutable — restore materialises a new active workarea from an
archive snapshot rather than mutating the source.

Pool-member states: warming, ready, acquired, releasing, invalid,
retired, archived (003-workarea-provider.md).`,
		SilenceUsage: true,
	}
	cmd.AddCommand(newWorkareaListCmd(factory))
	cmd.AddCommand(newWorkareaShowCmd(factory))
	cmd.AddCommand(newWorkareaRestoreCmd(factory))
	cmd.AddCommand(newWorkareaDiffCmd(factory))
	return cmd
}

// ── list ──────────────────────────────────────────────────────────────────

func newWorkareaListCmd(factory workareaClientFactory) *cobra.Command {
	var jsonOut bool
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List active pool members and on-disk archives",
		Long: `List workareas from the local daemon — both active pool
members and on-disk archives. Each row carries a ` + "`kind`" + ` field
indicating which side of the boundary it sits on.

Pass --json for machine-readable output.`,
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			client := factory(resolveWorkareaDaemonConfig())
			return runWorkareaList(cmd.OutOrStdout(), client, jsonOut)
		},
	}
	cmd.Flags().BoolVar(&jsonOut, "json", false, "Output as JSON")
	return cmd
}

func runWorkareaList(out io.Writer, client workareaDaemonClient, jsonOut bool) error {
	resp, err := client.ListWorkareas()
	if err != nil {
		return fmt.Errorf("list workareas: %w", err)
	}
	if jsonOut {
		return encodeWorkareaJSON(out, resp)
	}
	noColor := workareaview.NoColorEnv()
	return workareaview.RenderList(out, resp, noColor)
}

// ── show ──────────────────────────────────────────────────────────────────

func newWorkareaShowCmd(factory workareaClientFactory) *cobra.Command {
	var jsonOut bool
	cmd := &cobra.Command{
		Use:   "show <id>",
		Short: "Show full metadata for a single workarea",
		Long: `Show full metadata for a workarea by its id. The id may be
either an active pool member id or an archive id; the response Kind
field disambiguates.

Displays: kind, status, repository, ref, mode, acquire path, path,
clean state checksum, archive location, acquired/released timestamps,
and the resolved toolchain map.`,
		Args:         cobra.ExactArgs(1),
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			client := factory(resolveWorkareaDaemonConfig())
			return runWorkareaShow(cmd.OutOrStdout(), client, args[0], jsonOut)
		},
	}
	cmd.Flags().BoolVar(&jsonOut, "json", false, "Output as JSON")
	return cmd
}

func runWorkareaShow(out io.Writer, client workareaDaemonClient, id string, jsonOut bool) error {
	env, err := client.GetWorkarea(id)
	if err != nil {
		if errors.Is(err, afclient.ErrNotFound) {
			return fmt.Errorf("workarea not found: %s", id)
		}
		return fmt.Errorf("get workarea: %w", err)
	}
	if jsonOut {
		return encodeWorkareaJSON(out, &env.Workarea)
	}
	noColor := workareaview.NoColorEnv()
	return workareaview.RenderInspect(out, &env.Workarea, noColor)
}

// ── restore ───────────────────────────────────────────────────────────────

func newWorkareaRestoreCmd(factory workareaClientFactory) *cobra.Command {
	var (
		reason        string
		intoSessionID string
		jsonOut       bool
	)
	cmd := &cobra.Command{
		Use:   "restore <archive-id>",
		Short: "Materialise an archive into a new active workarea",
		Long: `Restore an archived workarea into a fresh active pool
member. The new workarea has a NEW id distinct from the archive id —
archives are immutable per ADR D4a.

--reason          optional human-readable justification recorded in the
                  daemon's audit log alongside the restore.
--into-session-id pin the restored workarea to a specific session id;
                  the daemon returns 409 if the id is already in use.

Pool saturation returns 503 + Retry-After.`,
		Args:         cobra.ExactArgs(1),
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			client := factory(resolveWorkareaDaemonConfig())
			return runWorkareaRestore(cmd.OutOrStdout(), client, args[0],
				afclient.WorkareaRestoreRequest{
					Reason:        reason,
					IntoSessionID: intoSessionID,
				}, jsonOut)
		},
	}
	cmd.Flags().StringVar(&reason, "reason", "", "Human-readable restore justification (audit log)")
	cmd.Flags().StringVar(&intoSessionID, "into-session-id", "", "Pin the restored workarea to this session id")
	cmd.Flags().BoolVar(&jsonOut, "json", false, "Output as JSON")
	return cmd
}

func runWorkareaRestore(out io.Writer, client workareaDaemonClient, archiveID string,
	req afclient.WorkareaRestoreRequest, jsonOut bool,
) error {
	res, err := client.RestoreWorkarea(archiveID, req)
	if err != nil {
		switch {
		case errors.Is(err, afclient.ErrNotFound):
			return fmt.Errorf("archive not found: %s", archiveID)
		case errors.Is(err, afclient.ErrConflict):
			return fmt.Errorf("session id already in use: %s", req.IntoSessionID)
		case errors.Is(err, afclient.ErrUnavailable):
			return fmt.Errorf("pool saturated; retry later: %w", err)
		case errors.Is(err, afclient.ErrBadRequest):
			return fmt.Errorf("archive corrupted or unreadable: %w", err)
		}
		return fmt.Errorf("restore workarea: %w", err)
	}
	if jsonOut {
		return encodeWorkareaJSON(out, res)
	}
	noColor := workareaview.NoColorEnv()
	return workareaview.RenderRestore(out, res, noColor)
}

// ── diff ──────────────────────────────────────────────────────────────────

func newWorkareaDiffCmd(factory workareaClientFactory) *cobra.Command {
	var jsonOut bool
	cmd := &cobra.Command{
		Use:   "diff <archive-a> <archive-b>",
		Short: "Diff two archived workareas",
		Long: `Diff the filesystem state of two archived workareas. Both
ids MUST resolve to archives — diffing live members is out of scope
because they mutate during the diff and produce torn reads.

Output shows per-path entries with status (added / removed /
modified), file sizes, modes, and SHA-256 hashes. Pass --json for
machine-readable output.`,
		Args:         cobra.ExactArgs(2),
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			client := factory(resolveWorkareaDaemonConfig())
			return runWorkareaDiff(cmd.OutOrStdout(), client, args[0], args[1], jsonOut)
		},
	}
	cmd.Flags().BoolVar(&jsonOut, "json", false, "Output as JSON")
	return cmd
}

func runWorkareaDiff(out io.Writer, client workareaDaemonClient, idA, idB string, jsonOut bool) error {
	diff, err := client.DiffWorkareas(idA, idB)
	if err != nil {
		if errors.Is(err, afclient.ErrNotFound) {
			return fmt.Errorf("one or both archives not found: %s, %s", idA, idB)
		}
		return fmt.Errorf("diff workareas: %w", err)
	}
	if jsonOut {
		return encodeWorkareaJSON(out, diff)
	}
	noColor := workareaview.NoColorEnv()
	return workareaview.RenderDiff(out, diff, noColor)
}

// ── helpers ────────────────────────────────────────────────────────────────

// encodeWorkareaJSON writes v as pretty-printed JSON to out.
func encodeWorkareaJSON(out io.Writer, v any) error {
	enc := json.NewEncoder(out)
	enc.SetIndent("", "  ")
	if err := enc.Encode(v); err != nil {
		return fmt.Errorf("encode JSON: %w", err)
	}
	return nil
}
