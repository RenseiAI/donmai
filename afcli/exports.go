// Package afcli exports.go — public factory functions for the `host`
// parent and for the four daemon-targeted command trees it contains
// (provider, kit, workarea, routing).
//
// Most afcli factories stay unexported and reach root via
// RegisterCommands. These need public factories so a composing
// downstream binary can consume the whole `host` noun — or graft an
// individual family under its own parent — without re-implementing or
// hand-assembling the surface.
//
// The factories return a fresh *cobra.Command tree on each call so
// callers can attach the same logical surface under multiple parents
// (e.g. both at top-level via RegisterCommands and nested under
// `host`) without sharing mutable command state.
//
// All four trees target the local daemon's HTTP control API per
// ADR-2026-05-07-daemon-http-control-api.md. They never hit the SaaS
// platform and never attach an Authorization header (D2 —
// localhost-only). The ds argument is accepted for signature
// consistency with the rest of afcli but is unused — daemon-targeted
// commands resolve their client via DONMAI_DAEMON_URL or the
// 127.0.0.1:7734 default.
package afcli

import (
	"github.com/RenseiAI/donmai/afclient"
	"github.com/spf13/cobra"
)

// NewHostCmd returns a fresh `host` Cobra command tree — the noun for
// *this machine*. It owns the daemon lifecycle (install, uninstall,
// setup, run, status, logs, doctor, pause, resume, update, drain, stop,
// stats, evict, set), this machine's capacity envelope (`host set`,
// `host stats`), its workarea pool (`host workarea`, `host evict`), the
// providers and kits installed on it (`host provider`, `host kit`),
// project admission (`host project`), and the local live-session
// dashboard (`host watch`).
//
// RegisterCommands already attaches this tree; the export exists so a
// composing downstream binary can graft the same surface under its own
// root instead of hand-assembling an equivalent, and so that additions
// here reach that binary without a second edit. cfg supplies BinaryName
// (used in help text and remediation hints) and HostBinaryVersion.
//
// The returned tree is fresh on each call, so the same logical surface
// can hang off more than one parent.
func NewHostCmd(ds func() afclient.DataSource, cfg Config) *cobra.Command {
	return newHostCmd(ds, cfg)
}

// NewProviderCmd returns a fresh `provider` Cobra command tree
// (list, show) targeting the local daemon. See provider.go for
// per-subcommand documentation.
func NewProviderCmd(ds func() afclient.DataSource) *cobra.Command {
	return newProviderCmd(ds)
}

// NewKitCmd returns a fresh `kit` Cobra command tree (list, show,
// install, enable, disable, verify, sources) targeting the local
// daemon. See kit.go for per-subcommand documentation.
func NewKitCmd(ds func() afclient.DataSource) *cobra.Command {
	return newKitCmd(ds)
}

// NewWorkareaCmd returns a fresh `workarea` Cobra command tree
// (list, show, restore, diff) targeting the local daemon. See
// workarea.go for per-subcommand documentation.
func NewWorkareaCmd(ds func() afclient.DataSource) *cobra.Command {
	return newWorkareaCmd(ds)
}

// NewRoutingCmd returns a fresh `routing` Cobra command tree (show,
// explain) targeting the local daemon. See routing.go for
// per-subcommand documentation.
func NewRoutingCmd(ds func() afclient.DataSource) *cobra.Command {
	return newRoutingCmd(ds)
}
