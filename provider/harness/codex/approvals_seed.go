package codex

import (
	"fmt"
	"strings"
)

// Approval seeding for the interactive (PTY) spawn mode — the second half of
// the "start unattended" story trust.go began.
//
// # The failure this exists to prevent
//
// trust.go removed the two MODAL STARTUP reviews that block a dispatched
// session before it reads its prompt. Getting past them is necessary but not
// sufficient: once the session is running, the codex CLI raises three further
// classes of blocking review, each of which stops an unattended session dead in
// exactly the same way — a modal nobody will answer, no timeout, no output that
// explains it.
//
//  1. COMMAND approval. With codex's default approval policy the model asks
//     before running a command the policy does not consider trusted:
//     "Would you like to run the following command?".
//  2. ESCALATED command approval. This one survives turning the policy off in
//     the UI, which is what makes it the expensive one to diagnose: the
//     sandbox, not the approval policy, is what stops a command that touches
//     the network or writes outside the workspace, and the escalation request
//     it produces is its own review. A session set to "full access" from inside
//     the TUI still stopped on a plain git command, because the SANDBOX was
//     still `workspace-write`.
//  3. MCP per-tool-call approval. codex asks once per distinct TOOL NAME —
//     "Allow the <server> MCP server to run tool "<tool>"?", offering Allow /
//     Allow for this session / Always allow — so a session driving a
//     platform-configured MCP server pays one modal per tool it reaches for.
//
// All three are removed here by CONFIGURING the answers before the child
// starts, the same way and for the same reason trust.go does.
//
// # Why the platform is entitled to answer these
//
// Same rule as trust.go — provenance, not convenience. Every input a dispatched
// interactive session acts on is platform-provisioned: the workspace was
// created by the runner, the credentials were minted and scoped server-side,
// and the MCP servers reaching the session are the ones the platform itself
// configured. An approval gate inside codex re-asks a question the control
// plane has already answered, to a terminal with nobody in front of it. It buys
// no security in that setting and costs the session its ability to finish.
//
// The posture is therefore explicit rather than implied: codex's own approval
// policy is turned off and its sandbox is set to full access, which is the
// state codex's TUI labels "YOLO mode". Isolation for a dispatched session is
// the job of the environment the platform provisioned around it — a container,
// a VM, a throwaway workspace — not of a second sandbox inside the CLI that
// only knows how to ask a human.
//
// # Config keys, not flags
//
// codex-cli 0.146 has `--yolo` and `--dangerously-bypass-approvals-and-sandbox`
// flags that reach the same state. This leg seeds the two CONFIG KEYS instead,
// for the reason trust.go states about `--disable`: a flag that is renamed or
// removed by a codex release turns into a harness that cannot spawn at all,
// while `approval_policy` / `sandbox_mode` are the settings those flags
// themselves write, are validated by `--strict-config`, and are visible in argv
// as the decision they are. Every value below was read back from codex 0.146's
// own config deserializer, which names the accepted variants when it rejects
// one.
//
// # Scope
//
// Interactive spawn mode only, exactly like trust.go. The headless app-server
// lane computes its approval decisions on the JSON-RPC approval bridge
// (approval.go) where nothing blocks on a keystroke, and it stays untouched.
const (
	// codexApprovalPolicyNever is codex's `approval_policy` variant that stops
	// it asking before it runs a command. Accepted variants in 0.146:
	// untrusted, on-failure, on-request, granular, never.
	codexApprovalPolicyNever = "never"

	// codexSandboxFullAccess is codex's `sandbox_mode` variant that stops the
	// sandbox raising an escalation review for a command that touches the
	// network or writes outside the workspace. Accepted variants in 0.146:
	// read-only, workspace-write, danger-full-access. Turning the approval
	// policy off WITHOUT this leaves that class of review in place — that is
	// the observation that produced this file.
	codexSandboxFullAccess = "danger-full-access"

	// codexMCPToolsApprovalApprove is codex's `default_tools_approval_mode`
	// variant that runs an MCP server's tools without a per-tool-call review.
	// Accepted variants in 0.146: auto, prompt, writes, approve — and the
	// intuitive-looking one is the wrong one: a session configured with `auto`
	// still raised the per-tool review, `approve` is what pre-answers it. The
	// key is set per SERVER (mcp_servers.<name>.default_tools_approval_mode),
	// so it reaches only the servers this spawn itself requested and never a
	// server the operator's ambient configuration also defines.
	codexMCPToolsApprovalApprove = "approve"

	// codexApprovalsEnv selects the approval posture for the session. It
	// exists for the attended case only; see codexApprovalsPolicy.
	codexApprovalsEnv = "DONMAI_CODEX_APPROVALS"

	// codexApprovalsOff is the default: codex's approval gates are configured
	// off, so a dispatched session runs commands and MCP tools without asking.
	codexApprovalsOff = "off"

	// codexApprovalsInherit leaves codex's own approval handling alone. An
	// attended terminal can then review each command and tool call — and an
	// UNATTENDED session started this way blocks on the first one, which is
	// exactly the hang this file otherwise prevents.
	codexApprovalsInherit = "inherit"
)

// codexApprovalsPolicy resolves the approval posture for one spawn. getenv is
// injected so the resolution is testable without mutating process state.
//
// An unrecognized value is an ERROR rather than a silent fall back to the
// default, for the same reason codexHooksPolicy treats one that way: the knob
// chooses between "cannot hang" and "may hang", and a typo that silently picked
// either would be indefensible in the direction it happened to pick.
func codexApprovalsPolicy(getenv func(string) string) (string, error) {
	raw := ""
	if getenv != nil {
		raw = strings.TrimSpace(getenv(codexApprovalsEnv))
	}
	switch strings.ToLower(raw) {
	case "":
		return codexApprovalsOff, nil
	case codexApprovalsOff:
		return codexApprovalsOff, nil
	case codexApprovalsInherit:
		return codexApprovalsInherit, nil
	default:
		return "", fmt.Errorf("%s=%q is not a recognized approvals policy (want %q or %q)",
			codexApprovalsEnv, raw, codexApprovalsOff, codexApprovalsInherit)
	}
}

// interactiveApprovalArgs builds the codex CLI overrides that keep a running
// interactive session from stopping on a command or escalation review. It
// returns nothing under the inherit policy, leaving codex's own handling in
// place for an attended terminal.
//
// Both keys are required together. `approval_policy` alone leaves the sandbox
// raising escalation reviews for network- and outside-workspace commands, and
// `sandbox_mode` alone leaves the policy asking before ordinary ones.
func interactiveApprovalArgs(approvalsPolicy string) []string {
	if approvalsPolicy != codexApprovalsOff {
		return nil
	}
	return []string{
		"--config", "approval_policy=" + tomlBasicString(codexApprovalPolicyNever),
		"--config", "sandbox_mode=" + tomlBasicString(codexSandboxFullAccess),
	}
}

// mcpToolsApprovalMode returns the per-server `default_tools_approval_mode`
// value to seed for the requested MCP servers, or "" to leave the key unset.
//
// Measured in 0.146, either this key or `approval_policy = never` closes the
// per-tool review on its own. Both are seeded anyway, because they are separate
// settings that codex lets you set separately: the approval policy is a global
// statement about commands that happens to also cover MCP today, while this key
// is the only statement scoped to the servers the platform itself requested. If
// a later codex decouples them, the MCP class stays closed without another
// incident to find it.
func mcpToolsApprovalMode(approvalsPolicy string) string {
	if approvalsPolicy != codexApprovalsOff {
		return ""
	}
	return codexMCPToolsApprovalApprove
}
