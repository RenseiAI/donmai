package codex

import (
	"path/filepath"
	"regexp"
	"strings"

	"github.com/RenseiAI/donmai/agent"
)

// ApprovalAction is the JSON value codex expects on approval responses.
//
// Mirrors the legacy TS ApprovalDecision.action union from
// ../donmai-libraries/packages/core/src/providers/codex-approval-bridge.ts.
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
// not seen yet. Unknown shapes fail closed whenever a policy was supplied.
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
// ../donmai-libraries/packages/core/src/providers/safety-rules.ts but
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
	allowFiles   bool
	denyFiles    bool
	hasAllowlist bool

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
		br.hasAllowlist = len(cfg.AllowPatterns) > 0
		br.allowRegexes, br.allowFiles = compilePatterns(cfg.AllowPatterns, false)
		br.denyRegexes, br.denyFiles = compilePatterns(cfg.DisallowPatterns, true)
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
		if b.config != nil {
			return ApprovalDecision{Action: ActionDecline, Reason: "unknown approval request under an explicit policy"}
		}
		return ApprovalDecision{Action: ActionAcceptForSession}
	}
}

func (b *ApprovalBridge) evaluateCommand(cmd string) ApprovalDecision {
	cmd = strings.TrimSpace(cmd)
	if cmd == "" {
		if b.config != nil {
			return ApprovalDecision{Action: ActionDecline, Reason: "empty command under an explicit policy"}
		}
		return ApprovalDecision{Action: ActionAcceptForSession}
	}

	// 1. Built-in safety deny.
	for _, sd := range builtInSafetyDeny {
		if sd.pattern.MatchString(cmd) {
			return ApprovalDecision{Action: ActionDecline, Reason: sd.reason}
		}
	}

	// 2. User-supplied disallow, against the whole line and against every
	// command it chains or wraps, so "cd x && git push" or "env git push"
	// cannot slip past a "git push" prefix.
	segments := append([]string{cmd}, commandSegments(cmd)...)
	for _, re := range b.denyRegexes {
		for _, segment := range segments {
			if re.MatchString(segment) {
				return ApprovalDecision{Action: ActionDecline, Reason: "command matches disallow pattern: " + re.String()}
			}
		}
	}

	// 3. User-supplied allow (if present, gates everything else). The whole
	// line must match an allow, as it always had to, AND so must every command
	// it chains: a "git" allow must not cover "git status && rm -rf src". The
	// chained commands keep their wrappers, so unwrapping can never widen an
	// allow either.
	// Any explicit allowlist gates commands, even one with no shell entry:
	// a card that allows only Read(...) or Edit authorizes no command.
	if b.hasAllowlist {
		if matchesAny(b.allowRegexes, cmd) {
			allowed := true
			for _, chained := range chainedCommands(cmd) {
				allowed = allowed && matchesAny(b.allowRegexes, chained)
			}
			if allowed {
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
	if b.denyFiles {
		return ApprovalDecision{Action: ActionDecline, Reason: "file changes match the configured disallow policy"}
	}
	if b.allowFiles {
		return ApprovalDecision{Action: ActionAcceptForSession}
	}
	if b.hasAllowlist {
		return ApprovalDecision{Action: ActionDecline, Reason: "file changes are not in the allowed tool list"}
	}

	if b.autoApprove {
		return ApprovalDecision{Action: ActionAcceptForSession}
	}
	return ApprovalDecision{Action: ActionDecline, Reason: "default decision is deny"}
}

// compilePatterns compiles one side of a PermissionConfig. Denies and allows
// differ wherever a reading could widen an allow:
//   - bare "Bash" declines every command, but as an allow it keeps the legacy
//     raw-regex meaning it always had on this bridge;
//   - a path-scoped Read/Edit/Write pattern and any other tool name (Read,
//     WebFetch, mcp__x__y) authorizes no command; as a deny, a tool name keeps
//     its legacy raw-regex meaning, which can only decline more.
func compilePatterns(patterns []string, deny bool) ([]*regexp.Regexp, bool) {
	out := make([]*regexp.Regexp, 0, len(patterns))
	files := false
	for _, p := range patterns {
		p = strings.TrimSpace(p)
		if isFileChangeTool(p) {
			files = true
			continue
		}
		name, _, scoped := splitToolPattern(p)
		if scoped && (name == "Read" || isFileChangeTool(name)) {
			continue
		}
		if commandPattern, ok := bashPermissionPattern(p); ok && (deny || p != "Bash") {
			out = append(out, regexp.MustCompile(commandPattern))
			continue
		}
		if !deny && p != "Bash" && toolNamePattern.MatchString(p) {
			continue
		}
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
	return out, files
}

// toolNamePattern matches a harness tool name, bare or scoped ("Read",
// "WebFetch(domain:x)", "mcp__server__tool"), as opposed to a raw regex.
var toolNamePattern = regexp.MustCompile(`^(?:[A-Z][A-Za-z0-9]*|mcp__[A-Za-z0-9_*-]+)(?:\(.*\))?$`)

func isFileChangeTool(pattern string) bool {
	switch strings.TrimSpace(pattern) {
	case "Edit", "Write", "MultiEdit", "NotebookEdit":
		return true
	default:
		return false
	}
}

// splitToolPattern splits "Name(arg)" into its parts; a bare name is unscoped.
func splitToolPattern(pattern string) (name, arg string, scoped bool) {
	open := strings.IndexByte(pattern, '(')
	if open <= 0 || !strings.HasSuffix(pattern, ")") {
		return pattern, "", false
	}
	return pattern[:open], strings.TrimSpace(pattern[open+1 : len(pattern)-1]), true
}

func bashPermissionPattern(pattern string) (string, bool) {
	pattern = strings.TrimSpace(pattern)
	if pattern == "Bash" {
		return `(?s)^.*$`, true
	}
	if !strings.HasPrefix(pattern, "Bash(") || !strings.HasSuffix(pattern, ")") {
		return "", false
	}
	inner := strings.TrimSpace(strings.TrimSuffix(strings.TrimPrefix(pattern, "Bash("), ")"))
	if inner == "" || inner == "*" {
		return `(?s)^.*$`, true
	}
	if strings.HasSuffix(inner, ":*") {
		prefix := strings.TrimSpace(strings.TrimSuffix(inner, ":*"))
		return `^` + regexp.QuoteMeta(prefix) + `(?:\s|$)`, true
	}
	// "git push*" is a plain string prefix; "pnpm *" ends at a word.
	if prefix, ok := strings.CutSuffix(inner, "*"); ok {
		if trimmed := strings.TrimSpace(prefix); trimmed != prefix {
			return `^` + regexp.QuoteMeta(trimmed) + `(?:\s|$)`, true
		}
		return `^` + regexp.QuoteMeta(prefix), true
	}
	return `^` + regexp.QuoteMeta(inner) + `$`, true
}

// commandWrappers match leading words that run the rest of the line as a
// command: VAR=value, env, command, exec, nohup, time, nice, timeout, xargs,
// eval, and a shell's -c flag, each optionally by absolute path and with flags.
var commandWrappers = regexp.MustCompile(`^(?:(?:[A-Za-z_][A-Za-z0-9_]*=(?:"[^"]*"|'[^']*'|\S*)|(?:\S*/)?(?:env|command|exec|nohup|time|nice|xargs|eval)(?:\s+-\S+(?:\s+\d+)?)*|(?:\S*/)?timeout(?:\s+-\S+)*\s+\S+|(?:\S*/)?(?:ba|z|da|k)?sh(?:\s+-\S+)*\s+-[A-Za-z]*c)\s+)+`)

// chainedCommands splits a shell line into the commands it runs, untouched.
// It splits at unquoted &&, ||, ;, |, &, newlines and parentheses, and at
// $( and backticks even inside double quotes, where a substitution still
// runs. Quoted text is never split, so 'one;two' stays one argument.
func chainedCommands(cmd string) []string {
	var out []string
	var cur strings.Builder
	flush := func() {
		if part := strings.TrimSpace(cur.String()); part != "" {
			out = append(out, part)
		}
		cur.Reset()
	}
	var quote byte
	for i := 0; i < len(cmd); i++ {
		c := cmd[i]
		switch {
		case quote == '\'':
			cur.WriteByte(c)
			if c == '\'' {
				quote = 0
			}
		case c == '\\' && i+1 < len(cmd):
			cur.WriteByte(c)
			cur.WriteByte(cmd[i+1])
			i++
		case c == '`' || (c == '$' && i+1 < len(cmd) && cmd[i+1] == '('):
			flush()
			if c == '$' {
				i++
			}
		case quote == '"':
			cur.WriteByte(c)
			if c == '"' {
				quote = 0
			}
		case c == '\'' || c == '"':
			quote = c
			cur.WriteByte(c)
		case strings.IndexByte(";&|()\n", c) >= 0:
			flush()
		default:
			cur.WriteByte(c)
		}
	}
	flush()
	return out
}

func matchesAny(res []*regexp.Regexp, s string) bool {
	for _, re := range res {
		if re.MatchString(s) {
			return true
		}
	}
	return false
}

// commandSegments returns, for deny matching only, each command a shell line
// chains or wraps: wrappers stripped, a quoted -c/eval program parsed as its
// own shell line, and each command also with runs of whitespace collapsed.
func commandSegments(cmd string) []string {
	return unwrapCommands(cmd, 0)
}

func unwrapCommands(line string, depth int) []string {
	var out []string
	for _, part := range chainedCommands(line) {
		for next := strings.TrimSpace(commandWrappers.ReplaceAllString(part, "")); next != part; next = strings.TrimSpace(commandWrappers.ReplaceAllString(part, "")) {
			part = next
		}
		out = append(out, part)
		if collapsed := strings.Join(strings.Fields(part), " "); collapsed != part {
			out = append(out, collapsed)
		}
		if program, ok := leadingQuotedWord(part); ok && depth < 4 {
			out = append(out, unwrapCommands(program, depth+1)...)
		}
	}
	return out
}

// leadingQuotedWord returns the content of a leading '...' or "..." word, the
// form a shell -c or eval program takes once its wrapper is stripped.
func leadingQuotedWord(s string) (string, bool) {
	if len(s) < 2 || (s[0] != '\'' && s[0] != '"') {
		return "", false
	}
	if s[0] == '\'' {
		end := strings.IndexByte(s[1:], '\'')
		if end < 0 {
			return "", false
		}
		return s[1 : 1+end], true
	}
	var b strings.Builder
	for i := 1; i < len(s); i++ {
		switch {
		case s[i] == '\\' && i+1 < len(s) && strings.IndexByte("\"\\$`", s[i+1]) >= 0:
			b.WriteByte(s[i+1])
			i++
		case s[i] == '"':
			return b.String(), true
		default:
			b.WriteByte(s[i])
		}
	}
	return "", false
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
