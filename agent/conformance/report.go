package conformance

import (
	"fmt"
	"strings"

	"github.com/RenseiAI/donmai/agent"
)

// Claim is a harness claim this suite does NOT verify, named on every report
// so a green run cannot be read as full certification.
type Claim struct {
	// Claim is the unverified claim, in the harness author's terms.
	Claim string `json:"claim"`
	// Row is the harness-addition checklist row that owns it (0 when none).
	Row int `json:"checklistRow"`
	// Why states what this suite cannot see, and where the evidence lives.
	Why string `json:"why"`
}

// UnverifiedClaims returns the claims a manifest makes, or the checklist rows
// a harness is bound by, that this suite has no check for.
//
// This is the deliberate counterweight to a passing report. Every entry is a
// place where a harness could still be lying and this suite would not notice,
// so the reader is told rather than left to assume the absence of a check is
// the absence of a risk. Entries are unconditional where the row is
// unconditional and manifest-conditional where the claim is.
func UnverifiedClaims(m agent.HarnessManifest) []Claim {
	out := []Claim{
		{
			Claim: "binary pin (min / pinned / verified-against)",
			Row:   1,
			Why:   "the suite never learns which harness binary it ran against; pin enforcement is a matrix-metadata and provider-probe concern",
		},
		{
			Claim: "pin-bump protocol",
			Row:   2,
			Why:   "re-running a harness smoke lane against a new pin is a CI gate, not something an in-process suite can observe",
		},
		{
			Claim: "policy injection (allowed/disallowed tools, permission default, MCP whitelist)",
			Row:   3,
			Why:   "proving a policy is ENFORCED needs a permission-denial smoke against the real harness; a Spec field being accepted is not enforcement",
		},
		{
			Claim: "fail-closed trust boundary",
			Row:   4,
			Why:   "the suite cannot revoke a boundary mid-session; fail-closed and bypass-monitor evidence is a smokes-lane capability",
		},
		{
			Claim: "endpoint pin and provider-fallback lockout",
			Row:   5,
			Why:   "proving no fallback outside the resolved cell needs network-level observation of where the harness actually went",
		},
		{
			Claim: "smoke set (spawn, prompt, permission-denial, teardown against the pinned binary)",
			Row:   7,
			Why:   "the smoke lane is the harness's own repo-level gate; this suite is the shared part underneath it",
		},
		{
			Claim: "tier entry / measurement ladder for routable cells",
			Row:   8,
			Why:   "cell tier is a router-side gate over measurements; the tiers this suite awards are inputs to it, never a substitute",
		},
		{
			Claim: "child conformance (native-child identity, event, cancel and terminal mapping)",
			Row:   11,
			Why:   "proving child mapping needs a live child spawn against an independently admitted cell and its own SessionRef; a check that cannot observe a child would be a silent skip, so none is registered",
		},
		{
			Claim: "materialized on-disk artifacts contain no secrets",
			Row:   10,
			Why:   "receipt/secret-free reads the host-compiled authority only; what the adapter writes to config files, environment or argv at materialization time is invisible here",
		},
	}

	if m.Caps.SupportsToolPlugins || m.Caps.AcceptsMcpServerSpec {
		out = append(out, Claim{
			Claim: "tool plugins / MCP server delivery actually reach the agent",
			Row:   12,
			Why:   "the manifest declares MCP or tool-plugin delivery; proving activation needs a tool the agent must call, which is harness-specific and belongs in that harness's smoke lane",
		})
	}
	if m.Caps.EmitsSubagentEvents {
		out = append(out, Claim{
			Claim: "subagent events are emitted",
			Row:   11,
			Why:   "the manifest declares subagent events; no generic prompt can force a subagent spawn, so a check here would fail honest harnesses and pass silent ones",
		})
	}
	if m.Caps.SupportsInteractivePTY {
		out = append(out, Claim{
			Claim: "interactive PTY spawn mode",
			Row:   10,
			Why:   "the manifest declares the PTY spawn mode; PTY evidence is separate from headless evidence and this suite drives the headless path only",
		})
	}
	if m.Caps.SupportsReasoningEffort {
		out = append(out, Claim{
			Claim: "reasoning effort is honored",
			Row:   9,
			Why:   "the manifest declares reasoning effort; that a Spec field was accepted says nothing about whether the harness passed it on",
		})
	}
	return out
}

// Text renders the report for a terminal or a CI log. The format leads with
// the tier verdicts because that is the certification answer, then shows every
// check with its reason, then the unverified claims.
func (r *Report) Text() string {
	var b strings.Builder
	fmt.Fprintf(&b, "harness: %s (contract %s)\n", r.Harness, r.ContractABI)

	b.WriteString("\ntiers\n")
	for _, tr := range r.TierResults {
		verdict := "NOT EARNED"
		if tr.Earned {
			verdict = "EARNED"
		}
		fmt.Fprintf(&b, "  %-20s %s\n", tr.Tier, verdict)
		if tr.Reason != "" {
			fmt.Fprintf(&b, "  %-20s   %s\n", "", tr.Reason)
		}
	}

	b.WriteString("\nchecks\n")
	for _, res := range r.Results {
		label := strings.ToUpper(string(res.Status))
		if res.Status == StatusNotApplicable {
			label = "N/A"
		}
		fmt.Fprintf(&b, "  %-5s %-34s row %-2d\n", label, res.ID, res.Row)
		if res.Reason != "" {
			suffix := ""
			if res.Decider != "" {
				suffix = fmt.Sprintf(" [%s]", res.Decider)
			}
			fmt.Fprintf(&b, "        %s%s\n", res.Reason, suffix)
		}
	}

	if len(r.Unverified) > 0 {
		b.WriteString("\nNOT verified by this suite (a passing report is not full certification)\n")
		for _, claim := range r.Unverified {
			fmt.Fprintf(&b, "  row %-2d %s\n", claim.Row, claim.Claim)
			fmt.Fprintf(&b, "        %s\n", claim.Why)
		}
	}
	return b.String()
}
