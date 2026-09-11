package runner

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/RenseiAI/donmai/agent"
	"github.com/RenseiAI/donmai/provider/harness/stub"
)

type followupFailureProvider struct {
	agent.Provider
	release <-chan struct{}
	fatal   bool
}

func (p *followupFailureProvider) Capabilities() agent.Capabilities {
	caps := p.Provider.Capabilities()
	caps.SupportsMessageInjection = true
	return caps
}

func (p *followupFailureProvider) Manifest() agent.HarnessManifest {
	return p.Provider.(interface{ Manifest() agent.HarnessManifest }).Manifest()
}

func (p *followupFailureProvider) Spawn(ctx context.Context, _ agent.Spec) (agent.Handle, error) {
	child, cancel := context.WithCancel(ctx)
	h := &followupFailureHandle{events: make(chan agent.Event, 8), cancel: cancel, initialDone: make(chan struct{}), fatal: p.fatal}
	go func() {
		defer close(h.initialDone)
		h.events <- agent.AssistantTextEvent{Text: "complete brief <!-- WORK_RESULT:passed -->"}
		select {
		case <-p.release:
			h.events <- agent.ResultEvent{Success: true}
		case <-child.Done():
		}
	}()
	return h, nil
}

type followupFailureHandle struct {
	events      chan agent.Event
	cancel      context.CancelFunc
	initialDone chan struct{}
	once        sync.Once
	fatal       bool
}

func (h *followupFailureHandle) SessionID() string          { return "fixture-followup" }
func (h *followupFailureHandle) Events() <-chan agent.Event { return h.events }
func (h *followupFailureHandle) Stop(context.Context) error {
	h.cancel()
	<-h.initialDone
	h.once.Do(func() { close(h.events) })
	return nil
}

func (h *followupFailureHandle) Inject(context.Context, string) error {
	if h.fatal {
		h.events <- agent.ErrorEvent{Code: "policy_extension_failed", Message: "unverified policy execution"}
	} else {
		h.events <- agent.ToolResultEvent{ToolName: "write", ToolUseID: "denied", IsError: true, Content: "policy denied before effect"}
		h.events <- agent.ResultEvent{Success: true, Message: "recovered brief <!-- WORK_RESULT:passed -->"}
	}
	h.once.Do(func() { close(h.events) })
	return nil
}

// The actual runner consumes a successful initial turn, then a heartbeat-delivered
// follow-up. Provider failures and recoverable tool results remain different events.
func TestRun_FollowupFailureControlsTerminalEnvelope(t *testing.T) {
	for _, fatal := range []bool{false, true} {
		name := "recovered_tool_error"
		if fatal {
			name = "fatal_provider_error"
		}
		t.Run(name, func(t *testing.T) {
			h := newRunnerHarness(t)
			release := make(chan struct{})
			var releaseOnce sync.Once
			original := h.server.Config.Handler
			var wireMu sync.Mutex
			var statuses []map[string]any
			h.server.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if strings.HasSuffix(r.URL.Path, "/lock-refresh") {
					var body struct {
						AckedInject string `json:"ackedInject"`
					}
					_ = json.NewDecoder(r.Body).Decode(&body)
					if body.AckedInject == "fixture-followup" {
						releaseOnce.Do(func() { close(release) })
						_ = json.NewEncoder(w).Encode(map[string]any{"refreshed": true})
						return
					}
					_ = json.NewEncoder(w).Encode(map[string]any{"refreshed": true, "inject": map[string]any{"deliveryId": "fixture-followup", "text": "follow-up context"}})
					return
				}
				if strings.HasSuffix(r.URL.Path, "/status") {
					var body map[string]any
					_ = json.NewDecoder(r.Body).Decode(&body)
					wireMu.Lock()
					statuses = append(statuses, body)
					wireMu.Unlock()
					_ = json.NewEncoder(w).Encode(map[string]bool{"ok": true})
					return
				}
				original.ServeHTTP(w, r)
			})
			base, _ := stub.New()
			h.runner.registry = NewRegistry()
			if err := h.runner.registry.Register(&followupFailureProvider{Provider: base, release: release, fatal: fatal}); err != nil {
				t.Fatal(err)
			}
			h.runner.hbInterval = 5 * time.Millisecond
			qw := h.queuedWork("followup-terminal")
			qw.WorkType = "research"
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			res, err := h.runner.Run(ctx, qw)
			if err != nil {
				t.Fatal(err)
			}
			want := "completed"
			if fatal {
				want = "failed"
			}
			t.Logf("fatal=%v status=%s failureMode=%s workResult=%s error=%q", fatal, res.Status, res.FailureMode, res.WorkResult, res.Error)
			if res.Status != want {
				t.Errorf("status=%s want=%s", res.Status, want)
			}
			if fatal && res.FailureMode != FailureProviderError {
				t.Errorf("failure mode=%s", res.FailureMode)
			}
			if !fatal && res.FailureMode != "" {
				t.Errorf("recoverable tool error became operational failure: %s", res.FailureMode)
			}
			wireMu.Lock()
			defer wireMu.Unlock()
			if len(statuses) == 0 {
				t.Fatal("actual result poster emitted no terminal status")
			}
			if got := statuses[len(statuses)-1]["status"]; got != want {
				t.Errorf("posted status=%v want=%s", got, want)
			}
		})
	}
}
