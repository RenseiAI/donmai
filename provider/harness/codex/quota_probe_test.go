package codex

import (
	"context"
	"encoding/json"
	"io"
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
