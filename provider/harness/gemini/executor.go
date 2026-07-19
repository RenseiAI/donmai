package gemini

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	runtimeenv "github.com/RenseiAI/donmai/runtime/env"
)

// toolExecutor runs a single Gemini functionCall and returns the
// functionResponse payload (the "response" object the model receives back).
//
// Gemini's REST endpoint — unlike Claude's CLI or Codex's app-server —
// does NOT execute tools. It returns functionCall parts and expects the
// caller to run the tool and POST a matching functionResponse. Without an
// executor an autonomous session deadlocks: the driver pauses on the
// functionCall and nothing ever delivers the result.
//
// This is the session-local executor. It runs the native filesystem /
// shell tools (Read, Edit, Write, Bash and their lower-cased variants)
// inside the session's working directory and folds the result straight
// back into the conversation loop, so an autonomous Gemini session
// completes without the runner having to dispatch + Inject every call.
//
// MCP tools (name prefixed "mcp__") route through the session's mcpBridge
// (mcp.go): the bridge dialed the declared Spec.MCPServers at Spawn and
// forwards the call to the live server over the runtime/mcp client. When
// the session declared no servers — or the named server failed to connect —
// the call resolves to a structured error functionResponse so the model can
// recover (retry with a native tool, or report the limitation) rather than
// hanging forever.
type toolExecutor struct {
	// cwd is the working directory native tools run in (the runner's
	// worktree). Empty falls back to the process working directory.
	cwd string
	// env is the per-session environment forwarded to Bash invocations.
	env map[string]string
	// mcp routes mcp__* calls to the live servers. nil when the session
	// declared no MCP servers.
	mcp *mcpBridge
}

// newToolExecutor builds an executor from the spawn spec. nil-safe: a
// zero Spec yields an executor that runs in the process cwd with no
// extra env and no MCP routing.
func newToolExecutor(cwd string, env map[string]string, bridge *mcpBridge) *toolExecutor {
	return &toolExecutor{cwd: cwd, env: env, mcp: bridge}
}

// execResult is the outcome of running one tool. response is the object
// folded into the functionResponse "response" field; isError marks the
// call as failed for the surfaced ToolResultEvent. text is a flat
// human-readable rendering for the event stream.
type execResult struct {
	response map[string]any
	text     string
	isError  bool
}

// execute runs one function call and returns its result. It never
// returns a Go error — a failed tool resolves to an execResult with
// isError=true so the model receives a functionResponse and can recover.
func (e *toolExecutor) execute(ctx context.Context, call candidateFuncCall) execResult {
	name := call.Name

	// MCP tools route through the session's bridge to the live server.
	if strings.HasPrefix(name, "mcp__") {
		return e.execMCP(ctx, call)
	}

	switch strings.ToLower(name) {
	case "bash", "shell":
		return e.runBash(ctx, call.Args)
	case "read", "view", "cat":
		return e.runRead(call.Args)
	case "write", "create":
		return e.runWrite(call.Args)
	case "edit", "str_replace", "strreplace":
		return e.runEdit(call.Args)
	default:
		return e.errResult(fmt.Sprintf(
			"tool %q is not implemented by the Gemini native runner's session-local executor (supported: Bash, Read, Write, Edit)",
			name,
		))
	}
}

// execMCP routes one mcp__* functionCall to its live server via the
// session's bridge. Failures — no bridge, unroutable name, unavailable
// server, transport error — resolve to a structured error functionResponse
// the model can recover from; a tool-side failure (isError result) keeps
// the server's own error text.
func (e *toolExecutor) execMCP(ctx context.Context, call candidateFuncCall) execResult {
	if e.mcp == nil {
		return e.errResult(fmt.Sprintf(
			"MCP tool %q is not executable: the session declared no MCP servers. Use a native tool (Bash/Read/Edit/Write) or report the limitation.",
			call.Name,
		))
	}
	res, err := e.mcp.call(ctx, call.Name, call.Args)
	if err != nil {
		return e.errResult(fmt.Sprintf("MCP tool %q: %v", call.Name, err))
	}
	if res.IsError {
		return execResult{
			response: map[string]any{"error": res.Content},
			text:     res.Content,
			isError:  true,
		}
	}
	return execResult{
		response: map[string]any{"output": res.Content},
		text:     res.Content,
	}
}

// runBash executes a shell command in the session cwd. The "command"
// arg is required; "cwd" optionally overrides the working directory for
// this one call (still resolved under the session cwd).
func (e *toolExecutor) runBash(ctx context.Context, args map[string]any) execResult {
	command := stringArg(args, "command")
	if command == "" {
		command = stringArg(args, "cmd")
	}
	if strings.TrimSpace(command) == "" {
		return e.errResult("Bash: missing required \"command\" argument")
	}

	cmd := exec.CommandContext(ctx, "sh", "-c", command) //nolint:gosec // autonomous agent shell tool; sandbox/trust is enforced upstream by the runner
	cmd.Dir = e.resolveDir(stringArg(args, "cwd"))
	cmd.Env = e.processEnv()

	out, err := cmd.CombinedOutput()
	output := string(out)
	if err != nil {
		exitCode := -1
		var ee *exec.ExitError
		if asExitError(err, &ee) {
			exitCode = ee.ExitCode()
		}
		return execResult{
			response: map[string]any{"output": output, "exitCode": exitCode, "error": err.Error()},
			text:     output,
			isError:  true,
		}
	}
	return execResult{
		response: map[string]any{"output": output, "exitCode": 0},
		text:     output,
	}
}

// runRead returns the contents of a file. Accepts "path" or "file_path".
func (e *toolExecutor) runRead(args map[string]any) execResult {
	path := firstStringArg(args, "path", "file_path", "filePath", "file")
	if path == "" {
		return e.errResult("Read: missing required \"path\" argument")
	}
	resolved := e.resolvePath(path)
	data, err := os.ReadFile(resolved) //nolint:gosec // path resolved under session cwd; autonomous agent file tool
	if err != nil {
		return e.errResult(fmt.Sprintf("Read %s: %v", path, err))
	}
	content := string(data)
	return execResult{
		response: map[string]any{"content": content},
		text:     content,
	}
}

// runWrite writes content to a file, creating parent directories.
// Accepts "path"/"file_path" and "content".
func (e *toolExecutor) runWrite(args map[string]any) execResult {
	path := firstStringArg(args, "path", "file_path", "filePath", "file")
	if path == "" {
		return e.errResult("Write: missing required \"path\" argument")
	}
	content := stringArg(args, "content")
	resolved := e.resolvePath(path)
	if dir := filepath.Dir(resolved); dir != "" {
		if err := os.MkdirAll(dir, 0o755); err != nil { //nolint:gosec // agent-created project dirs
			return e.errResult(fmt.Sprintf("Write %s: mkdir: %v", path, err))
		}
	}
	if err := os.WriteFile(resolved, []byte(content), 0o644); err != nil { //nolint:gosec // agent-authored project file
		return e.errResult(fmt.Sprintf("Write %s: %v", path, err))
	}
	return execResult{
		response: map[string]any{"output": fmt.Sprintf("wrote %d bytes to %s", len(content), path)},
		text:     fmt.Sprintf("wrote %d bytes to %s", len(content), path),
	}
}

// runEdit applies a single string replacement to a file. Accepts
// "path"/"file_path", "old_string"/"old", and "new_string"/"new".
func (e *toolExecutor) runEdit(args map[string]any) execResult {
	path := firstStringArg(args, "path", "file_path", "filePath", "file")
	if path == "" {
		return e.errResult("Edit: missing required \"path\" argument")
	}
	oldStr := firstStringArg(args, "old_string", "oldString", "old")
	newStr := firstStringArg(args, "new_string", "newString", "new")
	if oldStr == "" {
		return e.errResult("Edit: missing required \"old_string\" argument")
	}

	resolved := e.resolvePath(path)
	data, err := os.ReadFile(resolved) //nolint:gosec // path resolved under session cwd
	if err != nil {
		return e.errResult(fmt.Sprintf("Edit %s: %v", path, err))
	}
	body := string(data)
	count := strings.Count(body, oldStr)
	if count == 0 {
		return e.errResult(fmt.Sprintf("Edit %s: old_string not found", path))
	}
	if count > 1 {
		return e.errResult(fmt.Sprintf("Edit %s: old_string is not unique (%d matches); include more context", path, count))
	}
	updated := strings.Replace(body, oldStr, newStr, 1)
	if err := os.WriteFile(resolved, []byte(updated), 0o644); err != nil { //nolint:gosec // agent-authored project file
		return e.errResult(fmt.Sprintf("Edit %s: write: %v", path, err))
	}
	return execResult{
		response: map[string]any{"output": fmt.Sprintf("edited %s (1 replacement)", path)},
		text:     fmt.Sprintf("edited %s (1 replacement)", path),
	}
}

// errResult builds a failed execResult carrying the message as the
// functionResponse error so the model can recover.
func (e *toolExecutor) errResult(msg string) execResult {
	return execResult{
		response: map[string]any{"error": msg},
		text:     msg,
		isError:  true,
	}
}

// resolveDir resolves an optional per-call directory under the session
// cwd. Empty override returns the session cwd (or process cwd when unset).
func (e *toolExecutor) resolveDir(override string) string {
	if override == "" {
		return e.cwd
	}
	return e.resolvePath(override)
}

// resolvePath resolves a tool-supplied path. Absolute paths pass
// through; relative paths join onto the session cwd.
func (e *toolExecutor) resolvePath(path string) string {
	if filepath.IsAbs(path) || e.cwd == "" {
		return path
	}
	return filepath.Join(e.cwd, path)
}

// processEnv folds the per-session env onto the process environment for Bash
// invocations while keeping runner-only attach controls on the host side.
func (e *toolExecutor) processEnv() []string {
	return runtimeenv.ComposeChildEnv(os.Environ(), e.env)
}

// stringArg reads a string-valued arg from a functionCall args map.
// Non-string values are rendered with fmt for resilience against models
// that pass numbers/bools.
func stringArg(args map[string]any, key string) string {
	v, ok := args[key]
	if !ok {
		return ""
	}
	if s, ok := v.(string); ok {
		return s
	}
	return fmt.Sprintf("%v", v)
}

// firstStringArg returns the first non-empty string arg among the keys.
func firstStringArg(args map[string]any, keys ...string) string {
	for _, k := range keys {
		if s := stringArg(args, k); s != "" {
			return s
		}
	}
	return ""
}

// asExitError reports whether err is (or wraps) an *exec.ExitError and
// captures it. Thin wrapper so runBash stays readable.
func asExitError(err error, target **exec.ExitError) bool {
	if ee, ok := err.(*exec.ExitError); ok {
		*target = ee
		return true
	}
	return false
}
