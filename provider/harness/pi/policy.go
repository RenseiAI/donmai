package pi

import (
	"path/filepath"
	"regexp"
	"strings"

	"github.com/RenseiAI/donmai/agent"
)

// ToolKind classifies an intercepted built-in tool call. pi ships read /
// write / edit / bash plus the read-only variants grep / find / ls and shell
// `!` commands (design §5.1); the extension overrides every one of them and
// routes the intended call here for adjudication.
type ToolKind string

// ToolKind constants name the built-in tool families the policy adjudicates.
const (
	ToolBash  ToolKind = "bash"  // bash + shell `!` commands
	ToolWrite ToolKind = "write" // write / edit (mutating file ops)
	ToolEdit  ToolKind = "edit"
	ToolRead  ToolKind = "read" // read / grep / find / ls (read-only file ops)
	ToolGrep  ToolKind = "grep"
	ToolFind  ToolKind = "find"
	ToolLs    ToolKind = "ls"
)

// builtInToolNames is the set of pi built-in tools the extension overrides.
// A tool_execution_start naming one of these WITHOUT a preceding adjudication
// round-trip is a policy bypass (handle.go integrity monitor, design §5.3).
var builtInToolNames = map[string]ToolKind{
	"read":  ToolRead,
	"write": ToolWrite,
	"edit":  ToolEdit,
	"bash":  ToolBash,
	"grep":  ToolGrep,
	"find":  ToolFind,
	"ls":    ToolLs,
}

// isBuiltInTool reports whether name is one of pi's overridden built-in tools.
func isBuiltInTool(name string) bool {
	_, ok := builtInToolNames[strings.ToLower(strings.TrimSpace(name))]
	return ok
}

// isMutatingKind reports whether k mutates the filesystem (write/edit).
func isMutatingKind(k ToolKind) bool { return k == ToolWrite || k == ToolEdit }

// isReadKind reports whether k is a read-only file op (read/grep/find/ls).
func isReadKind(k ToolKind) bool {
	return k == ToolRead || k == ToolGrep || k == ToolFind || k == ToolLs
}

// ToolCall is the intended tool invocation parsed from an extension_ui_request
// (design §5.1: the overridden tool serializes tool, args, resolved paths,
// cwd). Path is the resolved absolute path for a file op; Command is the shell
// text for a bash op.
type ToolCall struct {
	Kind    ToolKind
	Command string // bash text
	Path    string // resolved path for file ops
	Cwd     string // worktree root, for containment
}

// Decision is the adjudicator verdict for one tool call. Reason is filled on
// deny so the model (which receives the deny string) sees WHY — mirroring
// codex ApprovalDecision.Reason.
type Decision struct {
	Allow  bool
	Reason string
}

// safetyDeny is a built-in deny pattern checked before any user-level rule.
// Byte-identical to codex/approval.go's list (design §5.2 names the same
// rules) — a rm -rf /, worktree remove/prune, reset --hard, bare force-push,
// recursive chmod/chown, curl|sh, or sudo is denied regardless of any allow
// config, because those corrupt the worktree or exfiltrate.
type safetyDeny struct {
	pattern *regexp.Regexp
	reason  string
}

var builtInSafetyDeny = []safetyDeny{
	{regexp.MustCompile(`(?i)\brm\s+(-[a-zA-Z]*r[a-zA-Z]*\s+)?(-[a-zA-Z]*f[a-zA-Z]*\s+)?(/\s*$|/\s+|/\*\s*$)`), "rm of filesystem root blocked"},
	{regexp.MustCompile(`\bgit\s+worktree\s+(remove|prune)\b`), "git worktree remove/prune is reserved for the runner"},
	{regexp.MustCompile(`\bgit\s+reset\s+--hard\b`), "git reset --hard would discard work-in-progress"},
	{regexp.MustCompile(`\bgit\s+push\s+--force(\s|$)`), "git push --force without --force-with-lease blocked"},
	{regexp.MustCompile(`\b(chmod|chown)\s+-?[Rr]?\s+/\S+`), "recursive chmod/chown on absolute paths blocked"},
	{regexp.MustCompile(`(?i)\b(curl|wget)\b[^|]*\|\s*(sudo\s+)?(bash|sh|zsh|dash|ksh)\b`), "piping a download to a shell blocked"},
	{regexp.MustCompile(`(?i)\bsudo\b`), "sudo invocation blocked"},
}

// networkReaching matches bash commands that reach the network. In an
// autonomous session with no explicit allow, these default-deny (design §5.2:
// "autonomous default: deny for network-reaching bash, allow for in-tree file
// ops"). Explicit AllowPatterns still override this (evaluated first below).
var networkReaching = regexp.MustCompile(`(?i)\b(curl|wget|nc|ncat|ssh|scp|rsync|telnet|ftp|npm\s+(i|install|ci)|pnpm\s+(i|install|add)|yarn\s+add|pip\s+install|go\s+get|git\s+(clone|fetch|pull|push))\b`)

// PolicyEngine adjudicates intercepted tool calls against the built-in safety
// rules, path containment, and the Spec's permission configuration. It is the
// codex ApprovalBridge generalized to pi's richer tool surface.
//
// Concurrency: safe for concurrent Evaluate calls (all fields read-only after
// construction).
type PolicyEngine struct {
	autonomous bool
	cwd        string

	allowRegexes []*regexp.Regexp
	denyRegexes  []*regexp.Regexp

	allowedTools    []toolPattern
	disallowedTools []toolPattern

	// defaultAllow is the fallback when no explicit rule matches. Derived
	// from PermissionConfig.DefaultDecision: "allow"/"" ⇒ true;
	// "deny"/"prompt" ⇒ false (autonomous sessions cannot answer a prompt).
	defaultAllow bool
}

// NewPolicyEngine builds the adjudicator from a Spec. It reads
// Spec.PermissionConfig (Claude-grammar allow/deny regexes + DefaultDecision),
// Spec.AllowedTools / Spec.DisallowedTools (Claude tool patterns), Spec.Cwd
// (containment root), and Spec.Autonomous (network-bash default-deny).
func NewPolicyEngine(spec agent.Spec) *PolicyEngine {
	e := &PolicyEngine{
		autonomous:      spec.Autonomous,
		cwd:             spec.Cwd,
		allowedTools:    parseToolPatterns(spec.AllowedTools),
		disallowedTools: parseToolPatterns(spec.DisallowedTools),
		defaultAllow:    true,
	}
	if cfg := spec.PermissionConfig; cfg != nil {
		e.allowRegexes = compilePatterns(cfg.AllowPatterns)
		e.denyRegexes = compilePatterns(cfg.DisallowPatterns)
		switch strings.ToLower(cfg.DefaultDecision) {
		case "deny", "prompt":
			e.defaultAllow = false
		case "allow", "":
			e.defaultAllow = true
		}
	}
	return e
}

// Evaluate produces the verdict for one intercepted tool call.
//
// Order (fail-closed at every step):
//  1. Built-in safety deny (bash only; cannot be overridden by any config).
//  2. Path containment (mutating ops outside cwd; reads outside cwd for
//     autonomous sessions) — checked BEFORE allow patterns so an allow cannot
//     grant an out-of-tree write it did not resolve a path for. An explicit
//     PermissionConfig AllowPattern covering the resolved path re-permits it.
//  3. Spec.DisallowedTools tool-pattern match ⇒ deny.
//  4. PermissionConfig DisallowPatterns regex match ⇒ deny.
//  5. Spec.AllowedTools / PermissionConfig AllowPatterns ⇒ allow-gate: when
//     any allow rule is configured, ONLY matching calls pass; the rest deny.
//  6. Autonomous network-reaching bash with no allow ⇒ deny.
//  7. defaultAllow.
func (e *PolicyEngine) Evaluate(call ToolCall) Decision {
	subject := call.subject()

	// 1. Built-in safety deny (bash text only).
	if call.Kind == ToolBash {
		for _, sd := range builtInSafetyDeny {
			if sd.pattern.MatchString(call.Command) {
				return Decision{Allow: false, Reason: sd.reason}
			}
		}
		// 1b. The harness's own state directory. Same standing as the rules
		// above — built-in, un-overridable — because losing it strands the
		// running session (statedir_guard.go).
		if reason := stateDirDeletionReason(call.Command, e.containmentRoot(call)); reason != "" {
			return Decision{Allow: false, Reason: reason}
		}
	}

	// 2. Path containment for file ops.
	if call.Path != "" && (isMutatingKind(call.Kind) || isReadKind(call.Kind)) {
		if reason, contained := e.checkContainment(call); !contained {
			// An explicit allow pattern covering the path re-permits it
			// (e.g. a sanctioned shared cache outside the worktree).
			if !e.matchesAllowRegex(subject) {
				return Decision{Allow: false, Reason: reason}
			}
		}
	}

	// 3. Spec.DisallowedTools.
	for _, tp := range e.disallowedTools {
		if tp.matches(call) {
			return Decision{Allow: false, Reason: "tool call matches a disallowed-tools pattern: " + tp.raw}
		}
	}

	// 4. PermissionConfig DisallowPatterns.
	for _, re := range e.denyRegexes {
		if re.MatchString(subject) {
			return Decision{Allow: false, Reason: "matches disallow pattern: " + re.String()}
		}
	}

	// 5. Allow-gate: when any allow rule exists, it becomes mandatory.
	if len(e.allowedTools) > 0 || len(e.allowRegexes) > 0 {
		if e.matchesAllowTool(call) || e.matchesAllowRegex(subject) {
			return Decision{Allow: true}
		}
		return Decision{Allow: false, Reason: "no allow pattern matched and an allow-list is configured"}
	}

	// 6. Autonomous network-reaching bash defaults deny.
	if e.autonomous && call.Kind == ToolBash && networkReaching.MatchString(call.Command) {
		return Decision{Allow: false, Reason: "network-reaching bash denied by default in autonomous session (no allow pattern configured)"}
	}

	// 7. Default.
	if e.defaultAllow {
		return Decision{Allow: true}
	}
	return Decision{Allow: false, Reason: "default decision is deny/prompt and no allow pattern matched"}
}

// containmentRoot is the session worktree root a call is judged against. The
// Spec's Cwd (captured at construction) is authoritative; a call that carries
// its own Cwd is only consulted when the engine has none, so a tool call
// cannot relocate the boundary it is being judged against.
func (e *PolicyEngine) containmentRoot(call ToolCall) string {
	if e.cwd != "" {
		return e.cwd
	}
	return call.Cwd
}

// subject returns the string the allow/deny regexes match against: the
// command text for bash, the resolved path for a file op.
func (c ToolCall) subject() string {
	if c.Kind == ToolBash {
		return strings.TrimSpace(c.Command)
	}
	return c.Path
}

// checkContainment enforces the worktree boundary. Returns contained=false
// with a reason when the path escapes cwd (mutating ops always; reads only in
// autonomous sessions — an interactive user may legitimately read outside).
func (e *PolicyEngine) checkContainment(call ToolCall) (reason string, contained bool) {
	if e.cwd == "" {
		return "", true
	}
	clean := filepath.Clean(call.Path)
	root := filepath.Clean(e.cwd)
	inside := clean == root || strings.HasPrefix(clean+string(filepath.Separator), root+string(filepath.Separator))

	// .git mutation is always denied regardless of containment.
	if isMutatingKind(call.Kind) {
		if strings.Contains(clean, string(filepath.Separator)+".git"+string(filepath.Separator)) ||
			strings.HasSuffix(clean, string(filepath.Separator)+".git") {
			return ".git directory modification blocked", false
		}
	}

	if inside {
		return "", true
	}
	switch {
	case isMutatingKind(call.Kind):
		return "file write/edit outside the worktree blocked: " + clean, false
	case isReadKind(call.Kind) && e.autonomous:
		return "file read outside the worktree denied for autonomous session: " + clean, false
	default:
		return "", true
	}
}

func (e *PolicyEngine) matchesAllowRegex(subject string) bool {
	for _, re := range e.allowRegexes {
		if re.MatchString(subject) {
			return true
		}
	}
	return false
}

func (e *PolicyEngine) matchesAllowTool(call ToolCall) bool {
	for _, tp := range e.allowedTools {
		if tp.matches(call) {
			return true
		}
	}
	return false
}

// toolPattern is a parsed Claude-grammar tool pattern: a tool name with an
// optional argument constraint, e.g. "Bash(git:*)", "Read", "Write(src/**)".
// The constraint is a prefix match: "git:*" allows bash commands whose text
// begins with "git"; "src/**" allows file paths under "src/".
type toolPattern struct {
	raw        string
	kind       ToolKind // the tool family this pattern gates
	anyKind    bool     // true when the pattern names no known built-in family
	constraint string   // stripped of a trailing ":*" / "*"
	hasConstr  bool
}

// parseToolPatterns parses a Claude tool-pattern list into toolPatterns.
func parseToolPatterns(patterns []string) []toolPattern {
	out := make([]toolPattern, 0, len(patterns))
	for _, p := range patterns {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		name := p
		constraint := ""
		hasConstr := false
		if i := strings.IndexByte(p, '('); i >= 0 && strings.HasSuffix(p, ")") {
			name = p[:i]
			constraint = p[i+1 : len(p)-1]
			hasConstr = true
			constraint = strings.TrimSuffix(constraint, "*")
			constraint = strings.TrimSuffix(constraint, ":")
		}
		kind, known := builtInToolNames[strings.ToLower(strings.TrimSpace(name))]
		out = append(out, toolPattern{
			raw:        p,
			kind:       kind,
			anyKind:    !known,
			constraint: constraint,
			hasConstr:  hasConstr,
		})
	}
	return out
}

// matches reports whether the pattern applies to the given call.
func (tp toolPattern) matches(call ToolCall) bool {
	if !tp.anyKind && tp.kind != call.Kind {
		return false
	}
	if !tp.hasConstr || tp.constraint == "" {
		return true
	}
	return strings.HasPrefix(strings.TrimSpace(call.subject()), tp.constraint)
}

// compilePatterns compiles user-supplied regexes with Compile (not
// MustCompile) so a malformed pattern is dropped rather than crashing the
// daemon — same posture as codex/approval.go.
func compilePatterns(patterns []string) []*regexp.Regexp {
	out := make([]*regexp.Regexp, 0, len(patterns))
	for _, p := range patterns {
		re, err := regexp.Compile(p)
		if err != nil {
			continue
		}
		out = append(out, re)
	}
	return out
}
