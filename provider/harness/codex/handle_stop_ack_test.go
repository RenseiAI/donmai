package codex

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/RenseiAI/donmai/agent"
)

// TestHandle_StopReportsUnacknowledgedShutdown pins the writer-quiescence
// boundary at the production seam: a real Provider.Spawn handle whose
// shutdown RPCs (turn/interrupt, thread/unsubscribe) are rejected must
// report a non-nil Stop, even when MCP cleanup succeeds. A nil Stop is
// the runner's proof that the owned writers stopped and reaped; returning
// nil after rejected shutdown RPCs would certify an unproved shutdown as
// quiesced. Drives Provider.Spawn + Handle.Stop, not a helper.
func TestHandle_StopReportsUnacknowledgedShutdown(t *testing.T) {
	fs, stdinW, stdoutR := newFakeServer()
	const threadID = "thread-stop-rejected"
	go func() {
		dec := json.NewDecoder(fs.stdin)
		for {
			var msg map[string]any
			if err := dec.Decode(&msg); err != nil {
				return
			}
			method, _ := msg["method"].(string)
			idRaw, hasID := msg["id"]
			switch {
			case method == "initialize" && hasID:
				fs.replyOK(t, idRaw)
			case method == "config/batchWrite" && hasID:
				params, _ := msg["params"].(map[string]any)
				edits, _ := params["edits"].([]any)
				if len(edits) == 1 {
					if edit, _ := edits[0].(map[string]any); edit["keyPath"] == codexMCPConfigKeyPath {
						if value, ok := edit["value"].(map[string]any); ok {
							fs.mu.Lock()
							fs.activeMCP = value
							fs.mu.Unlock()
						}
					}
				}
				fs.write(t, map[string]any{
					"jsonrpc": "2.0", "id": idRaw,
					"result": map[string]any{"filePath": params["filePath"], "status": "ok", "version": "fake"},
				})
			case method == "config/read" && hasID:
				fs.mu.Lock()
				active := fs.activeMCP
				fs.mu.Unlock()
				fs.write(t, map[string]any{
					"jsonrpc": "2.0", "id": idRaw,
					"result": map[string]any{"config": map[string]any{codexMCPConfigKeyPath: active}, "origins": map[string]any{}},
				})
			case method == "mcpServerStatus/list" && hasID:
				fs.mu.Lock()
				active := fs.activeMCP
				fs.mu.Unlock()
				fs.write(t, map[string]any{
					"jsonrpc": "2.0", "id": idRaw,
					"result": map[string]any{"data": fakeMCPStatusData(active)},
				})
			case method == "thread/start" && hasID:
				fs.mu.Lock()
				fs.threads[threadID] = true
				fs.mu.Unlock()
				fs.write(t, map[string]any{
					"jsonrpc": "2.0",
					"id":      idRaw,
					"result":  map[string]any{"thread": map[string]any{"id": threadID}},
				})
			case method == "turn/start" && hasID:
				fs.replyOK(t, idRaw)
				// Keep the turn active: emit start notifications but
				// withhold turn/completed so Stop must interrupt a
				// live turn (the reviewer's active-turn scenario).
				fs.write(t, map[string]any{
					"jsonrpc": "2.0",
					"method":  "thread/started",
					"params":  map[string]any{"threadId": threadID, "thread": map[string]any{"id": threadID}},
				})
				fs.write(t, map[string]any{
					"jsonrpc": "2.0",
					"method":  "turn/started",
					"params":  map[string]any{"threadId": threadID, "turn": map[string]any{"id": "t1"}},
				})
			case method == "turn/interrupt" && hasID:
				fs.write(t, map[string]any{
					"jsonrpc": "2.0", "id": idRaw,
					"error": map[string]any{"code": -32000, "message": "turn still active: rejected"},
				})
			case method == "thread/unsubscribe" && hasID:
				fs.write(t, map[string]any{
					"jsonrpc": "2.0", "id": idRaw,
					"error": map[string]any{"code": -32000, "message": "unsubscribe rejected"},
				})
			case hasID:
				fs.replyOK(t, idRaw)
			}
		}
	}()
	p, err := New(Options{
		skipProcess:       true,
		stdinOverride:     stdinW,
		stdoutOverride:    stdoutR,
		verifyMCPReadback: true,
	})
	if err != nil {
		fs.close()
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() {
		_ = p.Shutdown(context.Background())
		fs.close()
	})
	if err := p.ensureStarted(); err != nil {
		t.Fatalf("ensureStarted: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	h, err := p.Spawn(ctx, agent.Spec{Prompt: "quiescence probe", Cwd: t.TempDir(), Autonomous: true})
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	// Do not drain to a terminal event: the turn is deliberately left
	// active so Stop must prove it stopped and reaped the live turn.
	stopErr := h.Stop(ctx)
	if stopErr == nil {
		t.Fatalf("Stop = nil after both shutdown RPCs were rejected; want non-nil (unproved shutdown must not certify quiesced)")
	}
	msg := stopErr.Error()
	if !strings.Contains(msg, "turn/interrupt") && !strings.Contains(msg, "thread/unsubscribe") {
		t.Fatalf("Stop error = %v, want turn/interrupt or thread/unsubscribe shutdown acknowledgement", stopErr)
	}
	if second := h.Stop(ctx); second == nil {
		t.Fatalf("second Stop = nil after rejected shutdown; idempotent Stop must keep the shutdown error")
	}
}
