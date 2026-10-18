package runner

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/RenseiAI/donmai/agent"
	"github.com/RenseiAI/donmai/provider/harness/stub"
	"github.com/RenseiAI/donmai/result"
	"github.com/RenseiAI/donmai/runtime/worktree"
)

func TestRun_DispatchTraceHeartbeatHeaders(t *testing.T) {
	for _, tc := range []struct{ name, parent, state string }{
		{name: "present", parent: "00-11111111111111111111111111111111-2222222222222222-01", state: "vendor=value"},
		{name: "absent"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var mu sync.Mutex
			seen := map[string]int{}
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				for _, suffix := range []string{"/lock-refresh", "/step-heartbeat"} {
					if strings.HasSuffix(req.URL.Path, suffix) {
						mu.Lock()
						seen[suffix]++
						mu.Unlock()
						for name, want := range map[string]string{"traceparent": tc.parent, "tracestate": tc.state} {
							if got := req.Header.Values(name); (want == "" && len(got) != 0) || (want != "" && (len(got) != 1 || got[0] != want)) {
								t.Errorf("%s %s = %q, want %q (absent when empty)", suffix, name, got, want)
							}
						}
					}
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
				PlatformURL: srv.URL, WorkerID: "worker", AuthToken: "token", HTTPClient: srv.Client(), BaseDelay: 1,
			})
			if err != nil {
				t.Fatal(err)
			}
			reg := NewRegistry()
			provider, err := stub.New()
			if err != nil {
				t.Fatal(err)
			}
			if err := reg.Register(provider); err != nil {
				t.Fatal(err)
			}
			r, err := New(Options{
				Registry: reg, WorktreeManager: wtm, Poster: poster, HTTPClient: srv.Client(),
				SkipBackstop: true, SkipSteering: true, SkipPostSession: true,
			})
			if err != nil {
				t.Fatal(err)
			}
			qw := QueuedWork{
				QueuedWork: queuedWorkBase("heartbeat-trace"),
				WorkerID:   "worker", AuthToken: "token", PlatformURL: srv.URL,
				ResolvedProfile: ResolvedProfile{Provider: agent.ProviderStub},
			}
			qw.Traceparent, qw.Tracestate = tc.parent, tc.state
			qw.Repository = makeBareRepo(t)
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			if _, err := r.Run(ctx, qw); err != nil {
				t.Fatalf("Run: %v", err)
			}
			mu.Lock()
			defer mu.Unlock()
			for _, suffix := range []string{"/lock-refresh", "/step-heartbeat"} {
				if seen[suffix] == 0 {
					t.Errorf("no %s request received", suffix)
				}
			}
		})
	}
}
