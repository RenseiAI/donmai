package muse

import (
	"context"
	"encoding/json"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/RenseiAI/donmai/agent"
)

// fakeHost is a goroutine-driven fake `muse serve` host that runs
// against the handle's stdin/stdout pipes. It implements just enough
// MSP behavior to drive a Handle through handshake → session/start →
// turn/start → events → turn/completed, plus scripted variants for
// interrupt, resume, and view/gap recovery.
type fakeHost struct {
	stdin  *io.PipeReader // host reads from here (handle's stdin write end)
	stdout *io.PipeWriter // host writes here (handle's stdout read end)

	mu         sync.Mutex
	methods    []string
	params     []map[string]any
	decided    chan struct{}
	decideOnce sync.Once
	steered    chan struct{}
	steerOnce  sync.Once

	// script selects the event sequence emitted after turn/start.
	script string
	// sessionID is the id session/start echoes back.
	sessionID string
	// denyApproval, when true, drives the approval script with a
	// deny-by-default bridge policy so the decision must carry the
	// denial choice.
	denyApproval bool
	// resumeErr, when non-nil, is the JSON-RPC error session/resume answers.
	resumeErr *rpcError
	// schemaVersion overrides the handshake schema version (0 = 1).
	schemaVersion int
	// sessionStartErr, when non-nil, is the JSON-RPC error
	// session/start answers. Test-only first-turn sync failure door.
	sessionStartErr *rpcError
	// turnStartErr, when non-nil, is the JSON-RPC error turn/start
	// answers. The handle emits no applied-effort evidence.
	// Test-only first-turn sync failure door.
	turnStartErr *rpcError
	// turnStartMalformed, when true, answers turn/start with an
	// acknowledgement that fails validation (empty turn id). The handle
	// emits no applied-effort evidence.
	turnStartMalformed bool
	// turnStartOmitStatus, when true, answers turn/start with the
	// request's command/turn id and started disposition but omits the
	// acceptance status. The handle must reject it and emit nothing.
	turnStartOmitStatus bool
	// turnStartRejected, when true, answers turn/start with a
	// non-empty turn id but status rejected. The handle must reject
	// it and emit nothing.
	turnStartRejected bool
	// turnStartTurnIDOnly, when true, answers turn/start with only
	// {"turnId":"turn-1"}. The handle must reject it and emit
	// nothing.
	turnStartTurnIDOnly bool
	// turnStartHang, when true, never answers turn/start, so the bounded
	// request times out. The handle emits no applied-effort evidence.
	turnStartHang bool
	// failedKind and failedMessage override the failed script's
	// turn/completed error object. Empty keeps the canned
	// overloaded/host-overloaded pair.
	failedKind    string
	failedMessage string
	// recordMethod and recordParams drive the "record-only" script:
	// one extra notification emitted mid-turn before the normal
	// terminal completion. "__SID__" in recordParams is replaced
	// with the live session id before sending.
	recordMethod string
	recordParams string
}

func newFakeHost() (*fakeHost, *io.PipeWriter, *io.PipeReader) {
	stdinReader, stdinWriter := io.Pipe()
	stdoutReader, stdoutWriter := io.Pipe()
	fh := &fakeHost{stdin: stdinReader, stdout: stdoutWriter, sessionID: "11111111-1111-7111-8111-111111111111", decided: make(chan struct{}), steered: make(chan struct{})}
	return fh, stdinWriter, stdoutReader
}

// write sends one JSONL message to the handle. It runs on the host's
// goroutines, never the test goroutine, so it must not FailNow; a write
// after the handle tore its pipes down is expected at teardown and is
// dropped.
func (fh *fakeHost) write(t *testing.T, body any) {
	t.Helper()
	buf, err := json.Marshal(body)
	if err != nil {
		t.Errorf("marshal: %v", err)
		return
	}
	_, _ = fh.stdout.Write(append(buf, '\n'))
}

func (fh *fakeHost) close() {
	_ = fh.stdin.Close()
	_ = fh.stdout.Close()
}

func (fh *fakeHost) replyOK(t *testing.T, id any) {
	t.Helper()
	fh.write(t, map[string]any{"jsonrpc": "2.0", "id": id, "result": map[string]any{}})
}

func (fh *fakeHost) run(t *testing.T) {
	dec := json.NewDecoder(fh.stdin)
	for {
		var msg map[string]any
		if err := dec.Decode(&msg); err != nil {
			return
		}
		method, _ := msg["method"].(string)
		idRaw, hasID := msg["id"]
		fh.mu.Lock()
		fh.methods = append(fh.methods, method)
		params, _ := msg["params"].(map[string]any)
		fh.params = append(fh.params, params)
		script := fh.script
		sessionID := fh.sessionID
		schemaVersion := fh.schemaVersion
		denyApproval := fh.denyApproval
		sessionStartErr := fh.sessionStartErr
		turnStartErr := fh.turnStartErr
		turnStartMalformed := fh.turnStartMalformed
		turnStartHang := fh.turnStartHang
		turnStartOmitStatus := fh.turnStartOmitStatus
		turnStartRejected := fh.turnStartRejected
		turnStartTurnIDOnly := fh.turnStartTurnIDOnly
		recordMethod := fh.recordMethod
		recordParams := fh.recordParams
		fh.mu.Unlock()
		if schemaVersion == 0 {
			schemaVersion = 1
		}
		switch {
		case method == "initialize" && hasID:
			fh.write(t, map[string]any{
				"jsonrpc": "2.0", "id": idRaw,
				"result": map[string]any{
					"experimentalApi":     false,
					"grantedCapabilities": []string{"sessionMcp"},
					"museHome":            "/tmp/fake-muse-home",
					"platformFamily":      "unix",
					"platformOs":          "linux",
					"schema":              map[string]any{"fingerprint": "sha256:c94c3d1f2f51e6424074b9ec43eac3eedcf532d3dcd77351ec1383a87d3049c6", "version": schemaVersion},
					"serverInfo":          map[string]any{"name": "fake", "version": "1.4.14"},
					"userAgent":           "fake",
					"sessionDurability":   "durable",
				},
			})
		case method == "session/start" && hasID:
			if sessionStartErr != nil {
				fh.write(t, map[string]any{
					"jsonrpc": "2.0", "id": idRaw,
					"error": map[string]any{
						"code": sessionStartErr.Code, "message": sessionStartErr.Message,
						"data": map[string]any{"kind": "internal"},
					},
				})
				continue
			}
			params, _ := msg["params"].(map[string]any)
			sid, _ := params["sessionId"].(string)
			if sid == "" {
				sid = sessionID
			}
			fh.write(t, map[string]any{
				"jsonrpc": "2.0", "id": idRaw,
				"result": map[string]any{
					"session":    map[string]any{"sessionId": sid, "status": "idle", "activeTurnId": nil},
					"viewCursor": "cursor-0",
				},
			})
			fh.write(t, map[string]any{
				"jsonrpc": "2.0", "method": "session/started",
				"params": map[string]any{"session": map[string]any{"sessionId": sid, "status": "idle", "activeTurnId": nil}},
			})
		case method == "session/resume" && hasID:
			fh.mu.Lock()
			resumeErr := fh.resumeErr
			fh.mu.Unlock()
			if resumeErr != nil {
				fh.write(t, map[string]any{
					"jsonrpc": "2.0", "id": idRaw,
					"error": map[string]any{
						"code": resumeErr.Code, "message": resumeErr.Message,
						"data": map[string]any{"kind": "sessionNotFound"},
					},
				})
				continue
			}
			params, _ := msg["params"].(map[string]any)
			sid, _ := params["sessionId"].(string)
			fh.write(t, map[string]any{
				"jsonrpc": "2.0", "id": idRaw,
				"result": map[string]any{
					"session":         map[string]any{"sessionId": sid, "status": "idle", "activeTurnId": nil},
					"viewCursor":      "cursor-resumed",
					"pendingRequests": []any{},
				},
			})
		case method == "turn/start" && hasID:
			if turnStartHang {
				// Never answer: the bounded request times out.
				continue
			}
			if turnStartErr != nil {
				fh.write(t, map[string]any{
					"jsonrpc": "2.0", "id": idRaw,
					"error": map[string]any{
						"code": turnStartErr.Code, "message": turnStartErr.Message,
						"data": map[string]any{"kind": "overloaded"},
					},
				})
				continue
			}
			params, _ := msg["params"].(map[string]any)
			sid, _ := params["sessionId"].(string)
			cmdID, _ := params["commandId"].(string)
			if turnStartMalformed {
				// Acknowledgement that fails validation: empty turn id.
				fh.write(t, map[string]any{
					"jsonrpc": "2.0", "id": idRaw,
					"result": map[string]any{
						"commandId": cmdID, "disposition": "started",
						"startedNewTurn": true, "status": "accepted", "turnId": "",
					},
				})
				continue
			}
			if turnStartTurnIDOnly {
				// Minimal malformed acknowledgement: turn id only.
				fh.write(t, map[string]any{
					"jsonrpc": "2.0", "id": idRaw,
					"result": map[string]any{"turnId": "turn-1"},
				})
				continue
			}
			if turnStartOmitStatus {
				// Missing acceptance status with a valid turn id.
				fh.write(t, map[string]any{
					"jsonrpc": "2.0", "id": idRaw,
					"result": map[string]any{
						"commandId": cmdID, "disposition": "started",
						"startedNewTurn": true, "turnId": cmdID,
					},
				})
				continue
			}
			if turnStartRejected {
				// Rejected acceptance status with a valid turn id.
				fh.write(t, map[string]any{
					"jsonrpc": "2.0", "id": idRaw,
					"result": map[string]any{
						"commandId": cmdID, "disposition": "started",
						"startedNewTurn": true, "status": "rejected", "turnId": cmdID,
					},
				})
				continue
			}
			// The returned turnId equals the commandId.
			fh.write(t, map[string]any{
				"jsonrpc": "2.0", "id": idRaw,
				"result": map[string]any{
					"commandId": cmdID, "disposition": "started",
					"startedNewTurn": true, "status": "accepted", "turnId": cmdID,
				},
			})
			fh.emitScript(t, script, sid, cmdID, denyApproval, recordMethod, recordParams)
		case method == "turn/interrupt" && hasID:
			fh.replyOK(t, idRaw)
		case method == "turn/steer" && hasID:
			fh.replyOK(t, idRaw)
			fh.steerOnce.Do(func() { close(fh.steered) })
		case method == "approval/decide" && hasID:
			fh.decideOnce.Do(func() { close(fh.decided) })
			params, _ := msg["params"].(map[string]any)
			approvalID, _ := params["approvalId"].(string)
			cmdID, _ := params["commandId"].(string)
			fh.write(t, map[string]any{
				"jsonrpc": "2.0", "id": idRaw,
				"result": map[string]any{
					"approvalId": approvalID, "commandId": cmdID,
					"status": "accepted", "terminal": true,
				},
			})
		case method == "session/rename" && hasID:
			fh.replyOK(t, idRaw)
		case hasID:
			fh.replyOK(t, idRaw)
		default:
			// Notification from client (initialized) — no reply.
		}
	}
}

// emitScript plays the canned notification sequence for a script name.
// denyApproval switches the approval script to a deny-by-default bridge
// policy with an abort-arm choice set (the live shell shape). The
// failed script honors fh.failedKind/failedMessage when set (empty
// keeps the canned overloaded pair) so tests can plant host error text
// without touching the shared scripts.
func (fh *fakeHost) emitScript(t *testing.T, script, sid, turnID string, denyApproval bool, recordMethod, recordParams string) {
	fh.mu.Lock()
	failedKind := fh.failedKind
	failedMessage := fh.failedMessage
	fh.mu.Unlock()
	t.Helper()
	completed := func(terminal string) {
		fh.write(t, map[string]any{
			"jsonrpc": "2.0", "method": "turn/completed",
			"params": map[string]any{
				"sessionId": sid, "terminal": terminal, "turnId": turnID,
				"viewCursor": "cursor-end",
				"usage":      map[string]any{"cachedTokens": 10, "inputTokens": 100, "outputTokens": 50, "reasoningTokens": 5},
			},
		})
	}
	agentMsg := func(id, text string, rev int) {
		fh.write(t, map[string]any{
			"jsonrpc": "2.0", "method": "item/completed",
			"params": map[string]any{
				"sessionId": sid, "viewCursor": "cursor-" + id,
				"item": map[string]any{"itemId": id, "kind": "agentMessage", "revision": rev, "status": "completed", "text": text, "turnId": turnID},
			},
		})
	}
	switch script {
	case "", "single-turn":
		fh.write(t, map[string]any{
			"jsonrpc": "2.0", "method": "turn/started",
			"params": map[string]any{"sessionId": sid, "turnId": turnID, "viewCursor": "cursor-1"},
		})
		fh.write(t, map[string]any{
			"jsonrpc": "2.0", "method": "session/tokenUsage",
			"params": map[string]any{
				"sessionId": sid, "turnId": turnID, "viewCursor": "cursor-2",
				"modelId":      "muse-wonder-1.0",
				"usage":        map[string]any{"cachedTokens": 10, "inputTokens": 100, "outputTokens": 50, "reasoningTokens": 5},
				"promptTokens": 100, "totalTokens": 150,
				"cumulative": map[string]any{
					"outputTokens": 50, "promptTokens": 100, "totalTokens": 150,
					"cost": map[string]any{"partial": false, "usd": 0.0123},
				},
			},
		})
		agentMsg("item-1", "hello world", 1)
		completed("completed")
	case "streaming-text":
		notify := func(method string, params map[string]any) {
			params["sessionId"] = sid
			params["viewCursor"] = "cursor-stream"
			fh.write(t, map[string]any{"jsonrpc": "2.0", "method": method, "params": params})
		}
		for _, kind := range []string{"agentMessage", "reasoning"} {
			snapshot := func(method string, revision int, text string) {
				item := map[string]any{"itemId": kind, "kind": kind, "revision": revision, "text": text, "status": "inProgress"}
				if kind == "reasoning" {
					delete(item, "text")
					item["summary"] = strings.Split(text, "\n")
				}
				notify(method, map[string]any{"item": item})
			}
			snapshot("item/started", 1, "first")
			field := "text"
			if kind == "reasoning" {
				field = "summary.0"
			}
			notify("item/delta", map[string]any{"itemId": kind, "field": field, "delta": " second"})
			snapshot("item/updated", 2, "first second third")
			snapshot("item/updated", 1, "stale")
			notify("item/delta", map[string]any{"itemId": kind, "field": "output", "delta": "ignored"})
			tail := "first second third last"
			if kind == "reasoning" {
				notify("item/delta", map[string]any{"itemId": kind, "field": "summary.1", "delta": "next"})
				tail = "first second third\nnext last"
			}
			snapshot("item/completed", 3, tail)
			notify("item/delta", map[string]any{"itemId": kind, "delta": "late"})
		}
		agentMsg("final", "final answer", 1)
		completed("completed")
	case "step-usage":
		for i := 1; i <= 3; i++ {
			fh.write(t, map[string]any{
				"jsonrpc": "2.0", "method": "session/tokenUsage",
				"params": map[string]any{
					"sessionId": sid, "turnId": turnID, "modelId": "step-model",
					"usage":      map[string]any{"inputTokens": 100, "outputTokens": 50, "cachedTokens": 99, "cacheReadTokens": 10, "cacheWriteTokens": 4, "reasoningTokens": 5},
					"cumulative": map[string]any{"promptTokens": 100 * i, "outputTokens": 50 * i, "cacheReadTokens": 10 * i, "cacheWriteTokens": 4 * i, "cost": map[string]any{"usd": float64(i) * 0.25}},
				},
			})
		}
		fh.write(t, map[string]any{
			"jsonrpc": "2.0", "method": "turn/completed",
			"params": map[string]any{"sessionId": sid, "turnId": turnID, "terminal": "completed"},
		})
	case "multi-turn":
		fh.write(t, map[string]any{
			"jsonrpc": "2.0", "method": "turn/started",
			"params": map[string]any{"sessionId": sid, "turnId": turnID, "viewCursor": "cursor-1"},
		})
		fh.write(t, map[string]any{
			"jsonrpc": "2.0", "method": "item/started",
			"params": map[string]any{
				"sessionId": sid, "viewCursor": "cursor-2",
				"item": map[string]any{"itemId": "tool-1", "kind": "toolCall", "revision": 1, "status": "inProgress", "tool": "edit", "args": `{"path":"a.txt"}`, "turnId": turnID},
			},
		})
		fh.write(t, map[string]any{
			"jsonrpc": "2.0", "method": "item/delta",
			"params": map[string]any{"sessionId": sid, "viewCursor": "cursor-3", "itemId": "tool-1", "field": "output", "delta": "partial"},
		})
		fh.write(t, map[string]any{
			"jsonrpc": "2.0", "method": "item/completed",
			"params": map[string]any{
				"sessionId": sid, "viewCursor": "cursor-4",
				"item": map[string]any{"itemId": "tool-1", "kind": "toolCall", "revision": 2, "status": "completed", "tool": "edit", "args": `{"path":"a.txt"}`, "turnId": turnID, "visibleOutput": "edited a.txt"},
			},
		})
		agentMsg("item-2", "done", 1)
		completed("completed")
	case "interrupt":
		fh.write(t, map[string]any{
			"jsonrpc": "2.0", "method": "turn/started",
			"params": map[string]any{"sessionId": sid, "turnId": turnID, "viewCursor": "cursor-1"},
		})
		fh.write(t, map[string]any{
			"jsonrpc": "2.0", "method": "turn/completed",
			"params": map[string]any{"sessionId": sid, "terminal": "cancelled", "turnId": turnID, "viewCursor": "cursor-end", "reason": "interrupted by user"},
		})
	case "failed":
		fh.write(t, map[string]any{
			"jsonrpc": "2.0", "method": "turn/started",
			"params": map[string]any{"sessionId": sid, "turnId": turnID, "viewCursor": "cursor-1"},
		})
		kind := "overloaded"
		if failedKind != "" {
			kind = failedKind
		}
		message := "host overloaded"
		if failedMessage != "" {
			message = failedMessage
		}
		fh.write(t, map[string]any{
			"jsonrpc": "2.0", "method": "turn/completed",
			"params": map[string]any{
				"sessionId": sid, "terminal": "failed", "turnId": turnID, "viewCursor": "cursor-end",
				"error": map[string]any{"kind": kind, "message": message, "retryable": true},
			},
		})
	case "approval":
		fh.write(t, map[string]any{
			"jsonrpc": "2.0", "method": "turn/started",
			"params": map[string]any{"sessionId": sid, "turnId": turnID, "viewCursor": "cursor-1"},
		})
		// The live shell shape offers the abort arm (not denied):
		// decline must find it. The allow script keeps the
		// denied arm so both denial vocabularies are pinned.
		denyChoice := map[string]any{"choiceId": "deny", "decision": "denied", "label": "Deny", "scope": "once"}
		if denyApproval {
			denyChoice = map[string]any{"choiceId": "abort", "decision": "abort", "label": "Abort", "scope": "once"}
		}
		// Present the approval BOTH ways the host does: the
		// approval/requested notification (view fold, carries the
		// subject + choices) and the approval/request server-request
		// (presentation, answered `{}` + approval/decide).
		approvalParams := map[string]any{
			"approvalId": "appr-1", "sessionId": sid, "turnId": turnID,
			"itemId": "tool-1", "taskId": "task-1", "toolCallId": "call-1",
			"toolName": "shell", "rawArgs": "echo hi", "viewCursor": "cursor-2",
			"currentRequirementId": map[string]any{"approvalId": "appr-1", "sourceIndex": 0},
			"subject":              map[string]any{"kind": "shell", "command": "echo hi"},
			"availableChoices": []any{
				map[string]any{"choiceId": "allow", "decision": "approved", "label": "Allow", "scope": "once"},
				denyChoice,
			},
		}
		fh.write(t, map[string]any{
			"jsonrpc": "2.0", "method": "approval/requested",
			"params": approvalParams,
		})
		fh.write(t, map[string]any{
			"jsonrpc": "2.0", "method": "approval/request", "id": 999,
			"params": approvalParams,
		})
		// The completion is gated on the client's approval/decide.
		// The run loop answers decide immediately (recording it);
		// this goroutine finishes the turn once the decision
		// arrives, so emitScript never blocks the request loop.
		go func() {
			fh.waitDecided(t)
			agentMsg("item-2", "approved and done", 1)
			completed("completed")
		}()
	case "user-input":
		fh.write(t, map[string]any{
			"jsonrpc": "2.0", "method": "turn/started",
			"params": map[string]any{"sessionId": sid, "turnId": turnID, "viewCursor": "cursor-1"},
		})
		// The host asks the client for user input mid-turn. The
		// handle answers `{}` (autonomous mode has no user to
		// prompt) and records the prompt, then the turn completes.
		fh.write(t, map[string]any{
			"jsonrpc": "2.0", "method": "userInput/request", "id": 777,
			"params": map[string]any{
				"sessionId": sid, "turnId": turnID, "viewCursor": "cursor-2",
				"itemId": "tool-9", "toolCallId": "call-9", "toolName": "ask_user",
				"userInputId": "ui-1",
			},
		})
		agentMsg("item-2", "answered without a user", 1)
		completed("completed")
	case "steer":
		// The turn stays running until the client steers it, so a
		// steer can never race the turn's completion.
		fh.write(t, map[string]any{
			"jsonrpc": "2.0", "method": "turn/started",
			"params": map[string]any{"sessionId": sid, "turnId": turnID, "viewCursor": "cursor-1"},
		})
		go func() {
			select {
			case <-fh.steered:
			case <-time.After(10 * time.Second):
				return
			}
			agentMsg("item-2", "steered and done", 1)
			completed("completed")
		}()
	case "gap":
		fh.write(t, map[string]any{
			"jsonrpc": "2.0", "method": "turn/started",
			"params": map[string]any{"sessionId": sid, "turnId": turnID, "viewCursor": "cursor-1"},
		})
		fh.write(t, map[string]any{
			"jsonrpc": "2.0", "method": "view/gap",
			"params": map[string]any{"sessionId": sid, "after": "cursor-1", "next": "cursor-9"},
		})
		// After recovery (session/resume), the turn completes.
		agentMsg("item-9", "recovered", 1)
		completed("completed")
	case "stale-queued-failure":
		// The acknowledged first turn stays queued while an older
		// turn's success arrives first. The stale success must not
		// terminate the session or consume the first-turn diagnostic;
		// the queued first turn's failure then terminates with one.
		staleID := "00000000-0000-7000-8000-000000000001"
		if staleID == turnID {
			staleID = "00000000-0000-7000-8000-000000000002"
		}
		fh.write(t, map[string]any{
			"jsonrpc": "2.0", "method": "turn/started",
			"params": map[string]any{"sessionId": sid, "turnId": turnID, "viewCursor": "cursor-1"},
		})
		fh.write(t, map[string]any{
			"jsonrpc": "2.0", "method": "turn/completed",
			"params": map[string]any{
				"sessionId": sid, "terminal": "completed", "turnId": staleID, "viewCursor": "cursor-stale",
				"usage": map[string]any{"cachedTokens": 1, "inputTokens": 10, "outputTokens": 5, "reasoningTokens": 0},
			},
		})
		fh.write(t, map[string]any{
			"jsonrpc": "2.0", "method": "turn/completed",
			"params": map[string]any{
				"sessionId": sid, "terminal": "failed", "turnId": turnID, "viewCursor": "cursor-end",
				"error": map[string]any{"kind": "overloaded", "message": "host overloaded", "retryable": true},
			},
		})
	case "record-only":
		// One extra notification mid-turn, then normal terminal
		// completion. The record-only inventory test drives this
		// script once per allowlisted method.
		fh.write(t, map[string]any{
			"jsonrpc": "2.0", "method": "turn/started",
			"params": map[string]any{"sessionId": sid, "turnId": turnID, "viewCursor": "cursor-1"},
		})
		raw := strings.ReplaceAll(recordParams, "__SID__", sid)
		var decoded map[string]any
		if err := json.Unmarshal([]byte(raw), &decoded); err != nil {
			t.Errorf("record-only params are not valid JSON: %v", err)
			decoded = map[string]any{"sessionId": sid}
		}
		fh.write(t, map[string]any{
			"jsonrpc": "2.0", "method": recordMethod, "params": decoded,
		})
		completed("completed")
	}
}

func newTestProvider(t *testing.T, script string) (*Provider, *fakeHost) {
	t.Helper()
	return newTestProviderWithRPCTimeout(t, script, 0)
}

func newTestProviderWithRPCTimeout(t *testing.T, script string, rpcTimeout time.Duration) (*Provider, *fakeHost) {
	t.Helper()
	fh, stdinW, stdoutR := newFakeHost()
	fh.script = script
	go fh.run(t)
	opts := Options{skipProcess: true, stdinOverride: stdinW, stdoutOverride: stdoutR}
	if rpcTimeout > 0 {
		opts.RPCTimeout = rpcTimeout
	}
	p, err := New(opts)
	if err != nil {
		fh.close()
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() {
		_ = p.Shutdown(context.Background())
		fh.close()
	})
	return p, fh
}

func drainEvents(t *testing.T, ch <-chan agent.Event, timeout time.Duration) []agent.Event {
	t.Helper()
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	var out []agent.Event
	for {
		select {
		case ev, ok := <-ch:
			if !ok {
				return out
			}
			out = append(out, ev)
		case <-timer.C:
			t.Fatalf("timed out draining events; collected %d so far: %v", len(out), kindsOf(out))
			return out
		}
	}
}

func kindsOf(events []agent.Event) []agent.EventKind {
	out := make([]agent.EventKind, len(events))
	for i, e := range events {
		out[i] = e.Kind()
	}
	return out
}

func terminalResult(events []agent.Event) (agent.ResultEvent, bool) {
	for _, ev := range events {
		if r, ok := ev.(agent.ResultEvent); ok {
			return r, true
		}
	}
	return agent.ResultEvent{}, false
}

func (fh *fakeHost) waitDecided(t *testing.T) {
	t.Helper()
	timer := time.NewTimer(10 * time.Second)
	defer timer.Stop()
	select {
	case <-fh.decided:
	case <-timer.C:
		t.Fatalf("timed out waiting for approval/decide")
	}
}
