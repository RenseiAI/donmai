package opencode

import (
	"context"
	"encoding/json"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/RenseiAI/donmai/agent"
)

// ─── Permission round-trip (07 §5.2) ─────────────────────────────────────────
//
// Anything that reaches "ask" at runtime is adjudicated BY THE PROVIDER, not a
// human. Pending requests surface via GET /api/permission/request; the pump
// evaluates each against the same decision shape as
// provider/harness/codex/approval.go (built-in safety denies first, then user
// patterns, then the default decision) and replies via
// POST /api/session/:id/permission/:id/reply with once/always/reject. A
// SystemEvent{permission_request} is emitted for observability and a
// SystemEvent{permission_decision} after adjudication (the codex
// approval-bridge's surprise-surfacing posture).

// opencode reply verbs.
const (
	replyOnce   = "once"   // allow this one call
	replyAlways = "always" // PermissionSaved — persist an allow for this pattern
	replyReject = "reject" // deny
)

// permSafetyDeny mirrors provider/harness/codex/approval.go builtInSafetyDeny —
// the rules that would corrupt the worktree if a bad policy let them through.
// FIRST match wins; keep the most-specific rules first.
type permSafetyDeny struct {
	pattern *regexp.Regexp
	reason  string
}

var permBuiltInSafetyDeny = []permSafetyDeny{
	{regexp.MustCompile(`(?i)\brm\s+(-[a-zA-Z]*r[a-zA-Z]*\s+)?(-[a-zA-Z]*f[a-zA-Z]*\s+)?(/\s*$|/\s+|/\*\s*$)`), "rm of filesystem root blocked"},
	{regexp.MustCompile(`\bgit\s+worktree\s+(remove|prune)\b`), "git worktree remove/prune is reserved for the runner"},
	{regexp.MustCompile(`\bgit\s+reset\s+--hard\b`), "git reset --hard would discard work-in-progress"},
	{regexp.MustCompile(`\bgit\s+push\s+--force(\s|$)`), "git push --force without --force-with-lease blocked"},
	{regexp.MustCompile(`\b(chmod|chown)\s+-?[Rr]?\s+/\S+`), "recursive chmod/chown on absolute paths blocked"},
	{regexp.MustCompile(`(?i)\b(curl|wget)\b[^|]*\|\s*(sudo\s+)?(bash|sh|zsh|dash|ksh)\b`), "piping a download to a shell blocked"},
	{regexp.MustCompile(`(?i)\bsudo\b`), "sudo invocation blocked"},
}

// permEngine evaluates opencode permission requests. Concurrency-safe after
// construction (patterns are read-only).
type permEngine struct {
	allowRegexes []*regexp.Regexp
	denyRegexes  []*regexp.Regexp
	autoApprove  bool
	cwd          string
}

// newPermEngine compiles the Spec's permission policy. A nil PermissionConfig
// yields an engine that runs the built-in safety denies and otherwise
// auto-approves (autonomous fleets must not hang waiting for a human — the
// same default as codex's NewApprovalBridge).
func newPermEngine(spec agent.Spec) *permEngine {
	e := &permEngine{autoApprove: true, cwd: spec.Cwd}
	cfg := spec.PermissionConfig
	if cfg != nil {
		e.allowRegexes = compilePermPatterns(cfg.AllowPatterns)
		e.denyRegexes = compilePermPatterns(cfg.DisallowPatterns)
		switch strings.ToLower(cfg.DefaultDecision) {
		case "deny", "prompt":
			e.autoApprove = false
		case "allow", "":
			e.autoApprove = true
		}
	}
	return e
}

func compilePermPatterns(patterns []string) []*regexp.Regexp {
	out := make([]*regexp.Regexp, 0, len(patterns))
	for _, p := range patterns {
		// User-supplied; Compile (not MustCompile) so a bad pattern cannot
		// crash the daemon — malformed patterns are dropped.
		if re, err := regexp.Compile(p); err == nil {
			out = append(out, re)
		}
	}
	return out
}

// permDecision is the engine verdict for one request.
type permDecision struct {
	Reply  string // replyOnce | replyAlways | replyReject
	Reason string
}

// evalCommand adjudicates a shell command.
func (e *permEngine) evalCommand(cmd string) permDecision {
	cmd = strings.TrimSpace(cmd)
	if cmd == "" {
		return permDecision{Reply: replyOnce}
	}
	for _, sd := range permBuiltInSafetyDeny {
		if sd.pattern.MatchString(cmd) {
			return permDecision{Reply: replyReject, Reason: sd.reason}
		}
	}
	for _, re := range e.denyRegexes {
		if re.MatchString(cmd) {
			return permDecision{Reply: replyReject, Reason: "matches disallow pattern: " + re.String()}
		}
	}
	if len(e.allowRegexes) > 0 {
		for _, re := range e.allowRegexes {
			if re.MatchString(cmd) {
				// Static allow-pattern match → persist ("always") to cut
				// round-trip volume (§5.2 PermissionSaved).
				return permDecision{Reply: replyAlways}
			}
		}
		return permDecision{Reply: replyReject, Reason: "command not in allowed list"}
	}
	if e.autoApprove {
		return permDecision{Reply: replyOnce}
	}
	return permDecision{Reply: replyReject, Reason: "no allow pattern matched and default is deny/prompt"}
}

// evalFileChange adjudicates a file write/edit, enforcing worktree containment.
func (e *permEngine) evalFileChange(path string) permDecision {
	if e.cwd != "" && path != "" {
		clean := filepath.Clean(path)
		root := filepath.Clean(e.cwd)
		if !strings.HasPrefix(clean+string(filepath.Separator), root+string(filepath.Separator)) && clean != root {
			return permDecision{Reply: replyReject, Reason: "file change outside worktree blocked"}
		}
	}
	if strings.Contains(path, "/.git/") || strings.HasSuffix(path, "/.git") {
		return permDecision{Reply: replyReject, Reason: ".git directory modification blocked"}
	}
	if e.autoApprove {
		return permDecision{Reply: replyOnce}
	}
	return permDecision{Reply: replyReject, Reason: "default decision is deny"}
}

// Evaluate classifies an opencode permission request and returns the verdict.
// opencode requests carry an `action` (the tool/permission key) plus
// `resources` (command string or file paths) and free-form `metadata`.
func (e *permEngine) Evaluate(req permissionRequest) permDecision {
	action := strings.ToLower(req.Action)
	switch {
	case strings.Contains(action, "bash") || strings.Contains(action, "shell") || strings.Contains(action, "command"):
		return e.evalCommand(permCommandOf(req))
	case strings.Contains(action, "edit") || strings.Contains(action, "write") || strings.Contains(action, "patch"):
		return e.evalFileChange(permPathOf(req))
	default:
		// Unknown action shapes default to a single allow to avoid hangs; the
		// caller surfaces a SystemEvent so the surprise is observable.
		if e.autoApprove {
			return permDecision{Reply: replyOnce}
		}
		return permDecision{Reply: replyReject, Reason: "unknown action and default is deny"}
	}
}

// permCommandOf extracts the shell command from a request's resources /
// metadata. opencode carries the command either as the first resource or as
// metadata.command.
func permCommandOf(req permissionRequest) string {
	if len(req.Resources) > 0 && req.Resources[0] != "" {
		return req.Resources[0]
	}
	var md struct {
		Command string `json:"command"`
	}
	_ = json.Unmarshal(req.Metadata, &md)
	return md.Command
}

// permPathOf extracts the target path from a file-change request.
func permPathOf(req permissionRequest) string {
	if len(req.Resources) > 0 && req.Resources[0] != "" {
		return req.Resources[0]
	}
	var md struct {
		Path     string `json:"path"`
		FilePath string `json:"filePath"`
	}
	_ = json.Unmarshal(req.Metadata, &md)
	if md.Path != "" {
		return md.Path
	}
	return md.FilePath
}

// permPump adjudicates a session's pending permission requests. It is driven
// by the handle (on a ticker and on permission-ish SSE frames), listing
// pending requests, evaluating each, and replying. It returns the adjudicated
// records so the handle can surface observability SystemEvents. Each request
// id is replied to at most once (tracked in `done`).
type permPump struct {
	client    serverClient
	engine    *permEngine
	sessionID string
	done      map[string]bool
}

func newPermPump(client serverClient, engine *permEngine, sessionID string) *permPump {
	return &permPump{client: client, engine: engine, sessionID: sessionID, done: make(map[string]bool)}
}

// permRecord is one adjudicated request, for SystemEvent surfacing.
type permRecord struct {
	Request  permissionRequest
	Decision permDecision
}

// Adjudicate lists this session's pending permissions, replies to any not yet
// handled, and returns the records adjudicated on this call.
func (p *permPump) Adjudicate(ctx context.Context) ([]permRecord, error) {
	pending, err := p.client.PendingPermissions(ctx, p.sessionID)
	if err != nil {
		return nil, err
	}
	var records []permRecord
	for _, req := range pending {
		if p.done[req.ID] {
			continue
		}
		decision := p.engine.Evaluate(req)
		if err := p.client.RespondPermission(ctx, p.sessionID, req.ID, permissionResponse{
			Reply:   decision.Reply,
			Message: decision.Reason,
		}); err != nil {
			// Leave undone so the next tick retries.
			return records, err
		}
		p.done[req.ID] = true
		records = append(records, permRecord{Request: req, Decision: decision})
	}
	return records, nil
}
