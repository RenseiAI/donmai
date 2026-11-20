package codex

import (
	"context"
	"encoding/json"
	"io"
	"math"
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
	reads  atomic.Int64
}

func startQuotaProbeFake(t *testing.T, read map[string]any, refuse bool) (*Provider, *atomic.Int64, func()) {
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
				fs.reads.Add(1)
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
	return p, &fs.reads, func() {
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
	p, _, cleanup := startQuotaProbeFake(t, read, false)
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

	p, _, cleanup := startQuotaProbeFake(t, nil, true)
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
func TestProbeQuotaPreservesRawEvidence(t *testing.T) {
	t.Parallel()
	full := map[string]any{"accountId": "acct-evidence-1", "rateLimits": map[string]any{"limitId": "codex", "planType": "promax", "primary": map[string]any{"usedPercent": 12.5, "resetsAt": float64(1791646860), "windowDurationMins": 300, "limit": float64(100)}, "secondary": map[string]any{"usedPercent": 62.5, "usedTokens": float64(1250), "limit": float64(2000), "resetsAt": float64(1791646860), "windowDurationMins": 10080}}}
	p, reads, cleanup := startQuotaProbeFake(t, full, false)
	defer cleanup()
	account, plan, probed := p.ProbeQuota(t.Context())
	if reads.Load() != 1 {
		t.Fatalf("reads=%d want exactly one quota read per probe", reads.Load())
	}
	if probed.Unavailable != nil {
		t.Fatalf("probed=%+v want windows", probed.Unavailable)
	}
	if account != "acct-evidence-1" || plan != "promax" {
		t.Fatalf("identity=%q/%q want acct-evidence-1/promax", account, plan)
	}
	if len(probed.Evidence) != 2 {
		t.Fatalf("evidence=%+v want primary+secondary", probed.Evidence)
	}
	prim, sec := probed.Evidence[0], probed.Evidence[1]
	if prim.Window != "primary" || sec.Window != "secondary" {
		t.Fatalf("order=%+v want primary then secondary", probed.Evidence)
	}
	if prim.Provider != "codex" || prim.Plan != "promax" || prim.Allowance != "codex" || prim.Account != "acct-evidence-1" || prim.Unit != agent.UsageUnitPercent || prim.Used != 12.5 || prim.Limit == nil || *prim.Limit != 100 || prim.ResetsAt == nil || prim.DurationMins == nil || *prim.DurationMins != 300 {
		t.Errorf("primary=%+v want codex/promax/codex/acct percent 12.5 limit 100", prim)
	}
	if sec.Provider != "codex" || sec.Allowance != "codex" || sec.Window != "secondary" || sec.Unit != agent.UsageUnitTokens || sec.Used != 1250 || sec.Limit == nil || *sec.Limit != 2000 || sec.ResetsAt == nil || sec.DurationMins == nil || *sec.DurationMins != 10080 {
		t.Errorf("secondary=%+v want codex bucket tokens 1250 limit 2000 (clamp fails)", sec)
	}
	bare := map[string]any{"accountId": "acct-evidence-1", "rateLimits": map[string]any{"limitId": "codex", "planType": "promax", "primary": map[string]any{"usedPercent": 12.5}, "secondary": map[string]any{"usedPercent": 62.5, "usedTokens": float64(1250)}}}
	p2, reads2, cleanup2 := startQuotaProbeFake(t, bare, false)
	defer cleanup2()
	_, _, probed2 := p2.ProbeQuota(t.Context())
	if reads2.Load() != 1 {
		t.Fatalf("bare reads=%d want exactly one quota read per probe", reads2.Load())
	}
	if len(probed2.Evidence) != 2 || probed2.Evidence[1].Limit != nil || probed2.Evidence[1].ResetsAt != nil || probed2.Evidence[1].DurationMins != nil {
		t.Errorf("bare secondary=%+v want absent limit reset duration (nil, not zero)", probed2.Evidence)
	}
	if len(probed2.Windows) != 2 || probed2.Windows[0].WindowDurationMins == nil {
		t.Errorf("display=%+v want fallback durations despite absent evidence", probed2.Windows)
	}
}

func TestProbeQuotaPerReadTotalsSeatAndSerialization(t *testing.T) {
	t.Parallel()
	mk := func(tokens float64, scope, user string) map[string]any {
		return map[string]any{"accountId": "acct-shared-1", "rateLimits": map[string]any{"limitId": "codex", "planType": "promax", "secondary": map[string]any{"usedPercent": 62.5, "usedTokens": tokens, "limit": float64(2000), "windowDurationMins": 10080}, "scope": scope, "userId": user}}
	}
	p1, _, c1 := startQuotaProbeFake(t, mk(450, "", ""), false)
	defer c1()
	_, _, first := p1.ProbeQuota(t.Context())
	p2, _, c2 := startQuotaProbeFake(t, mk(725, "", ""), false)
	defer c2()
	_, _, second := p2.ProbeQuota(t.Context())
	if len(first.Evidence) != 1 || first.Evidence[0].Used != 450 || first.Evidence[0].Account != "acct-shared-1" {
		t.Fatalf("first=%+v want own total 450", first.Evidence)
	}
	if len(second.Evidence) != 1 || second.Evidence[0].Used != 725 {
		t.Fatalf("second=%+v want own total 725 not 1175", second.Evidence)
	}
	pa, _, ca := startQuotaProbeFake(t, mk(10, "seat", "user-a"), false)
	defer ca()
	pb, _, cb := startQuotaProbeFake(t, mk(10, "seat", "user-b"), false)
	defer cb()
	_, _, la := pa.ProbeQuota(t.Context())
	_, _, lb := pb.ProbeQuota(t.Context())
	if la.Evidence[0].User == lb.Evidence[0].User || la.Evidence[0].User != "user-a" {
		t.Fatalf("seat users=%q/%q want distinct user-a/user-b", la.Evidence[0].User, lb.Evidence[0].User)
	}
	pacct, _, cacct := startQuotaProbeFake(t, mk(10, "account", "user-a"), false)
	defer cacct()
	_, _, lacc := pacct.ProbeQuota(t.Context())
	if lacc.Evidence[0].User != "" {
		t.Fatalf("account-scoped user=%q want dropped", lacc.Evidence[0].User)
	}
	raw, _ := json.Marshal(first)
	rawLegacy, _ := json.Marshal(agent.MakeUsageLimits(first.CheckedAt, first.Windows))
	if string(raw) != string(rawLegacy) {
		t.Fatal("wire differs from legacy rows; evidence must be excluded")
	}
	var wire map[string]any
	_ = json.Unmarshal(raw, &wire)
	if _, ok := wire["Evidence"]; ok || func() bool { _, ok := wire["evidence"]; return ok }() {
		t.Fatal("wire carries evidence; want it excluded")
	}
}

func TestProbeQuotaBillingModes(t *testing.T) {
	t.Parallel()
	fptr := func(v float64) *float64 { return &v }
	for _, tc := range []struct {
		name string
		mode *string
		bal  *float64
		want agent.UsageBillingMode
	}{
		{name: "allowance", mode: strPtr("allowance"), want: agent.UsageBillingAllowance},
		{name: "credits", mode: strPtr("credits"), bal: fptr(5), want: agent.UsageBillingCredits},
		{name: "overage", mode: strPtr("overage"), want: agent.UsageBillingOverage},
		{name: "api", mode: strPtr("api"), want: agent.UsageBillingAPI},
		{name: "missing", want: agent.UsageBillingUnknown},
		{name: "unrecognized", mode: strPtr("wizard"), bal: fptr(5), want: agent.UsageBillingUnknown},
		{name: "zero", mode: strPtr("credits"), bal: fptr(0), want: agent.UsageBillingCredits},
		{name: "absent", mode: strPtr("credits"), want: agent.UsageBillingCredits},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			snap := map[string]any{"limitId": "codex", "planType": "promax", "primary": map[string]any{"usedPercent": 10, "windowDurationMins": 300}}
			if tc.mode != nil {
				snap["billingMode"] = *tc.mode
			}
			if tc.bal != nil {
				snap["creditBalance"] = *tc.bal
			}
			read := map[string]any{"accountId": "acct-bill-1", "rateLimits": snap}
			p, _, cleanup := startQuotaProbeFake(t, read, false)
			t.Cleanup(cleanup)
			_, _, probed := p.ProbeQuota(t.Context())
			ev := probed.Evidence[0]
			if ev.Billing != tc.want || (tc.bal == nil) != (ev.Balance == nil) || (tc.bal != nil && *ev.Balance != *tc.bal) {
				t.Errorf("billing=%q/%v want %q/%v", ev.Billing, ev.Balance, tc.want, tc.bal)
			}
		})
	}
	// Reset credits alone never establish a monetary balance or prepaid mode.
	p, _, cleanup := startQuotaProbeFake(t, map[string]any{"accountId": "acct-bill-1", "rateLimits": map[string]any{"limitId": "codex", "planType": "promax", "primary": map[string]any{"usedPercent": 10, "windowDurationMins": 300}}, "rateLimitResetCredits": map[string]any{"availableCount": 2}}, false)
	t.Cleanup(cleanup)
	_, _, probed := p.ProbeQuota(t.Context())
	if probed.Evidence[0].Billing != agent.UsageBillingUnknown || probed.Evidence[0].Balance != nil {
		t.Errorf("reset-credits evidence=%+v want unknown with no balance", probed.Evidence[0])
	}
	if probed.ResetCredits == nil || probed.ResetCredits.AvailableCount != 2 {
		t.Errorf("reset credits=%+v want count 2 riding along", probed.ResetCredits)
	}
}

// TestProbeQuotaEvidenceGuards pins the two evidence fences at the
// production entry point and mapper: a non-main allowance never yields
// rows or evidence, and a non-finite token amount never yields an
// evidence row.
func TestProbeQuotaEvidenceGuards(t *testing.T) {
	t.Parallel()
	spark := map[string]any{"accountId": "acct-guards-1", "rateLimits": map[string]any{"limitId": "spark", "planType": "plus", "primary": map[string]any{"usedPercent": 90, "windowDurationMins": 300}, "secondary": map[string]any{"usedPercent": 90, "windowDurationMins": 10080}}}
	p, _, cleanup := startQuotaProbeFake(t, spark, false)
	t.Cleanup(cleanup)
	_, _, probed := p.ProbeQuota(t.Context())
	if len(probed.Windows) != 0 {
		t.Errorf("spark windows=%+v want no rows (non-main allowance never counts)", probed.Windows)
	}
	if len(probed.Evidence) != 0 {
		t.Errorf("spark evidence=%+v want none (LimitID guard keeps the main bucket only)", probed.Evidence)
	}
	nan := math.NaN()
	inf := math.Inf(1)
	for _, tc := range []struct {
		name   string
		tokens float64
	}{
		{name: "nan", tokens: nan},
		{name: "inf", tokens: inf},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			toks := tc.tokens
			got := RateLimitsToLimits(&RateLimitSnapshot{
				LimitID:   strPtr("codex"),
				PlanType:  strPtr("promax"),
				Primary:   &RateLimitWindow{UsedPercent: 10, UsedTokens: &toks, WindowDurationMins: minsPtr(300)},
				Secondary: &RateLimitWindow{UsedPercent: 20, WindowDurationMins: minsPtr(10080)},
			}, nil, nil, rateLimitsTestCheckedAt)
			if len(got.Windows) != 2 {
				t.Fatalf("windows=%+v want both display rows (guard only drops evidence)", got.Windows)
			}
			if len(got.Evidence) != 1 || got.Evidence[0].Window != "secondary" {
				t.Fatalf("evidence=%+v want only the secondary row (non-finite tokens skipped)", got.Evidence)
			}
		})
	}
}

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
