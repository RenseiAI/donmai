// Package provider hosts the canonical renderers for the af /
// rensei `provider` command tree, sourced from afclient.Provider wire
// types. Lifted from the previous rensei-tui resident copy under
// rensei-tui/internal/views/provider per ADR-2026-05-07-daemon-http-
// control-api.md §D3.
//
// Two output paths:
//
//   - RenderList / RenderShow / TrustSymbol — ANSI rendering for TTY
//     users.
//   - PlainList / PlainShow — deterministic plain-text rendering used by
//     rensei-smokes integration tests and by `--plain` mode. No ANSI
//     escapes, no emoji.
//
// The CapabilityChip primitive from tui-components v0.2.0 is not yet
// shipped; capability flags render as plain key=value pairs. Swap in
// when REN-1331 lands.
package provider

import (
	"fmt"
	"io"
	"os"
	"text/tabwriter"

	"github.com/RenseiAI/agentfactory-tui/afclient"
)

// RenderList writes a human-readable table of providers grouped by
// family. Each row shows: NAME, VERSION, SCOPE, STATUS. When noColor
// is true (NO_COLOR=1 or --plain mode), ANSI escapes are suppressed.
func RenderList(out io.Writer, providers []afclient.Provider, noColor bool) error {
	// Group providers by family in afclient.AllProviderFamilies order so
	// the output is deterministic and matches the architecture doc's
	// ordering.
	byFamily := make(map[afclient.ProviderFamily][]afclient.Provider, len(afclient.AllProviderFamilies))
	for _, p := range providers {
		byFamily[p.Family] = append(byFamily[p.Family], p)
	}

	first := true
	for _, family := range afclient.AllProviderFamilies {
		entries := byFamily[family]
		if len(entries) == 0 {
			continue
		}

		if !first {
			_, _ = fmt.Fprintln(out)
		}
		first = false

		_, _ = fmt.Fprintln(out, sectionHeader(string(family), noColor))

		w := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
		_, _ = fmt.Fprintf(w, "  %s\t%s\t%s\t%s\n",
			colHeader("NAME", noColor),
			colHeader("VERSION", noColor),
			colHeader("SCOPE", noColor),
			colHeader("STATUS", noColor),
		)
		for _, p := range entries {
			_, _ = fmt.Fprintf(w, "  %s\t%s\t%s\t%s\n",
				bold(p.Name, noColor),
				p.Version,
				muted(string(p.Scope), noColor),
				statusStr(p.Status, noColor),
			)
		}
		if err := w.Flush(); err != nil {
			return fmt.Errorf("flush tabwriter: %w", err)
		}
	}

	if len(providers) == 0 {
		_, _ = fmt.Fprintln(out, muted("No providers registered.", noColor))
	}

	return nil
}

// PlainList is the smoke-pinning plain-text renderer for a provider
// list. It calls RenderList with noColor=true, which is what
// rensei-smokes integration tests target. Exposed as a separate
// function so the test harness depends on a stable, named symbol
// rather than the noColor flag plumbing.
func PlainList(out io.Writer, providers []afclient.Provider) error {
	return RenderList(out, providers, true)
}

// NoColorEnv returns true when NO_COLOR is set in the environment (per
// https://no-color.org). The caller may OR this with its own plainMode
// flag.
func NoColorEnv() bool {
	return os.Getenv("NO_COLOR") != ""
}

// ---- ANSI helpers -----------------------------------------------------------
// These are intentionally minimal — a real refactor will replace them with
// tui-components theme helpers once REN-1331 ships.

const (
	ansiReset   = "\033[0m"
	ansiBold    = "\033[1m"
	ansiDim     = "\033[2m"
	ansiGreen   = "\033[32m"
	ansiYellow  = "\033[33m"
	ansiRed     = "\033[31m"
	ansiMagenta = "\033[35m"
)

func bold(s string, noColor bool) string {
	if noColor {
		return s
	}
	return ansiBold + s + ansiReset
}

func muted(s string, noColor bool) string {
	if noColor {
		return s
	}
	return ansiDim + s + ansiReset
}

func colHeader(s string, noColor bool) string {
	if noColor {
		return s
	}
	return ansiDim + ansiBold + s + ansiReset
}

func sectionHeader(s string, noColor bool) string {
	if noColor {
		return s
	}
	return ansiMagenta + ansiBold + s + ansiReset
}

func statusStr(status afclient.ProviderStatus, noColor bool) string {
	if noColor {
		return string(status)
	}
	switch status {
	case afclient.StatusReady:
		return ansiGreen + string(status) + ansiReset
	case afclient.StatusDegraded:
		return ansiYellow + string(status) + ansiReset
	case afclient.StatusUnhealthy:
		return ansiRed + string(status) + ansiReset
	default:
		return ansiDim + string(status) + ansiReset
	}
}

// TrustSymbol returns the appropriate trust symbol for the given trust
// state. When noColor is true, plain ASCII fallbacks are used instead
// of emoji.
func TrustSymbol(trust afclient.ProviderTrustState, noColor bool) string {
	if noColor {
		switch trust {
		case afclient.TrustSignedVerified:
			return "[verified]"
		case afclient.TrustSignedUnverified:
			return "[signed/unverified]"
		default:
			return "[unsigned]"
		}
	}
	switch trust {
	case afclient.TrustSignedVerified:
		return "✅"
	case afclient.TrustSignedUnverified:
		return "⚠"
	default:
		return "🔓"
	}
}
