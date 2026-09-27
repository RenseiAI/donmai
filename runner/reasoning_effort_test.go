package runner

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/RenseiAI/donmai/agent"
	"github.com/RenseiAI/donmai/provider/harness/stub"
	"github.com/RenseiAI/donmai/result"
	"github.com/RenseiAI/donmai/runtime/state"
	"github.com/RenseiAI/donmai/runtime/worktree"
)

// TestRecordReasoningEffort pins the runner's effort record: one
// reasoning_effort event carrying the configured level (empty when none), and
// the same fact in state.json — the level name, or the explicit
// not-configured sentinel so an absent effort is never mistaken for a record
// that predates the field.
func TestRecordReasoningEffort(t *testing.T) {
	t.Parallel()
	cases := []struct {
		effort    agent.EffortLevel
		wantState string
	}{
		{effort: agent.EffortMax, wantState: "max"},
		{effort: agent.EffortXHigh, wantState: "xhigh"},
		{effort: "", wantState: state.ReasoningEffortNotConfigured},
	}
	for _, tc := range cases {
		t.Run("effort="+string(tc.effort), func(t *testing.T) {
			t.Parallel()
			r := minimalRunner(t)
			dir := t.TempDir()
			rec := &recordingSink{}
			r.recordReasoningEffort(context.Background(), dir, "sess-1", tc.effort, rec)

			rec.mu.Lock()
			events := append([]agent.Event(nil), rec.events...)
			rec.mu.Unlock()
			if len(events) != 1 {
				t.Fatalf("sent %d events, want exactly 1: %v", len(events), events)
			}
			ev, ok := events[0].(agent.SystemEvent)
			if !ok || ev.Subtype != agent.SystemSubtypeReasoningEffort || ev.Message != string(tc.effort) {
				t.Errorf("event = %#v, want a %s event carrying %q", events[0], agent.SystemSubtypeReasoningEffort, tc.effort)
			}
			persisted, err := state.NewStore().Read(dir)
			if err != nil {
				t.Fatalf("read state: %v", err)
			}
			if persisted.ReasoningEffort != tc.wantState {
				t.Errorf("state reasoningEffort = %q, want %q", persisted.ReasoningEffort, tc.wantState)
			}
		})
	}
}

// TestRun_PostsReasoningEffortActivity drives a full Run against a fake
// platform and asserts the session's activity stream states the effort it was
// spawned with — the configured level from the dispatch's resolved profile, or
// that none was configured.
func TestRun_PostsReasoningEffortActivity(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	cases := []struct {
		effort agent.EffortLevel
		want   string
	}{
		{effort: agent.EffortMax, want: "reasoning effort: max (as configured)"},
		{effort: "", want: "reasoning effort: not configured; none was requested, so the harness or model default applies"},
	}
	for _, tc := range cases {
		t.Run("effort="+string(tc.effort), func(t *testing.T) {
			var (
				mu       sync.Mutex
				contexts []string
			)
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if strings.HasSuffix(r.URL.Path, "/activity") {
					raw, _ := io.ReadAll(r.Body)
					_ = r.Body.Close()
					var body struct {
						Activity struct {
							Type    string `json:"type"`
							Content string `json:"content"`
						} `json:"activity"`
					}
					if json.Unmarshal(raw, &body) == nil && body.Activity.Type == "context" {
						mu.Lock()
						contexts = append(contexts, body.Activity.Content)
						mu.Unlock()
					}
					w.WriteHeader(http.StatusOK)
					return
				}
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"refreshed":true,"ok":true}`))
			}))
			t.Cleanup(srv.Close)

			wtm, err := worktree.NewManager(worktree.Options{ParentDir: t.TempDir()})
			if err != nil {
				t.Fatal(err)
			}
			poster, err := result.NewPoster(result.Options{
				PlatformURL: srv.URL, WorkerID: "wkr_effort", AuthToken: "tok", HTTPClient: srv.Client(), BaseDelay: 1,
			})
			if err != nil {
				t.Fatal(err)
			}
			reg := NewRegistry()
			p, _ := stub.New()
			_ = reg.Register(p)
			r, err := New(Options{
				Registry: reg, WorktreeManager: wtm, Poster: poster, HTTPClient: srv.Client(),
				SkipBackstop: true, SkipSteering: true, SkipPostSession: true,
			})
			if err != nil {
				t.Fatal(err)
			}
			qw := QueuedWork{
				QueuedWork:      queuedWorkBase("REN-EFFORT-1"),
				WorkerID:        "wkr_effort",
				AuthToken:       "tok",
				PlatformURL:     srv.URL,
				ResolvedProfile: ResolvedProfile{Provider: agent.ProviderStub, Effort: tc.effort},
			}
			qw.Repository = makeBareRepo(t)

			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			if _, runErr := r.Run(ctx, qw); runErr != nil {
				t.Fatalf("Run: %v", runErr)
			}
			mu.Lock()
			got := append([]string(nil), contexts...)
			mu.Unlock()
			matches := 0
			for _, c := range got {
				if strings.HasPrefix(c, "reasoning effort: ") {
					matches++
					if c != tc.want {
						t.Errorf("effort activity = %q, want %q", c, tc.want)
					}
				}
			}
			if matches != 1 {
				t.Errorf("effort activities = %d, want exactly 1 (context activities: %q)", matches, got)
			}
		})
	}
}
