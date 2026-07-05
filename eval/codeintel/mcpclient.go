package codeintel

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os/exec"
	"strings"

	"github.com/RenseiAI/donmai/agent"
)

// mcpToolResult is one code-intel MCP tool call's outcome: the text content the
// server returned (the tool's JSON output) and whether it was an isError result.
type mcpToolResult struct {
	Text    string
	IsError bool
}

// callMCPTool spawns the authored af-code-intelligence stdio server (from the
// frozen MCP entry) and performs a real JSON-RPC round-trip: initialize →
// tools/call → stdin-EOF shutdown. It returns the tool's result content. This is
// the REAL v0.50.0 MCP surface — the same server a live session drives, spawned
// by the same command/args the runner authors — not a mock.
//
// The server warms its code index at startup (logged to stderr) and blocks the
// tools/call until warm, so the caller's ctx should carry a generous deadline
// for a first-index build on a large repo.
func callMCPTool(ctx context.Context, entry agent.MCPServerConfig, env []string, toolName string, args map[string]any) (mcpToolResult, error) {
	if entry.Command == "" {
		return mcpToolResult{}, fmt.Errorf("mcp entry has empty Command")
	}
	cmd := exec.CommandContext(ctx, entry.Command, entry.Args...) // nolint:gosec // command/args are the frozen af-code-intelligence entry.
	cmd.Env = env
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return mcpToolResult{}, fmt.Errorf("stdin pipe: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return mcpToolResult{}, fmt.Errorf("stdout pipe: %w", err)
	}
	// Drain stderr so warm-up logging never blocks the pipe; it is diagnostic only.
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return mcpToolResult{}, fmt.Errorf("stderr pipe: %w", err)
	}
	if err := cmd.Start(); err != nil {
		return mcpToolResult{}, fmt.Errorf("start mcp server: %w", err)
	}
	go func() { _, _ = io.Copy(io.Discard, stderr) }()

	enc := json.NewEncoder(stdin)
	writeReq := func(id int, method string, params any) error {
		return enc.Encode(map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params})
	}
	// 1) initialize. 2) tools/call.
	if err := writeReq(1, "initialize", map[string]any{"protocolVersion": "2025-03-26"}); err != nil {
		_ = cmd.Process.Kill()
		return mcpToolResult{}, fmt.Errorf("write initialize: %w", err)
	}
	if err := writeReq(2, "tools/call", map[string]any{"name": toolName, "arguments": args}); err != nil {
		_ = cmd.Process.Kill()
		return mcpToolResult{}, fmt.Errorf("write tools/call: %w", err)
	}

	// Read newline-framed responses until we see id==2 (the tools/call reply).
	res, readErr := readToolCallResponse(stdout)

	// Graceful shutdown: EOF on stdin, then wait.
	_ = stdin.Close()
	_ = cmd.Wait()

	if readErr != nil {
		return mcpToolResult{}, readErr
	}
	return res, nil
}

// readToolCallResponse scans newline-delimited JSON-RPC responses and returns the
// content of the response whose id == 2 (the tools/call).
func readToolCallResponse(r io.Reader) (mcpToolResult, error) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 1<<16), 8<<20)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var resp struct {
			ID     json.RawMessage `json:"id"`
			Result struct {
				Content []struct {
					Type string `json:"type"`
					Text string `json:"text"`
				} `json:"content"`
				IsError bool `json:"isError"`
			} `json:"result"`
			Error *struct {
				Code    int    `json:"code"`
				Message string `json:"message"`
			} `json:"error"`
		}
		if err := json.Unmarshal([]byte(line), &resp); err != nil {
			continue // ignore non-JSON noise (there should be none on stdout)
		}
		if strings.TrimSpace(string(resp.ID)) != "2" {
			continue // initialize (id 1) or other; keep reading.
		}
		if resp.Error != nil {
			return mcpToolResult{}, fmt.Errorf("mcp tools/call error %d: %s", resp.Error.Code, resp.Error.Message)
		}
		var text strings.Builder
		for _, c := range resp.Result.Content {
			text.WriteString(c.Text)
		}
		return mcpToolResult{Text: text.String(), IsError: resp.Result.IsError}, nil
	}
	if err := sc.Err(); err != nil {
		return mcpToolResult{}, fmt.Errorf("read mcp stdout: %w", err)
	}
	return mcpToolResult{}, fmt.Errorf("mcp server closed stdout before a tools/call reply")
}
