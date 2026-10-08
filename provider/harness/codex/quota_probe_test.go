package codex

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/RenseiAI/donmai/agent"
)

// quotaProbeFakeServer answers the initialize handshake and the
// `account/rateLimits/read` call with a canned fixture-shaped result,
// so ProbeQuota runs end to end without a real app-server.
type quotaProbeFakeServer struct {
	stdin  *io.PipeReader
	stdout *io.PipeWriter
	read   map[string]any
}

func startQuotaProbeFake(t *testing.T, read map[string]any, refuse bool) (*Provider, func()) {
	t.Helper()
	stdinReader, stdinWriter := io.Pipe()
	stdoutReader, stdoutWriter := io.Pipe()
	fs := &quotaProbeFakeServer{stdin: stdinReader, stdout: stdoutWriter, read: read}
	go func() {
		dec := json.NewDecoder(fs.stdin)
		for {
			var msg map[string]any
			if err := dec.Decode(&msg); err != nil {
				return
			}
			method, _ := msg["method"].(string)
			idRaw, hasID := msg["id"]
			if !hasID {
				continue
			}
			switch method {
			case "initialize":
				writeQuotaProbeLine(t, fs.stdout, map[string]any{"jsonrpc": "2.0", "id": idRaw, "result": map[string]any{}})
			case "account/rateLimits/read":
				if refuse {
					writeQuotaProbeLine(t, fs.stdout, map[string]any{
						"jsonrpc": "2.0", "id": idRaw,
						"error": map[string]any{"code": -32600, "message": "authentication required to read rate limits"},
					})
				} else {
					writeQuotaProbeLine(t, fs.stdout, map[string]any{"jsonrpc": "2.0", "id": idRaw, "result": fs.read})
				}
			default:
				writeQuotaProbeLine(t, fs.stdout, map[string]any{"jsonrpc": "2.0", "id": idRaw, "result": map[string]any{}})
			}
		}
	}()
	p, err := New(Options{
		skipProcess:    true,
		stdinOverride:  stdinWriter,
		stdoutOverride: stdoutReader,
		configTempDir:  t.TempDir(),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return p, func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = p.Shutdown(ctx)
		_ = stdinReader.Close()
		_ = stdoutWriter.Close()
	}
}

func writeQuotaProbeLine(t *testing.T, w *io.PipeWriter, body any) {
	t.Helper()
	buf, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	_, _ = w.Write(append(buf, '\n'))
}

// TestProvider_ProbeQuota drives the production probe entry point:
// initialize, read, map through the shared mapper, and lift the
// account identity and plan the mapped snapshot drops.
func TestProvider_ProbeQuota(t *testing.T) {
	t.Parallel()

	read := map[string]any{
		"accountId": "fixture-account-1",
		"rateLimits": map[string]any{
			"limitId":  "codex",
			"planType": "promax",
			"primary":  map[string]any{"usedPercent": 1, "resetsAt": float64(1791646860), "windowDurationMins": 10080},
		},
	}
	p, cleanup := startQuotaProbeFake(t, read, false)
	defer cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	accountID, plan, probed := p.ProbeQuota(ctx)
	if probed.Unavailable != nil {
		t.Fatalf("probed = %+v, want windows", probed.Unavailable)
	}
	if accountID != "fixture-account-1" {
		t.Errorf("accountID = %q, want the read's accountId", accountID)
	}
	if plan != "promax" {
		t.Errorf("plan = %q, want promax", plan)
	}
	if len(probed.Windows) != 1 || probed.Windows[0].ID != "primary" || probed.Windows[0].UsedPercent != 1 {
		t.Errorf("windows = %+v, want the primary row at 1", probed.Windows)
	}
}

// TestProvider_ProbeQuotaRefusalIsAnswered pins the logged-out
// posture end to end: the app-server's refusal yields a
// probe-failed snapshot marked answered, so the daemon records
// ok:false beside the last good windows.
func TestProvider_ProbeQuotaRefusalIsAnswered(t *testing.T) {
	t.Parallel()

	p, cleanup := startQuotaProbeFake(t, nil, true)
	defer cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	accountID, plan, probed := p.ProbeQuota(ctx)
	if probed.Unavailable == nil || probed.Unavailable.Reason != agent.UsageUnavailableProbeFailed {
		t.Fatalf("probed = %+v, want probeFailed", probed.Unavailable)
	}
	if !probed.Unavailable.Answered {
		t.Error("refusal not marked answered; the app-server refused the read")
	}
	if accountID != "" || plan != "" {
		t.Errorf("identity = %q/%q, want empty when the read produced nothing", accountID, plan)
	}
}

// TestMapRateLimitsUpdated_MapsThroughSharedMapper drives the
// production stream path: one `account/rateLimits/updated`
// notification maps onto the probe-stable window id, a partial
// second notification keeps the earlier row, and a model-specific
// one maps to nothing.
func TestMapRateLimitsUpdated_MapsThroughSharedMapper(t *testing.T) {
	t.Parallel()

	state := &mapperState{}
	full := []byte(`{"rateLimits":{"limitId":"codex","planType":"plus",` +
		`"primary":{"usedPercent":12,"windowDurationMins":300},` +
		`"secondary":{"usedPercent":47,"windowDurationMins":10080}}}`)
	events := mapNotification("account/rateLimits/updated", full, state, nil)
	if len(events) != 1 {
		t.Fatalf("events = %+v, want one UsageEvent", events)
	}
	usage, ok := events[0].(agent.UsageEvent)
	if !ok || usage.Usage == nil || len(usage.Usage.Windows) != 2 {
		t.Fatalf("event = %+v, want a UsageEvent with two windows", events[0])
	}
	partial := []byte(`{"rateLimits":{"secondary":{"usedPercent":51,"windowDurationMins":10080}}}`)
	events = mapNotification("account/rateLimits/updated", partial, state, nil)
	if len(events) != 1 {
		t.Fatalf("partial events = %+v, want one UsageEvent", events)
	}
	// The partial notification omits the plan: classifying against the
	// merged snapshot keeps the rows on the probe's ids.
	seen := map[string]float64{}
	for _, w := range events[0].(agent.UsageEvent).Usage.Windows {
		seen[w.ID] = w.UsedPercent
	}
	if seen["secondary"] != 51 {
		t.Errorf("windows = %+v, want the updated secondary at 51", seen)
	}
	spark := []byte(`{"rateLimits":{"limitId":"spark","secondary":{"usedPercent":90}}}`)
	if events := mapNotification("account/rateLimits/updated", spark, state, nil); len(events) != 0 {
		t.Errorf("model-specific events = %+v, want nothing", events)
	}
	if events := mapNotification("account/rateLimits/updated", []byte(`{}`), state, nil); len(events) != 0 {
		t.Errorf("empty events = %+v, want nothing", events)
	}
}

// TestProvider_ProbeQuotaProjectsHostLogin pins the host-session half
// of the production probe: the app-server must start with the host's
// CLI login projected into its isolated home, or it has no login and
// refuses every read (a signed-in host reported as logged out). The
// fake answers like a real app-server: a refusal unless the isolated
// home carries the login file.
func TestProvider_ProbeQuotaProjectsHostLogin(t *testing.T) {
	hostHome := t.TempDir()
	if err := os.WriteFile(filepath.Join(hostHome, codexAuthFileName), []byte(`{"fixture":true}`), 0o600); err != nil {
		t.Fatalf("write host login: %v", err)
	}
	t.Setenv("CODEX_HOME", hostHome)

	stdinReader, stdinWriter := io.Pipe()
	stdoutReader, stdoutWriter := io.Pipe()
	var isolatedHome atomic.Value
	go func() {
		dec := json.NewDecoder(stdinReader)
		for {
			var msg map[string]any
			if err := dec.Decode(&msg); err != nil {
				return
			}
			idRaw, hasID := msg["id"]
			if !hasID {
				continue
			}
			reply := map[string]any{"jsonrpc": "2.0", "id": idRaw, "result": map[string]any{}}
			if msg["method"] == "account/rateLimits/read" {
				home, _ := isolatedHome.Load().(string)
				if _, err := os.Lstat(filepath.Join(home, codexAuthFileName)); home == "" || err != nil {
					reply = map[string]any{"jsonrpc": "2.0", "id": idRaw, "error": map[string]any{
						"code": -32600, "message": "codex account authentication required to read rate limits",
					}}
				} else {
					reply["result"] = map[string]any{"rateLimits": map[string]any{
						"limitId": "codex", "planType": "team",
						"primary": map[string]any{"usedPercent": 4, "windowDurationMins": 300},
					}}
				}
			}
			writeQuotaProbeLine(t, stdoutWriter, reply)
		}
	}()
	p, err := New(Options{
		skipProcess:     true,
		stdinOverride:   stdinWriter,
		stdoutOverride:  stdoutReader,
		configTempDir:   t.TempDir(),
		HostSessionAuth: true,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	isolatedHome.Store(p.config.home)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = p.Shutdown(ctx)
		_ = stdinReader.Close()
		_ = stdoutWriter.Close()
	})

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_, plan, probed := p.ProbeQuota(ctx)
	if probed.Unavailable != nil {
		t.Fatalf("probed = %+v, want windows: the probe started the app-server without the host login", probed.Unavailable)
	}
	if plan != "team" || len(probed.Windows) != 1 {
		t.Errorf("plan = %q windows = %+v, want team and one row", plan, probed.Windows)
	}
}

// TestProvider_UnhandledServerRequestStillAnswered pins the client
// fall-through the rate-limit forwarder replaces: a server request no
// thread handles is still answered with -32601, so codex never waits
// on it.
func TestProvider_UnhandledServerRequestStillAnswered(t *testing.T) {
	t.Parallel()

	stdinReader, stdinWriter := io.Pipe()
	stdoutReader, stdoutWriter := io.Pipe()
	answered := make(chan map[string]any, 1)
	go func() {
		dec := json.NewDecoder(stdinReader)
		for {
			var msg map[string]any
			if err := dec.Decode(&msg); err != nil {
				return
			}
			if msg["id"] == "server-1" {
				answered <- msg
				continue
			}
			if idRaw, hasID := msg["id"]; hasID {
				writeQuotaProbeLine(t, stdoutWriter, map[string]any{"jsonrpc": "2.0", "id": idRaw, "result": map[string]any{}})
			}
		}
	}()
	p, err := New(Options{
		skipProcess:    true,
		stdinOverride:  stdinWriter,
		stdoutOverride: stdoutReader,
		configTempDir:  t.TempDir(),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = p.Shutdown(ctx)
		_ = stdinReader.Close()
		_ = stdoutWriter.Close()
	})
	if err := p.ensureStarted(); err != nil {
		t.Fatalf("start: %v", err)
	}
	writeQuotaProbeLine(t, stdoutWriter, map[string]any{"jsonrpc": "2.0", "id": "server-1", "method": "attestation/generate", "params": map[string]any{}})
	select {
	case msg := <-answered:
		rpcErr, _ := msg["error"].(map[string]any)
		if code, _ := rpcErr["code"].(float64); code != -32601 {
			t.Errorf("answer = %+v, want a -32601 error", msg)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("unhandled server request never answered; codex would wait on it")
	}
}
