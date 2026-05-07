// Package afcli exports.go — public factory functions for the four
// daemon-targeted command trees (provider, kit, workarea, routing).
//
// Most afcli factories stay unexported and reach root via
// RegisterCommands. The four daemon-targeted families need public
// factories so downstream binaries can graft them under their own
// parent commands (e.g. `rensei host provider`, `rensei host kit`,
// `rensei host workarea`) without re-implementing the surface.
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
// commands resolve their client via RENSEI_DAEMON_URL or the
// 127.0.0.1:7734 default.
package afcli

import (
	"github.com/RenseiAI/agentfactory-tui/afclient"
	"github.com/spf13/cobra"
)

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
