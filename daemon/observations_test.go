package daemon

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/RenseiAI/donmai/agent"
	"github.com/RenseiAI/donmai/observation"
	providerclaude "github.com/RenseiAI/donmai/provider/harness/claude"
)

var observationNow = time.Date(2026, 11, 19, 12, 0, 0, 0, time.UTC)

func quotaAccount(harness, id string, ok *bool, used ...float64) agent.UsageAccount {
	var windows []agent.UsageWindow
	for i, u := range used {
		windows = append(windows, agent.UsageWindow{ID: []string{"primary", "secondary"}[i], Kind: agent.UsageWindowWeekly, UsedPercent: u})
	}
	account := agent.UsageAccount{ID: id, Provider: harness, Limits: agent.MakeUsageLimits("2026-11-19T11:59:00Z", windows)}
	if ok != nil {
		account.AuthCheck = &agent.UsageAuthCheck{Harness: harness, OK: *ok, CheckedAt: "2026-11-19T11:58:00Z"}
	}
	return account
}

// observedItems decodes a report's items into generic maps.
func observedItems(t *testing.T, slots []quotaSlot) ([]map[string]any, string) {
	t.Helper()
	report, err := subscriptionLoginReport(slots, observationNow, false)
	if err != nil {
		t.Fatalf("subscriptionLoginReport: %v", err)
	}
	raw, err := json.Marshal(report)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var decoded struct {
		Full  bool             `json:"full"`
		Kinds []string         `json:"kinds"`
		Items []map[string]any `json:"items"`
	}
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !decoded.Full || len(decoded.Kinds) != 1 || decoded.Kinds[0] != observation.KindSubscriptionLogin {
		t.Fatalf("report header = full %v kinds %v, want a full subscription-login scan", decoded.Full, decoded.Kinds)
	}
	return decoded.Items, string(raw)
}

func TestSubscriptionLoginItems(t *testing.T) {
	yes, no := true, false
	tests := []struct {
		name                        string
		account                     agent.UsageAccount
		wantIdentity, wantKey       string
		wantReadiness, wantBilling  string
		wantReasons, wantAllowances int
	}{
		{
			name: "codex with an account id", account: quotaAccount("codex", "acct-raw-1", &yes, 40, 100),
			wantIdentity: "account", wantKey: observation.SubscriptionLoginItemKey("openai-chatgpt", "acct-raw-1", ""),
			wantReadiness: "ready", wantBilling: "allowance", wantAllowances: 1,
		},
		{
			name: "claude without an account id", account: quotaAccount("claude", "", &no, 100),
			wantIdentity: "host-scoped", wantKey: observation.HostScopedItemKey("subscription-login", "claude", "default"),
			wantReadiness: "unavailable", wantReasons: 1, wantBilling: "unknown", wantAllowances: 1,
		},
		{
			name: "muse host session", account: quotaAccount("muse", museHostSessionAccountID, nil),
			wantIdentity: "host-scoped", wantKey: observation.HostScopedItemKey("subscription-login", "muse", "default"),
			wantReadiness: "unknown", wantBilling: "unknown",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			items, _ := observedItems(t, []quotaSlot{{accounts: []agent.UsageAccount{tt.account}}})
			if len(items) != 1 {
				t.Fatalf("items = %v, want one", items)
			}
			item := items[0]
			attrs, _ := item["attributes"].(map[string]any)
			readiness, _ := item["readiness"].(map[string]any)
			billing, _ := attrs["billing"].(map[string]any)
			allowances, _ := attrs["allowances"].([]any)
			reasons, _ := readiness["reasons"].([]any)
			if item["identity"] != tt.wantIdentity || item["itemKey"] != tt.wantKey || item["slot"] != "default" {
				t.Errorf("identity/itemKey/slot = %v/%v/%v, want %s/%s/default", item["identity"], item["itemKey"], item["slot"], tt.wantIdentity, tt.wantKey)
			}
			if readiness["state"] != tt.wantReadiness || len(reasons) != tt.wantReasons {
				t.Errorf("readiness = %v, want %s with %d reasons", readiness, tt.wantReadiness, tt.wantReasons)
			}
			if billing["mode"] != tt.wantBilling || len(allowances) != tt.wantAllowances {
				t.Errorf("billing = %v allowances = %v, want %s and %d", billing, allowances, tt.wantBilling, tt.wantAllowances)
			}
		})
	}
}

func TestSubscriptionLoginItemsPerSlotAndSecretFree(t *testing.T) {
	yes := true
	codex := quotaAccount("codex", "4f9c2b1e-raw-account", &yes, 10)
	codex.Plan = "Pro"
	codex.LimitID = "codex"
	extraHome := "/home/someone/.codex-work"
	items, raw := observedItems(t, []quotaSlot{
		{accounts: []agent.UsageAccount{codex}},
		{configHome: extraHome, accounts: []agent.UsageAccount{codex}},
	})
	if len(items) != 2 || items[0]["slot"] != "default" || items[1]["slot"] != observation.SlotFor("codex", extraHome) {
		t.Fatalf("items = %v, want the default slot and the extra home's slot", items)
	}
	if items[0]["definition"] != "openai-chatgpt:pro" {
		t.Errorf("definition = %v, want openai-chatgpt:pro", items[0]["definition"])
	}
	for _, secret := range []string{codex.ID, extraHome} {
		if strings.Contains(raw, secret) {
			t.Errorf("observations carry %q: %s", secret, raw)
		}
	}
}

// TestSubscriptionLoginItemsFromRealClaudeWindows runs the claude
// mapper's own fixture through the producer, so the real window ids
// (five_hour, seven_day, seven_day_<model>) must encode; an encode
// error would otherwise omit observations from every claude host.
func TestSubscriptionLoginItemsFromRealClaudeWindows(t *testing.T) {
	raw, err := os.ReadFile("../provider/harness/claude/testdata/t3code/usage_read.json")
	if err != nil {
		t.Fatalf("read claude fixture: %v", err)
	}
	limits, _ := providerclaude.UsageResponseToLimits(raw, "2026-11-19T11:59:00Z")
	account := agent.UsageAccount{ID: "claude-account-hash", Provider: "claude", Limits: limits}
	items, out := observedItems(t, []quotaSlot{{accounts: []agent.UsageAccount{account}}})
	if len(items) != 1 {
		t.Fatalf("items = %v, want one claude item", items)
	}
	for _, id := range []string{`"five_hour"`, `"seven_day"`, `"seven_day_fable"`} {
		if !strings.Contains(out, id) {
			t.Errorf("claude observations lack window %s: %s", id, out)
		}
	}
}

func TestSubscriptionLoginReportOmittedWithoutQuota(t *testing.T) {
	if report, err := subscriptionLoginReport([]quotaSlot{{}}, observationNow, false); report != nil || err != nil {
		t.Fatalf("report = %v, err = %v, want nil, nil", report, err)
	}
	if got := (&Daemon{}).ObservationReport(); got != nil {
		t.Fatalf("ObservationReport without quota state = %v, want nil", got)
	}
}

func TestHeartbeatRequestBody_CarriesObservationsBesideQuota(t *testing.T) {
	t.Parallel()
	q := &quotaState{}
	q.noteCodexProbe("acct-raw-beat", "pro", agent.MakeUsageLimits("2026-11-19T11:59:00Z", []agent.UsageWindow{
		{ID: "primary", Kind: agent.UsageWindowSession, UsedPercent: 5},
	}), observationNow)
	d := &Daemon{quota: q}
	tests := []struct {
		name            string
		getObservations func() *observation.Report
		wantPresent     bool
	}{
		{name: "observations ride beside quota", getObservations: d.ObservationReport, wantPresent: true},
		{name: "nil callback omits the key", wantPresent: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			bodies := make(chan []byte, 1)
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				buf, _ := io.ReadAll(r.Body)
				bodies <- buf
				_ = json.NewEncoder(w).Encode(map[string]any{"acknowledged": true})
			}))
			t.Cleanup(srv.Close)
			hs := NewHeartbeatService(HeartbeatOptions{
				WorkerID: "wkr_obs", Hostname: "h", OrchestratorURL: srv.URL, RuntimeJWT: "runtime.jwt.value", IntervalSeconds: 1,
				GetActiveCount: func() int { return 0 }, GetMaxCount: func() int { return 1 },
				GetStatus: func() RegistrationStatus { return RegistrationIdle },
				GetQuota:  d.QuotaSnapshot, GetObservations: tt.getObservations,
			})
			hs.sendOne(context.Background())
			body := <-bodies
			var decoded struct {
				Quota        []map[string]any `json:"quota"`
				Observations *struct {
					Items []map[string]any `json:"items"`
				} `json:"observations"`
			}
			if err := json.Unmarshal(body, &decoded); err != nil {
				t.Fatalf("unmarshal: %v (%s)", err, body)
			}
			if len(decoded.Quota) != 1 || decoded.Quota[0]["id"] != "acct-raw-beat" {
				t.Fatalf("quota = %v, want the unchanged codex entry", decoded.Quota)
			}
			if (decoded.Observations != nil) != tt.wantPresent {
				t.Fatalf("observations present = %v, want %v (body: %s)", decoded.Observations != nil, tt.wantPresent, body)
			}
			if !tt.wantPresent {
				return
			}
			raw, _ := json.Marshal(decoded.Observations)
			if len(decoded.Observations.Items) != 1 || strings.Contains(string(raw), "acct-raw-beat") {
				t.Errorf("observations = %s, want one item without the raw account id", raw)
			}
		})
	}
}
