package pi

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"github.com/RenseiAI/donmai/agent"
	"github.com/RenseiAI/donmai/internal/codeintelcontract"
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
// A tool_execution_end naming one of these WITHOUT a recorded adjudication
// outcome is an UNPROVEN call: the monitor records it and surfaces a
// non-fatal error, because "no record" cannot tell a real bypass apart from a
// lost ruling (handle.go integrity monitor, doc.go "The fail-safe fence").
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

// callIDFieldNames is the ONE rule for reading a tool call's id, in priority
// order. Every site that has to correlate a single call across the trust
// boundary reads it through toolCallID below: the bypass monitor consuming a
// tool_execution_end, the adjudication/refusal payload reader, and the event
// mapper's ToolResultEvent (handle.go, event_mapping.go). The embedded
// extension's own toolCallIdOf() accepts the same spellings in the same order
// (extensions/donmai-policy.ts).
//
// The symmetry is load-bearing, not cosmetic. A writer that serialized only
// `toolCallId` while the reader also accepted `callId`/`call_id`/`id` would
// leave an HONOURED ruling unrecorded whenever the runtime spelled the field
// any other way — and an unrecorded ruling used to be read as a bypass and
// killed the session (doc.go, "The fail-safe fence").
var callIDFieldNames = []string{"toolCallId", "callId", "call_id", "id"}

// toolCallID reads a tool call id from any accepted spelling. It returns ""
// when the id is absent or empty — a call that cannot be correlated, which
// the monitor records as an unproven call rather than treating as a bypass.
//
// A NUMERIC id is accepted and rendered the same way the extension's
// toolCallIdOf renders it (JavaScript String(n)), because the symmetry has to
// hold in both directions: if a runtime ever spelled ids numerically, a writer
// that serialized "123" against a reader that saw float64(123) and gave up
// would turn every honoured ruling into an unproven call. Integral values
// render identically on both sides; values large enough that JavaScript
// switches to exponent notation (>= 1e21) are out of scope — no runtime mints
// call ids there.
func toolCallID(fields map[string]any) string {
	for _, key := range callIDFieldNames {
		switch value := fields[key].(type) {
		case string:
			if value != "" {
				return value
			}
		case float64:
			return strconv.FormatFloat(value, 'f', -1, 64)
		case json.Number:
			return value.String()
		}
	}
	return ""
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
//
// What a deny is worth downstream: it is recorded against the call id before
// it is delivered, and the integrity monitor later treats an end event for
// that call as a bypass ONLY if the runtime reports the call ran and
// succeeded. A denied call that ends with an error result is indistinguishable
// from an honoured block — pi finalizes a blocked call with exactly that
// shape, carrying this Reason as the result text — so a bypass whose tool
// errors after its side effects is not caught. The reason text is not used to
// tell those apart: tool output is the one part of the event a hostile
// execution can write, and the monitor's session-fatal path must not rest on
// it (doc.go, "What case 1 cannot see, and why it is not widened").
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
	raw         string
	kind        ToolKind // the tool family this pattern gates
	anyKind     bool     // true only for the explicit "*" tool wildcard
	unsupported bool     // foreign or malformed names match no pi built-in
	constraint  string   // stripped of a trailing ":*" / "*"
	hasConstr   bool
}

// parseToolPatterns parses a Claude tool-pattern list into toolPatterns.
func parseToolPatterns(patterns []string) []toolPattern {
	out := make([]toolPattern, 0, len(patterns))
	for _, p := range patterns {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		pattern := toolPattern{raw: p}
		name := p
		if i := strings.IndexByte(p, '('); i >= 0 {
			pattern.hasConstr = true
			if i == 0 || !strings.HasSuffix(p, ")") {
				pattern.unsupported = true
				out = append(out, pattern)
				continue
			}
			name = p[:i]
			pattern.constraint = p[i+1 : len(p)-1]
			pattern.constraint = strings.TrimSuffix(pattern.constraint, "*")
			pattern.constraint = strings.TrimSuffix(pattern.constraint, ":")
		} else if strings.Contains(p, ")") {
			pattern.unsupported = true
		}

		normalizedName := strings.ToLower(strings.TrimSpace(name))
		kind, known := builtInToolNames[normalizedName]
		switch {
		case normalizedName == "*":
			pattern.anyKind = true
		case known:
			pattern.kind = kind
		default:
			pattern.unsupported = true
		}
		out = append(out, pattern)
	}
	return out
}

// matches reports whether the pattern applies to the given call.
func (tp toolPattern) matches(call ToolCall) bool {
	if tp.unsupported {
		return false
	}
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

type nativeCodeIntelPolicy struct {
	selected      map[string]bool
	allowed       map[string]bool
	disallowed    map[string]bool
	allowRegexes  []*regexp.Regexp
	denyRegexes   []*regexp.Regexp
	allowWildcard bool
	denyWildcard  bool
	hasAllowGate  bool
	defaultAllow  bool
}

var nativePolicyToolDesignator = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*(\(.*\))?$`)

func newNativeCodeIntelPolicy(spec agent.Spec, selected []string) (*nativeCodeIntelPolicy, error) {
	policy := &nativeCodeIntelPolicy{
		selected:     make(map[string]bool, len(selected)),
		allowed:      make(map[string]bool),
		disallowed:   make(map[string]bool),
		hasAllowGate: len(spec.AllowedTools) > 0 || spec.PermissionConfig != nil && len(spec.PermissionConfig.AllowPatterns) > 0,
		defaultAllow: spec.PermissionConfig != nil && strings.EqualFold(strings.TrimSpace(spec.PermissionConfig.DefaultDecision), "allow"),
	}
	if len(selected) == 0 {
		return nil, fmt.Errorf("native code-intelligence selected set is empty")
	}
	if spec.PermissionConfig != nil {
		switch strings.ToLower(strings.TrimSpace(spec.PermissionConfig.DefaultDecision)) {
		case "", "allow", "deny", "prompt", "ask":
		default:
			return nil, fmt.Errorf("native code-intelligence default decision is invalid")
		}
	}
	for _, name := range selected {
		match, err := codeintelcontract.NormalizePolicyIdentity(name)
		if err != nil || !match.Related || match.Canonical != name || policy.selected[name] {
			return nil, fmt.Errorf("native code-intelligence selected set is invalid")
		}
		policy.selected[name] = true
	}

	var err error
	policy.allowWildcard, err = collectNativeToolRules(spec.AllowedTools, policy.allowed)
	if err != nil {
		return nil, fmt.Errorf("native code-intelligence allowed tools: %w", err)
	}
	policy.denyWildcard, err = collectNativeToolRules(spec.DisallowedTools, policy.disallowed)
	if err != nil {
		return nil, fmt.Errorf("native code-intelligence disallowed tools: %w", err)
	}
	if spec.PermissionConfig != nil {
		allowWildcard, regexes, err := collectNativePermissionRules(spec.PermissionConfig.AllowPatterns, policy.allowed)
		if err != nil {
			return nil, fmt.Errorf("native code-intelligence allow patterns: %w", err)
		}
		policy.allowWildcard = policy.allowWildcard || allowWildcard
		policy.allowRegexes = regexes
		denyWildcard, regexes, err := collectNativePermissionRules(spec.PermissionConfig.DisallowPatterns, policy.disallowed)
		if err != nil {
			return nil, fmt.Errorf("native code-intelligence disallow patterns: %w", err)
		}
		policy.denyWildcard = policy.denyWildcard || denyWildcard
		policy.denyRegexes = regexes
	}
	return policy, nil
}

func collectNativeToolRules(values []string, exact map[string]bool) (bool, error) {
	wildcard := false
	for _, value := range values {
		if strings.TrimSpace(value) == "" {
			return false, fmt.Errorf("tool pattern is blank")
		}
		if value == "*" {
			wildcard = true
			continue
		}
		match, err := codeintelcontract.NormalizePolicyIdentity(value)
		if err != nil {
			return false, err
		}
		if match.Related {
			exact[match.Canonical] = true
		}
	}
	return wildcard, nil
}

func collectNativePermissionRules(values []string, exact map[string]bool) (bool, []*regexp.Regexp, error) {
	wildcard := false
	regexes := make([]*regexp.Regexp, 0, len(values))
	for _, value := range values {
		if strings.TrimSpace(value) == "" {
			return false, nil, fmt.Errorf("permission pattern is blank")
		}
		if value == "*" {
			wildcard = true
			continue
		}
		match, err := codeintelcontract.NormalizePolicyIdentity(value)
		if err != nil {
			return false, nil, err
		}
		if match.Related {
			exact[match.Canonical] = true
			continue
		}
		if nativePolicyToolDesignator.MatchString(strings.TrimSpace(value)) {
			continue
		}
		re, err := regexp.Compile(value)
		if err != nil {
			return false, nil, fmt.Errorf("permission regex is invalid")
		}
		regexes = append(regexes, re)
	}
	return wildcard, regexes, nil
}

func (p *nativeCodeIntelPolicy) Evaluate(name string) Decision {
	match, err := codeintelcontract.NormalizePolicyIdentity(name)
	if err != nil || !match.Related {
		return Decision{Allow: false, Reason: "native code-intelligence tool identity is invalid"}
	}
	canonical := match.Canonical
	if !p.selected[canonical] {
		return Decision{Allow: false, Reason: "native code-intelligence tool is outside the selected capability surface"}
	}
	if p.denyWildcard || p.disallowed[canonical] || matchesNativeRegex(p.denyRegexes, canonical) {
		return Decision{Allow: false, Reason: "native code-intelligence tool is explicitly denied"}
	}
	if p.hasAllowGate {
		if p.allowWildcard || p.allowed[canonical] || matchesNativeRegex(p.allowRegexes, canonical) {
			return Decision{Allow: true}
		}
		return Decision{Allow: false, Reason: "no native code-intelligence allow rule matched"}
	}
	if p.defaultAllow {
		return Decision{Allow: true}
	}
	return Decision{Allow: false, Reason: "native code-intelligence defaults to deny"}
}

func matchesNativeRegex(regexes []*regexp.Regexp, subject string) bool {
	for _, re := range regexes {
		if re.MatchString(subject) {
			return true
		}
	}
	return false
}
