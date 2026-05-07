// Package kit hosts the canonical renderers for the af / rensei `kit`
// command tree, sourced from afclient.Kit / KitManifest /
// KitRegistrySource wire types.
//
// Lifted from the previous rensei-tui resident copy under
// rensei-tui/internal/views/kit per
// ADR-2026-05-07-daemon-http-control-api.md §D3 (Wave 9 A2).
//
// Two output paths:
//
//   - RenderList / RenderShow / RenderInstall / RenderToggle /
//     RenderVerifySignature / RenderSources — ANSI rendering for TTY users.
//   - PlainList / PlainShow / PlainSources — deterministic plain-text
//     rendering used by rensei-smokes integration tests and by `--plain`
//     mode. No ANSI escapes, no emoji.
//
// The KitDetectResult, AttestationChip, and ScopePill primitives from
// tui-components v0.2.0 are not yet shipped; trust state and detect
// rules render as plain text. Swap in when REN-1331 lands.
package kit

import (
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"text/tabwriter"

	"github.com/RenseiAI/agentfactory-tui/afclient"
)

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
	ansiCyan    = "\033[36m"
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

func colored(s, color string, noColor bool) string {
	if noColor {
		return s
	}
	return color + s + ansiReset
}

// statusStr renders a KitStatus with colour coding.
func statusStr(status afclient.KitStatus, noColor bool) string {
	if noColor {
		return string(status)
	}
	switch status {
	case afclient.KitStatusActive:
		return ansiGreen + string(status) + ansiReset
	case afclient.KitStatusDisabled:
		return ansiDim + string(status) + ansiReset
	case afclient.KitStatusError:
		return ansiRed + string(status) + ansiReset
	default:
		return ansiDim + string(status) + ansiReset
	}
}

// TrustSymbol returns the trust badge for a kit trust state.
// When noColor is true, plain ASCII fallbacks are used instead of emoji.
func TrustSymbol(trust afclient.KitTrustState, noColor bool) string {
	if noColor {
		switch trust {
		case afclient.KitTrustSignedVerified:
			return "[verified]"
		case afclient.KitTrustSignedUnverified:
			return "[signed/unverified]"
		default:
			return "[unsigned]"
		}
	}
	switch trust {
	case afclient.KitTrustSignedVerified:
		return "✅"
	case afclient.KitTrustSignedUnverified:
		return "⚠"
	default:
		return "🔓"
	}
}

func trustLabel(trust afclient.KitTrustState, noColor bool) string {
	switch trust {
	case afclient.KitTrustSignedVerified:
		return colored("signed and verified", ansiGreen, noColor)
	case afclient.KitTrustSignedUnverified:
		return colored("signed but unverified", ansiYellow, noColor)
	default:
		return colored("unsigned", ansiRed, noColor)
	}
}

func label(out io.Writer, key, value string, noColor bool) {
	_, _ = fmt.Fprintf(out, "%s %s\n", muted(key, noColor), bold(value, noColor))
}

// ---- RenderList -------------------------------------------------------------

// RenderList writes a human-readable table of installed kits.
// Each row shows: ID, VERSION, SCOPE, STATUS, SOURCE.
// When noColor is true (NO_COLOR=1 or plainMode), ANSI escapes are suppressed.
func RenderList(out io.Writer, kits []afclient.Kit, noColor bool) error {
	if len(kits) == 0 {
		_, err := fmt.Fprintln(out, muted("No kits installed.", noColor))
		return err
	}

	w := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintf(w, "  %s\t%s\t%s\t%s\t%s\n",
		colHeader("ID", noColor),
		colHeader("VERSION", noColor),
		colHeader("SCOPE", noColor),
		colHeader("STATUS", noColor),
		colHeader("SOURCE", noColor),
	)
	for _, k := range kits {
		_, _ = fmt.Fprintf(w, "  %s\t%s\t%s\t%s\t%s\n",
			bold(k.ID, noColor),
			k.Version,
			muted(string(k.Scope), noColor),
			statusStr(k.Status, noColor),
			muted(string(k.Source), noColor),
		)
	}
	if err := w.Flush(); err != nil {
		return fmt.Errorf("flush tabwriter: %w", err)
	}
	return nil
}

// PlainList is the smoke-pinning plain-text renderer for a kit list.
// Calls RenderList with noColor=true; rensei-smokes integration tests
// target this stable function name.
func PlainList(out io.Writer, kits []afclient.Kit) error {
	return RenderList(out, kits, true)
}

// ---- RenderShow -------------------------------------------------------------

// RenderShow writes the detail view for a single kit manifest.
func RenderShow(out io.Writer, m *afclient.KitManifest, noColor bool) error {
	label(out, "ID:          ", m.ID, noColor)
	label(out, "Name:        ", m.Name, noColor)
	if m.Description != "" {
		label(out, "Description: ", m.Description, noColor)
	}
	label(out, "Version:     ", m.Version, noColor)
	label(out, "Scope:       ", string(m.Scope), noColor)
	label(out, "Status:      ", statusStr(m.Status, noColor), noColor)
	label(out, "Source:      ", string(m.Source), noColor)
	if m.Author != "" {
		label(out, "Author:      ", m.Author, noColor)
	}
	if m.AuthorID != "" {
		label(out, "AuthorID:    ", m.AuthorID, noColor)
	}
	if m.License != "" {
		label(out, "License:     ", m.License, noColor)
	}
	if m.Homepage != "" {
		label(out, "Homepage:    ", m.Homepage, noColor)
	}

	// Trust state
	trustSym := TrustSymbol(m.Trust, noColor)
	label(out, "Trust:       ", fmt.Sprintf("%s %s", trustSym, trustLabel(m.Trust, noColor)), noColor)
	if m.SignerID != "" {
		label(out, "Signer:      ", m.SignerID, noColor)
	}
	if m.SignedAt != "" {
		label(out, "SignedAt:    ", m.SignedAt, noColor)
	}

	// Detect rules
	if len(m.DetectFiles) > 0 || m.DetectExec != "" || len(m.DetectToolchain) > 0 {
		_, _ = fmt.Fprintln(out)
		_, _ = fmt.Fprintln(out, sectionHeader("Detect", noColor))
		if len(m.DetectFiles) > 0 {
			label(out, "  Files:  ", strings.Join(m.DetectFiles, ", "), noColor)
		}
		if m.DetectExec != "" {
			label(out, "  Exec:   ", m.DetectExec, noColor)
		}
		if len(m.DetectToolchain) > 0 {
			_, _ = fmt.Fprintln(out, muted("  Toolchain:", noColor))
			// Sort keys for deterministic output (map iteration order is
			// random; smoke tests pin the stable order).
			keys := make([]string, 0, len(m.DetectToolchain))
			for k := range m.DetectToolchain {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			for _, k := range keys {
				_, _ = fmt.Fprintf(out, "    %s%s\n",
					muted(k+": ", noColor),
					bold(m.DetectToolchain[k], noColor),
				)
			}
		}
	}

	// Commands
	if len(m.Commands) > 0 {
		_, _ = fmt.Fprintln(out)
		_, _ = fmt.Fprintln(out, sectionHeader("Commands", noColor))
		keys := make([]string, 0, len(m.Commands))
		for k := range m.Commands {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			_, _ = fmt.Fprintf(out, "  %s%s\n",
				muted(k+": ", noColor),
				bold(m.Commands[k], noColor),
			)
		}
	}

	// Provide summary
	provides := buildProvidesSummary(m, noColor)
	if len(provides) > 0 {
		_, _ = fmt.Fprintln(out)
		_, _ = fmt.Fprintln(out, sectionHeader("Provides", noColor))
		for _, line := range provides {
			_, _ = fmt.Fprintln(out, "  "+line)
		}
	}

	// Composition
	if len(m.ConflictsWith) > 0 || len(m.ComposesWith) > 0 || m.Order != "" {
		_, _ = fmt.Fprintln(out)
		_, _ = fmt.Fprintln(out, sectionHeader("Composition", noColor))
		if m.Order != "" {
			label(out, "  Order:          ", m.Order, noColor)
		}
		if len(m.ComposesWith) > 0 {
			label(out, "  Composes with:  ", strings.Join(m.ComposesWith, ", "), noColor)
		}
		if len(m.ConflictsWith) > 0 {
			_, _ = fmt.Fprintf(out, "  %s %s\n",
				muted("Conflicts with:", noColor),
				colored(strings.Join(m.ConflictsWith, ", "), ansiYellow, noColor),
			)
		}
	}

	return nil
}

// PlainShow is the smoke-pinning plain-text renderer for a kit manifest.
func PlainShow(out io.Writer, m *afclient.KitManifest) error {
	return RenderShow(out, m, true)
}

func buildProvidesSummary(m *afclient.KitManifest, noColor bool) []string {
	var lines []string
	if m.ProvidesCommands {
		lines = append(lines, colored("commands", ansiCyan, noColor))
	}
	if m.ProvidesPrompts {
		lines = append(lines, colored("prompt fragments", ansiCyan, noColor))
	}
	if m.ProvidesTools {
		lines = append(lines, colored("tool permissions", ansiCyan, noColor))
	}
	if m.ProvidesMCPServers {
		s := colored("MCP servers", ansiCyan, noColor)
		if len(m.MCPServerNames) > 0 {
			s += muted(" ("+strings.Join(m.MCPServerNames, ", ")+")", noColor)
		}
		lines = append(lines, s)
	}
	if m.ProvidesSkills {
		s := colored("skills", ansiCyan, noColor)
		if len(m.SkillFiles) > 0 {
			s += muted(" ("+strings.Join(m.SkillFiles, ", ")+")", noColor)
		}
		lines = append(lines, s)
	}
	if m.ProvidesAgents {
		s := colored("agents", ansiCyan, noColor)
		if len(m.AgentIDs) > 0 {
			s += muted(" ("+strings.Join(m.AgentIDs, ", ")+")", noColor)
		}
		lines = append(lines, s)
	}
	if m.ProvidesA2ASkills {
		s := colored("A2A skills", ansiCyan, noColor)
		if len(m.A2ASkillIDs) > 0 {
			s += muted(" ("+strings.Join(m.A2ASkillIDs, ", ")+")", noColor)
		}
		lines = append(lines, s)
	}
	if m.ProvidesExtractors {
		s := colored("intelligence extractors", ansiCyan, noColor)
		if len(m.ExtractorNames) > 0 {
			s += muted(" ("+strings.Join(m.ExtractorNames, ", ")+")", noColor)
		}
		lines = append(lines, s)
	}
	return lines
}

// ---- RenderInstall ----------------------------------------------------------

// RenderInstall writes the result of a kit installation.
func RenderInstall(out io.Writer, res *afclient.KitInstallResult, noColor bool) error {
	sym := colored("✓", ansiGreen, noColor)
	if noColor {
		sym = "OK"
	}
	msg := res.Message
	if msg == "" {
		msg = fmt.Sprintf("kit %s@%s installed", res.Kit.ID, res.Kit.Version)
	}
	if _, err := fmt.Fprintf(out, "%s %s\n", sym, bold(msg, noColor)); err != nil {
		return fmt.Errorf("write install header: %w", err)
	}
	label(out, "Source:", string(res.Kit.Source), noColor)
	label(out, "Status:", statusStr(res.Kit.Status, noColor), noColor)
	return nil
}

// ---- RenderToggle -----------------------------------------------------------

// RenderToggle writes the result of a kit enable or disable operation.
func RenderToggle(out io.Writer, k *afclient.Kit, enabled bool, noColor bool) error {
	sym := colored("✓", ansiGreen, noColor)
	if noColor {
		sym = "OK"
	}
	action := "disabled"
	if enabled {
		action = "enabled"
	}
	if _, err := fmt.Fprintf(out, "%s %s\n", sym, bold(fmt.Sprintf("kit %s %s", k.ID, action), noColor)); err != nil {
		return fmt.Errorf("write toggle header: %w", err)
	}
	label(out, "Status:", statusStr(k.Status, noColor), noColor)
	return nil
}

// ---- RenderVerifySignature --------------------------------------------------

// RenderVerifySignature writes the result of a kit signature verification.
func RenderVerifySignature(out io.Writer, res *afclient.KitSignatureResult, noColor bool) error {
	sym := TrustSymbol(res.Trust, noColor)
	label(out, "Kit:    ", res.KitID, noColor)
	label(out, "Trust:  ", fmt.Sprintf("%s %s", sym, trustLabel(res.Trust, noColor)), noColor)
	if res.SignerID != "" {
		label(out, "Signer: ", res.SignerID, noColor)
	}
	if res.SignedAt != "" {
		label(out, "Signed: ", res.SignedAt, noColor)
	}
	if res.Details != "" {
		_, _ = fmt.Fprintln(out)
		_, _ = fmt.Fprintln(out, muted(res.Details, noColor))
	}
	return nil
}

// ---- RenderSources ----------------------------------------------------------

// RenderSources writes the list of configured kit registry sources.
func RenderSources(out io.Writer, sources []afclient.KitRegistrySource, noColor bool) error {
	if len(sources) == 0 {
		_, err := fmt.Fprintln(out, muted("No kit sources configured.", noColor))
		return err
	}

	w := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintf(w, "  %s\t%s\t%s\t%s\n",
		colHeader("NAME", noColor),
		colHeader("KIND", noColor),
		colHeader("STATUS", noColor),
		colHeader("URL", noColor),
	)
	for _, s := range sources {
		var statusFmt string
		if s.Enabled {
			statusFmt = colored("enabled", ansiGreen, noColor)
		} else {
			statusFmt = muted("disabled", noColor)
		}
		_, _ = fmt.Fprintf(w, "  %s\t%s\t%s\t%s\n",
			bold(s.Name, noColor),
			muted(s.Kind, noColor),
			statusFmt,
			muted(s.URL, noColor),
		)
	}
	if err := w.Flush(); err != nil {
		return fmt.Errorf("flush tabwriter: %w", err)
	}
	return nil
}

// PlainSources is the smoke-pinning plain-text renderer for the
// registry-source list.
func PlainSources(out io.Writer, sources []afclient.KitRegistrySource) error {
	return RenderSources(out, sources, true)
}
