package gemini

import (
	"path/filepath"
	"strings"

	"github.com/RenseiAI/donmai/agent"
)

// toolPolicy enforces the Claude-shaped Spec policy at the in-box execution
// boundary. Function declarations alone are not authorization: the REST API
// asks the caller to execute tools, so this check must run immediately before
// Bash/filesystem/MCP dispatch.
type toolPolicy struct {
	allowed    []geminiToolPattern
	disallowed []geminiToolPattern
}

type geminiToolPattern struct {
	raw      string
	toolGlob string
	argGlob  string
}

func newToolPolicy(spec agent.Spec) *toolPolicy {
	policy := &toolPolicy{}
	for _, raw := range spec.AllowedTools {
		if pattern, ok := parseGeminiToolPattern(raw); ok {
			policy.allowed = append(policy.allowed, pattern)
		}
	}
	for _, raw := range spec.MCPToolNames {
		if pattern, ok := parseGeminiToolPattern(raw); ok {
			policy.allowed = append(policy.allowed, pattern)
		}
	}
	// A declared MCP server is an explicit service grant. Its discovered tool
	// names remain bounded to that server prefix.
	for _, server := range spec.MCPServers {
		if strings.TrimSpace(server.Name) == "" {
			continue
		}
		policy.allowed = append(policy.allowed, geminiToolPattern{
			raw: "mcp server " + server.Name, toolGlob: "mcp__" + sanitizeToolName(server.Name) + "__*",
		})
	}
	for _, raw := range spec.DisallowedTools {
		if pattern, ok := parseGeminiToolPattern(raw); ok {
			policy.disallowed = append(policy.disallowed, pattern)
		}
	}
	return policy
}

func (p *toolPolicy) allow(call candidateFuncCall) (bool, string) {
	for _, pattern := range p.disallowed {
		if pattern.matches(call) {
			return false, "matches disallowed pattern " + pattern.raw
		}
	}
	if len(p.allowed) == 0 {
		return false, "no allowed tool boundary was configured"
	}
	for _, pattern := range p.allowed {
		if pattern.matches(call) {
			return true, ""
		}
	}
	return false, "no allowed pattern matched"
}

func parseGeminiToolPattern(raw string) (geminiToolPattern, bool) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return geminiToolPattern{}, false
	}
	pattern := geminiToolPattern{raw: raw, toolGlob: raw}
	if open := strings.IndexByte(raw, '('); open >= 0 && strings.HasSuffix(raw, ")") {
		pattern.toolGlob = strings.TrimSpace(raw[:open])
		pattern.argGlob = strings.TrimSpace(raw[open+1 : len(raw)-1])
	}
	if pattern.toolGlob == "" {
		return geminiToolPattern{}, false
	}
	return pattern, true
}

func (p geminiToolPattern) matches(call candidateFuncCall) bool {
	tool := sanitizeToolName(call.Name)
	if !globMatchFold(p.toolGlob, tool) {
		return false
	}
	if p.argGlob == "" {
		return true
	}
	return argumentMatches(p.argGlob, toolPolicySubject(call))
}

func toolPolicySubject(call candidateFuncCall) string {
	switch strings.ToLower(call.Name) {
	case "bash", "shell":
		if command := stringArg(call.Args, "command"); command != "" {
			return strings.TrimSpace(command)
		}
		return strings.TrimSpace(stringArg(call.Args, "cmd"))
	default:
		return firstStringArg(call.Args, "path", "file_path", "filePath", "file")
	}
}

func argumentMatches(pattern, subject string) bool {
	// Claude's common Bash(git:*) spelling means the command begins with
	// `git`; map the colon separator onto a shell-token boundary.
	if strings.HasSuffix(pattern, ":*") {
		prefix := strings.TrimSpace(strings.TrimSuffix(pattern, ":*"))
		return subject == prefix || strings.HasPrefix(subject, prefix+" ")
	}
	matched, err := filepath.Match(pattern, subject)
	return err == nil && matched
}

func globMatchFold(pattern, value string) bool {
	matched, err := filepath.Match(strings.ToLower(pattern), strings.ToLower(value))
	return err == nil && matched
}
