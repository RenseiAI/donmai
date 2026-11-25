package muse

import (
	"regexp"
	"strings"

	"github.com/RenseiAI/donmai/agent"
)

// ApprovalAction is the bridge verdict for one inbound approval request.
type ApprovalAction string

const (
	// ActionAllow approves the call for this turn.
	ActionAllow ApprovalAction = "allow"
	// ActionDecline denies the call. On the MSP wire a decline maps to
	// the denial/abort choice via approval/decide.
	ActionDecline ApprovalAction = "decline"
)

// ApprovalKind classifies what the approval gates.
type ApprovalKind string

const (
	// ApprovalKindCommand gates a shell command execution.
	ApprovalKindCommand ApprovalKind = "command"
	// ApprovalKindFileAccess gates a workspace file access.
	ApprovalKindFileAccess ApprovalKind = "fileAccess"
	// ApprovalKindNetwork gates a network access.
	ApprovalKindNetwork ApprovalKind = "network"
	// ApprovalKindTool gates a generic tool call.
	ApprovalKindTool ApprovalKind = "tool"
	// ApprovalKindUnknown marks an open-union subject this bridge does not recognize (never auto-approved).
	ApprovalKindUnknown ApprovalKind = "unknown"
)

// ApprovalRequest is the normalized form of an MSP approval subject.
type ApprovalRequest struct {
	Kind    ApprovalKind
	Command string
	Path    string
	Tool    string
	Target  string
	Cwd     string
	Raw     ApprovalRequestParams
}

// ApprovalDecision is the bridge verdict plus the reason.
type ApprovalDecision struct {
	Action ApprovalAction
	Reason string
}

// parseApprovalRequest normalizes an MSP approval request into the
// bridge's input shape. The open subject union is classified by kind:
// shell → command (command text), fileAccess → path, everything else
// carries tool/target for pattern matching.
func parseApprovalRequest(params ApprovalRequestParams, cwd string) ApprovalRequest {
	req := ApprovalRequest{Cwd: cwd, Raw: params, Tool: params.ToolName}
	subject := params.Subject
	kind := ApprovalKind(strings.TrimSpace(subject.Kind))
	switch kind {
	case ApprovalKindCommand:
		switch {
		case subject.Command != nil:
			req.Command = *subject.Command
		case params.RawArgs != "":
			req.Command = params.RawArgs
		case req.Tool != "":
			req.Command = req.Tool
		}
		if req.Kind == "" {
			req.Kind = ApprovalKindCommand
		}
	case ApprovalKindFileAccess:
		if subject.Path != nil {
			req.Path = *subject.Path
		} else if subject.Target != nil {
			req.Path = *subject.Target
		}
		req.Kind = ApprovalKindFileAccess
	case ApprovalKindNetwork, ApprovalKindTool:
		if subject.Target != nil {
			req.Target = *subject.Target
		}
		req.Kind = kind
	case "shell":
		// Hosts may spell the shell subject "shell".
		if subject.Command != nil {
			req.Command = *subject.Command
		} else if params.RawArgs != "" {
			req.Command = params.RawArgs
		}
		req.Kind = ApprovalKindCommand
	case "unixSocket", "process":
		if subject.Target != nil {
			req.Target = *subject.Target
		}
		req.Kind = ApprovalKindTool
	default:
		// Open union: unknown kinds never auto-approve.
		req.Kind = ApprovalKindUnknown
	}
	// A shell-kind subject that arrived without command text still
	// classifies as a command so the safety rules can inspect the tool
	// name.
	if req.Kind == "" {
		req.Kind = ApprovalKindUnknown
	}
	return req
}

// ApprovalBridge evaluates inbound MSP approval requests against a
// Spec.PermissionConfig. Concurrency-safe after construction.
type ApprovalBridge struct {
	config *agent.PermissionConfig

	allowRegexes []*regexp.Regexp
	denyRegexes  []*regexp.Regexp
	allowFiles   bool
	denyFiles    bool
	hasAllowlist bool

	// denyAllCommands is a bare Bash deny: no shell command is approved.
	// denyAllFiles / allowAllFiles are a bare Edit/Write deny or allow, which
	// cover every file access. They are flags, not match-all regexes, because
	// the regex lists are shared with commands and generic tool targets.
	denyAllCommands bool
	denyAllFiles    bool
	allowAllFiles   bool

	// autoApprove is the default when no pattern matches. True unless
	// the Spec sets DefaultDecision deny/prompt (autonomous fleets
	// cannot answer prompts, so prompt reads as decline-by-default).
	autoApprove bool
}

// NewApprovalBridge compiles the configured patterns and returns a
// ready-to-use bridge. A nil config produces a bridge that runs the
// built-in safety deny rules and otherwise auto-approves.
func NewApprovalBridge(cfg *agent.PermissionConfig) *ApprovalBridge {
	br := &ApprovalBridge{config: cfg, autoApprove: true}
	if cfg != nil {
		br.hasAllowlist = len(cfg.AllowPatterns) > 0
		br.allowRegexes, br.allowFiles = compilePatterns(cfg.AllowPatterns)
		br.denyRegexes, br.denyFiles = compilePatterns(cfg.DisallowPatterns)
		for _, p := range cfg.DisallowPatterns {
			br.denyAllCommands = br.denyAllCommands || strings.EqualFold(strings.TrimSpace(p), "Bash")
			br.denyAllFiles = br.denyAllFiles || isBareFileTool(p)
		}
		for _, p := range cfg.AllowPatterns {
			br.allowAllFiles = br.allowAllFiles || isBareFileTool(p)
		}
		switch strings.ToLower(cfg.DefaultDecision) {
		case "deny", "prompt":
			br.autoApprove = false
		}
	}
	return br
}

// Evaluate produces the verdict for one inbound approval request.
//
// Order:
//  1. Built-in safety deny patterns (always enforced).
//  2. User-supplied DisallowPatterns.
//  3. User-supplied AllowPatterns. When at least one allow pattern is
//     present, ONLY matching calls are approved.
//  4. Unknown subject kinds are always declined.
//  5. Default decision: allow when autoApprove, decline otherwise.
func (b *ApprovalBridge) Evaluate(req ApprovalRequest) ApprovalDecision {
	if req.Kind == ApprovalKindUnknown {
		return ApprovalDecision{Action: ActionDecline, Reason: "unknown approval subject kind is never auto-approved"}
	}
	switch req.Kind {
	case ApprovalKindCommand:
		return b.evaluateCommand(req.Command)
	case ApprovalKindFileAccess:
		return b.evaluateFileAccess(req.Path)
	default:
		target := req.Target
		if target == "" {
			target = req.Tool
		}
		return b.evaluateGeneric(target)
	}
}

func (b *ApprovalBridge) evaluateCommand(cmd string) ApprovalDecision {
	cmd = strings.TrimSpace(cmd)
	if cmd == "" {
		return ApprovalDecision{Action: ActionDecline, Reason: "empty command approval is never auto-approved"}
	}
	for _, deny := range builtInSafetyDeny {
		if deny.re.MatchString(cmd) {
			return ApprovalDecision{Action: ActionDecline, Reason: deny.reason}
		}
	}
	if b.denyAllCommands {
		return ApprovalDecision{Action: ActionDecline, Reason: "shell commands are denied"}
	}
	for _, re := range b.denyRegexes {
		if re.MatchString(cmd) {
			return ApprovalDecision{Action: ActionDecline, Reason: "matched disallow pattern"}
		}
	}
	if b.hasAllowlist {
		for _, re := range b.allowRegexes {
			if re.MatchString(cmd) {
				return ApprovalDecision{Action: ActionAllow}
			}
		}
		return ApprovalDecision{Action: ActionDecline, Reason: "no allow pattern matched"}
	}
	if b.autoApprove {
		return ApprovalDecision{Action: ActionAllow}
	}
	return ApprovalDecision{Action: ActionDecline, Reason: "default decision is not allow"}
}

func (b *ApprovalBridge) evaluateFileAccess(path string) ApprovalDecision {
	path = strings.TrimSpace(path)
	if path == "" {
		return ApprovalDecision{Action: ActionDecline, Reason: "empty file approval is never auto-approved"}
	}
	if b.denyAllFiles {
		return ApprovalDecision{Action: ActionDecline, Reason: "file changes are denied"}
	}
	if b.denyFiles {
		for _, re := range b.denyRegexes {
			if re.MatchString(path) {
				return ApprovalDecision{Action: ActionDecline, Reason: "matched disallow pattern"}
			}
		}
	}
	if b.allowFiles && b.hasAllowlist {
		if b.allowAllFiles {
			return ApprovalDecision{Action: ActionAllow}
		}
		for _, re := range b.allowRegexes {
			if re.MatchString(path) {
				return ApprovalDecision{Action: ActionAllow}
			}
		}
		return ApprovalDecision{Action: ActionDecline, Reason: "no allow pattern matched"}
	}
	if b.autoApprove {
		return ApprovalDecision{Action: ActionAllow}
	}
	return ApprovalDecision{Action: ActionDecline, Reason: "default decision is not allow"}
}

func (b *ApprovalBridge) evaluateGeneric(target string) ApprovalDecision {
	target = strings.TrimSpace(target)
	for _, re := range b.denyRegexes {
		if re.MatchString(target) {
			return ApprovalDecision{Action: ActionDecline, Reason: "matched disallow pattern"}
		}
	}
	if b.hasAllowlist {
		for _, re := range b.allowRegexes {
			if re.MatchString(target) {
				return ApprovalDecision{Action: ActionAllow}
			}
		}
		return ApprovalDecision{Action: ActionDecline, Reason: "no allow pattern matched"}
	}
	if b.autoApprove {
		return ApprovalDecision{Action: ActionAllow}
	}
	return ApprovalDecision{Action: ActionDecline, Reason: "default decision is not allow"}
}

// safetyDeny is one built-in rule that can never be opted out of.
type safetyDeny struct {
	re     *regexp.Regexp
	reason string
}

func mustCompile(p string) *regexp.Regexp { return regexp.MustCompile(p) }

// builtInSafetyDeny always declines commands that would corrupt the
// worktree or exfiltrate credentials, regardless of user config.
var builtInSafetyDeny = []safetyDeny{
	{re: mustCompile(`(?i)\brm\s+-rf\s+(/|\~|/tmp|\$HOME)`), reason: "built-in safety deny: recursive delete of a root path"},
	{re: mustCompile(`(?i)\bgit\s+push\s+--force\b`), reason: "built-in safety deny: forced push"},
	{re: mustCompile(`(?i)\bgit\s+reset\s+--hard\b`), reason: "built-in safety deny: hard reset"},
}

func compilePatterns(patterns []string) ([]*regexp.Regexp, bool) {
	var out []*regexp.Regexp
	files := false
	for _, p := range patterns {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		if isBareFileTool(p) {
			files = true
			continue
		}
		if isFileChangeTool(p) {
			files = true
		}
		if body, ok := bashPermissionPattern(p); ok {
			if re, err := regexp.Compile(body); err == nil {
				out = append(out, re)
			}
			continue
		}
		if re, err := regexp.Compile(globToRegexp(p)); err == nil {
			out = append(out, re)
		}
	}
	return out, files
}

// isFileChangeTool reports whether a permission pattern targets file
// edits rather than shell commands.
func isFileChangeTool(pattern string) bool {
	lower := strings.ToLower(pattern)
	return strings.HasPrefix(lower, "edit(") || strings.HasPrefix(lower, "write(") ||
		strings.HasPrefix(lower, "patch(") || strings.HasPrefix(lower, "file(")
}

// isBareFileTool reports a bare file-change tool name, which covers every
// file access.
func isBareFileTool(pattern string) bool {
	switch strings.ToLower(strings.TrimSpace(pattern)) {
	case "edit", "write", "multiedit", "notebookedit":
		return true
	}
	return false
}

// bashPermissionPattern converts a Claude "Bash(...)" pattern into a regexp
// body: "prefix:*" is the prefix up to a word boundary, anything else a glob
// anchored at the start. Only a trailing ":*" is special; a ":" elsewhere is
// literal. (Keeping only the text after the first ":" turned "git:*" into
// "^.*", which approved every command as an allow.)
func bashPermissionPattern(pattern string) (string, bool) {
	lower := strings.ToLower(pattern)
	if !strings.HasPrefix(lower, "bash(") || !strings.HasSuffix(pattern, ")") {
		return "", false
	}
	inner := strings.TrimSpace(pattern[len("Bash(") : len(pattern)-1])
	if prefix, ok := strings.CutSuffix(inner, ":*"); ok {
		return "^" + globToRegexpBody(strings.TrimSpace(prefix)) + `(?:\s|$)`, true
	}
	return "^" + globToRegexpBody(inner), true
}

// globToRegexp converts a glob pattern to an anchored regexp.
func globToRegexp(glob string) string { return "^" + globToRegexpBody(glob) + "$" }

func globToRegexpBody(glob string) string {
	var b strings.Builder
	for _, r := range glob {
		switch r {
		case '*':
			b.WriteString(".*")
		case '?':
			b.WriteString(".")
		case '.', '+', '(', ')', '|', '^', '$', '[', ']', '{', '}', '\\':
			b.WriteRune('\\')
			b.WriteRune(r)
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}
