package pi

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/RenseiAI/donmai/agent"
	"github.com/RenseiAI/donmai/internal/codeintelcontract"
	"github.com/RenseiAI/donmai/internal/toolpolicy"
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
	// stateRoot is the session's relocated harness state root
	// (sessionStateRoot). The state-dir guard protects it alongside the
	// legacy in-checkout directory.
	stateRoot string

	// allowGated is the session's effective toolApproval level being
	// allow-list or stronger (ADR-2026-09-27-execution-security-levels.md).
	// Below it (bypass, deny-list) no call waits on a gate the runner did
	// not configure, so the autonomous network-bash fallback deny (step 6),
	// which only ever stood in for an unconfigured allow list, is off.
	// Safety denies, containment, the deny entries and an explicitly
	// configured allow list still apply at every level.
	allowGated bool

	allowRegexes []*regexp.Regexp
	denyRegexes  []*regexp.Regexp

	// allowedTools / disallowedTools are the unconstrained tool-kind
	// patterns ("Read", "Bash", "*"). Constrained Bash and path patterns are
	// compiled by internal/toolpolicy: denyCommands / allowCommands (with the
	// PermissionConfig rules), denyReads / denyWrites (a scoped path deny of
	// any read or write tool covers every read or write kind; see
	// policy_paths.go for how they fail closed) and allowPaths (a scoped path
	// allow covers only its own tool kind).
	allowedTools    []toolPattern
	disallowedTools []toolPattern
	denyCommands    toolpolicy.Commands
	allowCommands   toolpolicy.Commands
	denyReads       pathDenies
	denyWrites      pathDenies
	allowPaths      map[ToolKind]toolpolicy.Paths
	hasAllowlist    bool

	// defaultAllow is the fallback when no explicit rule matches. Derived
	// from PermissionConfig.DefaultDecision: "allow"/"" ⇒ true;
	// "deny"/"prompt" ⇒ false (autonomous sessions cannot answer a prompt).
	defaultAllow bool
}

// NewPolicyEngine builds the adjudicator from a Spec. It reads
// Spec.PermissionConfig (Claude-grammar allow/deny regexes + DefaultDecision),
// Spec.AllowedTools / Spec.DisallowedTools (Claude tool patterns), Spec.Cwd
// (containment root), and Spec.Autonomous (network-bash default-deny).
// The harness state root is derived the same way the spawn path derives it
// (sessionStateRoot), so the state-dir guard protects the relocated state
// whether or not the caller materialized a layout first.
func NewPolicyEngine(spec agent.Spec) *PolicyEngine {
	e := &PolicyEngine{
		autonomous:      spec.Autonomous,
		cwd:             spec.Cwd,
		stateRoot:       sessionStateRoot(spec),
		allowGated:      spec.ToolApprovalAllowGated(),
		allowedTools:    unconstrained(parseToolPatterns(spec.AllowedTools)),
		disallowedTools: unconstrained(parseToolPatterns(spec.DisallowedTools)),
		defaultAllow:    true,
	}
	denies, allows := scoped(spec.DisallowedTools), scoped(spec.AllowedTools)
	e.hasAllowlist = len(spec.AllowedTools) > 0
	if cfg := spec.PermissionConfig; cfg != nil {
		denies = append(denies, cfg.DisallowPatterns...)
		allows = append(allows, cfg.AllowPatterns...)
		e.hasAllowlist = e.hasAllowlist || len(cfg.AllowPatterns) > 0
	}
	e.denyCommands = toolpolicy.CompileCommands(denies, toolpolicy.Deny)
	e.allowCommands = toolpolicy.CompileCommands(allows, toolpolicy.Allow)
	home, _ := os.UserHomeDir()
	var errs, more []error
	e.denyReads, errs = compilePathDenies(denies, isReadToolName, spec.Cwd, home, foldPathCase)
	e.denyWrites, more = compilePathDenies(denies, isWriteToolName, spec.Cwd, home, foldPathCase)
	e.allowPaths = map[ToolKind]toolpolicy.Paths{}
	for kind, names := range pathAllowNames {
		e.allowPaths[kind], _ = toolpolicy.CompilePaths(allows, func(name string) bool {
			return slices.ContainsFunc(names, func(n string) bool { return strings.EqualFold(n, name) })
		}, toolpolicy.Allow, spec.Cwd, home)
	}
	if errs = append(errs, more...); len(errs) > 0 {
		slog.Warn("pi: path denies that cannot be compiled deny every path of their tool family", "errors", errs)
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
//  6. Autonomous network-reaching bash with no allow ⇒ deny, only when the
//     toolApproval level is allow-list or stronger.
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
		// running session (statedir_guard.go). Both the relocated state
		// root and the legacy in-checkout directory are guarded: the
		// relocated root is live state, and the legacy path may hold a
		// previous version's live state.
		if reason := stateDirDeletionReasonForRoots(call.Command, e.containmentRoot(call), e.stateRoot); reason != "" {
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

	// 4. Command and path denies, and PermissionConfig regexes on a file
	// path (a bash command is matched by denyCommands).
	if call.Kind == ToolBash && e.denyCommands.Match(call.Command) {
		return Decision{Allow: false, Reason: "command matches a disallow pattern"}
	}
	if denied, reason := e.pathDenied(call); denied {
		return Decision{Allow: false, Reason: reason + ": " + call.Path}
	}
	for _, re := range e.denyRegexes {
		if call.Kind != ToolBash && re.MatchString(subject) {
			return Decision{Allow: false, Reason: "matches disallow pattern: " + re.String()}
		}
	}

	// 5. Allow-gate: when any allow rule exists, it becomes mandatory. A
	// command must be covered whole and command by command; a file path by a
	// path allow of its own tool kind.
	if e.hasAllowlist {
		if e.matchesAllowTool(call) || e.allowedByRules(call) || (call.Kind != ToolBash && e.matchesAllowRegex(subject)) {
			return Decision{Allow: true}
		}
		return Decision{Allow: false, Reason: "no allow pattern matched and an allow-list is configured"}
	}

	// 6. Autonomous network-reaching bash defaults deny — a fallback gate
	// that only an allow-gated level carries.
	if e.autonomous && e.allowGated && call.Kind == ToolBash && networkReaching.MatchString(call.Command) {
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

// pathDenied applies the path denies, with the reason. grep, find and ls
// walk their path, or without one the working directory: both the session's
// and the call's are checked, and with neither the walk is denied.
func (e *PolicyEngine) pathDenied(call ToolCall) (bool, string) {
	switch {
	case isMutatingKind(call.Kind):
		if call.Path == "" {
			return false, ""
		}
		return e.denyWrites.denies(call.Path, false, false)
	case isReadKind(call.Kind):
		walk := call.Kind != ToolRead
		paths := []string{call.Path}
		if call.Path == "" {
			if !walk {
				return false, ""
			}
			paths = nil
			for _, dir := range []string{e.cwd, call.Cwd} {
				if dir != "" {
					paths = append(paths, dir)
				}
			}
			if len(paths) == 0 && e.denyReads.active() {
				return true, "walk without a directory cannot be checked against path denies"
			}
		}
		for _, p := range paths {
			if denied, reason := e.denyReads.denies(p, walk, call.Kind == ToolRead); denied {
				return true, reason
			}
		}
	}
	return false, ""
}

func (e *PolicyEngine) allowedByRules(call ToolCall) bool {
	if call.Kind == ToolBash {
		return e.allowCommands.Match(call.Command)
	}
	rules, ok := e.allowPaths[call.Kind]
	return ok && call.Path != "" && rules.Match(call.Path, e.containmentRoot(call))
}

// pathAllowNames maps each file tool kind to the Claude tool names whose
// scoped path allow covers it.
var pathAllowNames = map[ToolKind][]string{
	ToolRead: {"Read"}, ToolGrep: {"Grep"}, ToolFind: {"Glob", "Find"}, ToolLs: {"LS"},
	ToolWrite: {"Write"}, ToolEdit: {"Edit", "MultiEdit"},
}

func isReadToolName(name string) bool {
	return slices.ContainsFunc([]string{"Read", "Grep", "Glob", "Find", "LS"}, func(n string) bool { return strings.EqualFold(n, name) })
}

func isWriteToolName(name string) bool {
	return slices.ContainsFunc([]string{"Write", "Edit", "MultiEdit", "NotebookEdit"}, func(n string) bool { return strings.EqualFold(n, name) })
}

// scoped keeps the Name(arg) patterns; unconstrained keeps the bare ones.
// A Claude tool name is rewritten to the spelling internal/toolpolicy
// compiles, because the flat tool lists match tool names case-insensitively
// (as parseToolPatterns does): "bash(git push*)" is a Bash deny, not a raw
// command regex that matches nothing.
func scoped(patterns []string) []string {
	var out []string
	for _, p := range patterns {
		p = strings.TrimSpace(p)
		name, arg, ok := toolpolicy.ParseToolPattern(p)
		if !ok {
			continue
		}
		if canonical, known := claudeToolNames[strings.ToLower(strings.TrimSpace(name))]; known {
			p = canonical + "(" + arg + ")"
		}
		out = append(out, p)
	}
	return out
}

// claudeToolNames maps a lower-cased Claude tool name to its canonical
// spelling.
var claudeToolNames = map[string]string{
	"bash": "Bash", "read": "Read", "grep": "Grep", "glob": "Glob", "find": "Find", "ls": "LS",
	"write": "Write", "edit": "Edit", "multiedit": "MultiEdit", "notebookedit": "NotebookEdit",
}

// A constraint that is empty after the trailing ":*" or "*" is trimmed
// ("Bash(*)", "Bash(:*)") keeps its legacy whole-kind meaning.
func unconstrained(patterns []toolPattern) []toolPattern {
	var out []toolPattern
	for _, p := range patterns {
		if !p.hasConstr || p.constraint == "" {
			out = append(out, p)
		}
	}
	return out
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
		if alias, ok := claudeToolAliases[normalizedName]; ok {
			kind, known = alias, true
		}
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

// claudeToolAliases maps Claude tool names to the pi built-in they gate.
var claudeToolAliases = map[string]ToolKind{"glob": ToolFind, "multiedit": ToolEdit}

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
		defaultAllow: spec.PermissionConfig != nil && strings.EqualFold(spec.PermissionConfig.DefaultDecision, "allow"),
	}
	// Below an allow-gated toolApproval level with no configured policy at
	// all, the selected surface runs like every other tool: nothing but the
	// deny entries stands in front of it. A configured PermissionConfig keeps
	// its own default decision.
	if !policy.hasAllowGate && !spec.ToolApprovalAllowGated() && spec.PermissionConfig == nil {
		policy.defaultAllow = true
	}
	if len(selected) == 0 {
		return nil, fmt.Errorf("native code-intelligence selected set is empty")
	}
	if spec.PermissionConfig != nil {
		switch strings.ToLower(spec.PermissionConfig.DefaultDecision) {
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
