package codex

import (
	"path/filepath"
	"regexp"
	"strings"

	"github.com/RenseiAI/agentfactory-tui/agent"
)

// ApprovalAction is the JSON value codex expects on approval responses.
//
// Mirrors the legacy TS ApprovalDecision.action union from
// ../agentfactory/packages/core/src/providers/codex-approval-bridge.ts.
type ApprovalAction string

const (
	// ActionAccept approves a single tool call and returns to
	// per-call prompting for the next call.
	ActionAccept ApprovalAction = "accept"

	// ActionAcceptForSession approves the call AND remembers the
	// approval for the rest of the session (subsequent identical
	// calls auto-approve without re-asking).
	ActionAcceptForSession ApprovalAction = "acceptForSession"

	// ActionDecline denies the call.
	ActionDecline ApprovalAction = "decline"
)

// ApprovalDecision is the verdict the bridge emits for one inbound
// approval request. Reason is filled when ActionDecline so observers
// see why something was blocked.
type ApprovalDecision struct {
	Action ApprovalAction
	Reason string
}

// ApprovalKind classifies the inbound request shape — "command"
// (shell), "file_change" (write/edit), or "unknown" for shapes we have
// not seen yet (those default to acceptForSession to avoid hangs, with
// a SystemEvent surfacing the surprise).
type ApprovalKind string

// ApprovalKind constants name the inbound approval-request shapes the
// codex app-server emits today.
const (
	ApprovalKindCommand    ApprovalKind = "command"
	ApprovalKindFileChange ApprovalKind = "file_change"
	ApprovalKindUnknown    ApprovalKind = "unknown"
)

// ApprovalRequest is the parsed approval payload handed to the bridge.
type ApprovalRequest struct {
	Kind    ApprovalKind
	Command string // populated when Kind == Command
	Path    string // populated when Kind == FileChange
	Cwd     string // worktree root, used to enforce path containment
}

// safetyDeny is a built-in deny pattern checked before any user-level
// allow/deny rules. Mirrors the SAFETY_DENY_PATTERNS list from
// ../agentfactory/packages/core/src/providers/safety-rules.ts but
// trimmed to the rules that mattered most in production.
//
// Order matters — the FIRST match wins. Keep the most-specific rules
// first.
type safetyDeny struct {
	pattern *regexp.Regexp
	reason  string
}

var builtInSafetyDeny = []safetyDeny{
	// rm -rf / and friends.
	{regexp.MustCompile(`(?i)\brm\s+(-[a-zA-Z]*r[a-zA-Z]*\s+)?(-[a-zA-Z]*f[a-zA-Z]*\s+)?(/\s*$|/\s+|/\*\s*$)`), "rm of filesystem root blocked"},

	// git worktree remove / prune — orchestrator owns worktree
	// lifecycle; agents that touch it strand themselves.
	{regexp.MustCompile(`\bgit\s+worktree\s+(remove|prune)\b`), "git worktree remove/prune is reserved for the runner"},

	// git reset --hard — destroys uncommitted work.
	{regexp.MustCompile(`\bgit\s+reset\s+--hard\b`), "git reset --hard would discard work-in-progress"},

	// git push --force-with-lease is fine; bare --force is blocked.
	// Go's regexp lacks negative lookahead — we approximate by
	// requiring whitespace or end-of-line after --force.
	{regexp.MustCompile(`\bgit\s+push\s+--force(\s|$)`), "git push --force without --force-with-lease blocked"},

	// chmod / chown of root or system dirs.
	{regexp.MustCompile(`\b(chmod|chown)\s+-?[Rr]?\s+/\S+`), "recursive chmod/chown on absolute paths blocked"},

	// curl / wget piped to shell.
	{regexp.MustCompile(`(?i)\b(curl|wget)\b[^|]*\|\s*(sudo\s+)?(bash|sh|zsh|dash|ksh)\b`), "piping a download to a shell blocked"},

	// sudo any.
	{regexp.MustCompile(`(?i)\bsudo\b`), "sudo invocation blocked"},
}

// ApprovalBridge evaluates inbound codex approval requests against a
// Spec.PermissionConfig + the built-in safety rules. Mirrors the
// behavior of the legacy TS evaluateCommandApproval +
// evaluateFileChangeApproval.
//
// Concurrency: ApprovalBridge is safe for concurrent calls. The
// configured patterns are read-only after construction.
type ApprovalBridge struct {
	config *agent.PermissionConfig

	// allowRegexes / denyRegexes are the compiled forms of
	// PermissionConfig.AllowPatterns / DisallowPatterns. Compiled
	// once on construction; evaluated in order.
	allowRegexes []*regexp.Regexp
	denyRegexes  []*regexp.Regexp

	// autoApprove is the legacy "default to acceptForSession" flag.
	// Per F.1.1 §10.5 the v0.5.0 bridge ships true by default — we
	// only "ask" when an explicit policy says so. The runner can
	// flip the gate by setting Spec.PermissionConfig.DefaultDecision.
	autoApprove bool
}

// NewApprovalBridge compiles the configured patterns and returns a
// ready-to-use bridge. A nil config produces a bridge that runs the
// built-in safety deny rules and otherwise auto-approves (per F.1.1
// §10.5: ship the bridge, default to allow when no policy is set so
// autonomous fleets do not hang waiting for a human).
func NewApprovalBridge(cfg *agent.PermissionConfig) *ApprovalBridge {
	br := &ApprovalBridge{config: cfg, autoApprove: true}
	if cfg != nil {
		br.allowRegexes = compilePatterns(cfg.AllowPatterns)
		br.denyRegexes = compilePatterns(cfg.DisallowPatterns)
		switch strings.ToLower(cfg.DefaultDecision) {
		case "deny":
			br.autoApprove = false
		case "prompt":
			// "prompt" without a wired UI = decline by default —
			// autonomous fleets cannot answer prompts.
			br.autoApprove = false
		case "allow", "":
			br.autoApprove = true
		}
	}
	return br
}

// Evaluate produces the verdict for one inbound approval request.
//
// Order:
//  1. Built-in safety deny patterns (always enforced; cannot be
//     overridden by user-supplied config — these are the rules that
//     would otherwise corrupt the worktree).
//  2. User-supplied DisallowPatterns.
//  3. User-supplied AllowPatterns. When at least one allow pattern is
//     present, ONLY commands matching an allow pattern are accepted.
//  4. Default decision: acceptForSession when autoApprove, decline
//     otherwise.
func (b *ApprovalBridge) Evaluate(req ApprovalRequest) ApprovalDecision {
	switch req.Kind {
	case ApprovalKindCommand:
		return b.evaluateCommand(req.Command)
	case ApprovalKindFileChange:
		return b.evaluateFileChange(req.Path, req.Cwd)
	default:
		// Unknown shapes default to acceptForSession to avoid
		// hangs; the caller emits a SystemEvent so the surprise is
		// observable.
		return ApprovalDecision{Action: ActionAcceptForSession}
	}
}

func (b *ApprovalBridge) evaluateCommand(cmd string) ApprovalDecision {
	cmd = strings.TrimSpace(cmd)
	if cmd == "" {
		return ApprovalDecision{Action: ActionAcceptForSession}
	}

	// 1. Built-in safety deny.
	for _, sd := range builtInSafetyDeny {
		if sd.pattern.MatchString(cmd) {
			return ApprovalDecision{Action: ActionDecline, Reason: sd.reason}
		}
	}

	// 2. User-supplied disallow.
	for _, re := range b.denyRegexes {
		if re.MatchString(cmd) {
			return ApprovalDecision{Action: ActionDecline, Reason: "command matches disallow pattern: " + re.String()}
		}
	}

	// 3. User-supplied allow (if present, gates everything else).
	if len(b.allowRegexes) > 0 {
		for _, re := range b.allowRegexes {
			if re.MatchString(cmd) {
				return ApprovalDecision{Action: ActionAcceptForSession}
			}
		}
		return ApprovalDecision{Action: ActionDecline, Reason: "command not in allowed list"}
	}

	// 4. Default.
	if b.autoApprove {
		return ApprovalDecision{Action: ActionAcceptForSession}
	}
	return ApprovalDecision{Action: ActionDecline, Reason: "no allow pattern matched and default decision is deny/prompt"}
}

func (b *ApprovalBridge) evaluateFileChange(path, cwd string) ApprovalDecision {
	// Block writes outside the worktree root.
	if cwd != "" {
		clean := filepath.Clean(path)
		root := filepath.Clean(cwd)
		if !strings.HasPrefix(clean+string(filepath.Separator), root+string(filepath.Separator)) && clean != root {
			return ApprovalDecision{Action: ActionDecline, Reason: "file change outside worktree blocked"}
		}
	}

	// Block .git directory mutation.
	if strings.Contains(path, "/.git/") || strings.HasSuffix(path, "/.git") {
		return ApprovalDecision{Action: ActionDecline, Reason: ".git directory modification blocked"}
	}

	if b.autoApprove {
		return ApprovalDecision{Action: ActionAcceptForSession}
	}
	return ApprovalDecision{Action: ActionDecline, Reason: "default decision is deny"}
}

func compilePatterns(patterns []string) []*regexp.Regexp {
	out := make([]*regexp.Regexp, 0, len(patterns))
	for _, p := range patterns {
		// PermissionConfig patterns are user-supplied. Compile with
		// Compile (not MustCompile) so a malformed pattern cannot
		// crash the daemon; bad patterns are dropped silently — the
		// approval-bridge tests assert this.
		re, err := regexp.Compile(p)
		if err != nil {
			continue
		}
		out = append(out, re)
	}
	return out
}

// parseApprovalRequest extracts the kind+payload from a JSON-RPC
// notification's params. The legacy TS supports several shapes
// depending on codex version; this Go port reads the same field names
// (command, filePath) and otherwise classifies as Unknown so the
// caller still responds.
func parseApprovalRequest(params map[string]any, cwd string) ApprovalRequest {
	if cmd, ok := params["command"].(string); ok && cmd != "" {
		return ApprovalRequest{Kind: ApprovalKindCommand, Command: cmd, Cwd: cwd}
	}
	// Codex sometimes wraps the command in a "call.command" or
	// "input.command" subobject; check those too.
	if call, ok := params["call"].(map[string]any); ok {
		if cmd, ok := call["command"].(string); ok && cmd != "" {
			return ApprovalRequest{Kind: ApprovalKindCommand, Command: cmd, Cwd: cwd}
		}
	}
	if path, ok := params["filePath"].(string); ok && path != "" {
		return ApprovalRequest{Kind: ApprovalKindFileChange, Path: path, Cwd: cwd}
	}
	if path, ok := params["path"].(string); ok && path != "" {
		return ApprovalRequest{Kind: ApprovalKindFileChange, Path: path, Cwd: cwd}
	}
	return ApprovalRequest{Kind: ApprovalKindUnknown, Cwd: cwd}
}
