package provider

import (
	"fmt"
	"io"
	"sort"

	"github.com/RenseiAI/agentfactory-tui/afclient"
)

// RenderShow writes the detail view for a single provider. Capability
// flags are rendered via plain ANSI key=value pairs. The real
// CapabilityChip primitive from tui-components v0.2.0 is pending
// REN-1331.
func RenderShow(out io.Writer, p *afclient.Provider, noColor bool) error {
	if p == nil {
		return fmt.Errorf("provider is nil")
	}
	label(out, "ID:      ", p.ID, noColor)
	label(out, "Name:    ", p.Name, noColor)
	label(out, "Family:  ", string(p.Family), noColor)
	label(out, "Version: ", p.Version, noColor)
	label(out, "Scope:   ", string(p.Scope), noColor)
	label(out, "Status:  ", statusStr(p.Status, noColor), noColor)
	label(out, "Source:  ", string(p.Source), noColor)

	// Trust state — symbols per 002 / issue spec.
	trustSym := TrustSymbol(p.Trust, noColor)
	label(out, "Trust:   ", fmt.Sprintf("%s %s", trustSym, trustLabel(p, noColor)), noColor)
	if p.SignerID != "" {
		label(out, "Signer:  ", p.SignerID, noColor)
	}
	if p.SignedAt != "" {
		label(out, "SignedAt:", p.SignedAt, noColor)
	}

	// Capability flags — rendered as indented key=value pairs in
	// deterministic key order so the output is stable across Go map
	// iteration shuffles. The smoke harness pins this rendering.
	if len(p.Capabilities) > 0 {
		fmt.Fprintln(out)
		fmt.Fprintln(out, bold("Capabilities:", noColor))
		keys := make([]string, 0, len(p.Capabilities))
		for k := range p.Capabilities {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			fmt.Fprintf(out, "  %s%s\n",
				muted(k+": ", noColor),
				bold(fmt.Sprintf("%v", p.Capabilities[k]), noColor),
			)
		}
	}

	return nil
}

// PlainShow is the smoke-pinning plain-text renderer for a single
// provider. It calls RenderShow with noColor=true.
func PlainShow(out io.Writer, p *afclient.Provider) error {
	return RenderShow(out, p, true)
}

func label(out io.Writer, key, value string, noColor bool) {
	fmt.Fprintf(out, "%s %s\n", muted(key, noColor), bold(value, noColor))
}

func trustLabel(p *afclient.Provider, noColor bool) string {
	switch p.Trust {
	case afclient.TrustSignedVerified:
		if noColor {
			return "signed and verified"
		}
		return ansiGreen + "signed and verified" + ansiReset
	case afclient.TrustSignedUnverified:
		if noColor {
			return "signed but unverified"
		}
		return ansiYellow + "signed but unverified" + ansiReset
	default:
		if noColor {
			return "unsigned"
		}
		return ansiRed + "unsigned" + ansiReset
	}
}
