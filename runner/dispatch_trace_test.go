package runner

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/RenseiAI/donmai/agent"
	"github.com/RenseiAI/donmai/internal/interview"
	"github.com/RenseiAI/donmai/provider/harness/stub"
)

type traceInterviewProvider struct {
	agent.HarnessProvider
	handle agent.Handle
}

func (p *traceInterviewProvider) Spawn(context.Context, agent.Spec) (agent.Handle, error) {
	return p.handle, nil
}

func TestRunDispatchTraceHeaders(t *testing.T) {
	for _, mode := range []string{"headless", interview.InterviewRunMode} {
		for _, traced := range []bool{false, true} {
			name := mode + "/absent"
			if traced {
				name = mode + "/present"
			}
			t.Run(name, func(t *testing.T) {
				h := newRunnerHarness(t)
				qw := h.queuedWork("trace-headers")
				if traced {
					qw.Traceparent = "00-0123456789abcdef0123456789abcdef-0123456789abcdef-01"
					qw.Tracestate = "vendor=value"
				}
				var activities, deltas atomic.Int64
				srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
					activity := strings.HasSuffix(req.URL.Path, "/activity")
					delta := strings.HasSuffix(req.URL.Path, "/token-delta")
					if activity || delta {
						for header, want := range map[string]string{"traceparent": qw.Traceparent, "tracestate": qw.Tracestate} {
							values, present := req.Header[http.CanonicalHeaderKey(header)]
							if want == "" {
								if present {
									t.Errorf("%s: %s unexpectedly present: %q", req.URL.Path, header, values)
								}
							} else if len(values) != 1 || values[0] != want {
								t.Errorf("%s: %s = %q; want [%q]", req.URL.Path, header, values, want)
							}
						}
						if activity {
							activities.Add(1)
						} else {
							deltas.Add(1)
						}
						w.WriteHeader(http.StatusOK)
						return
					}
					h.server.Config.Handler.ServeHTTP(w, req)
				}))
				t.Cleanup(srv.Close)
				qw.PlatformURL = srv.URL
				if mode == interview.InterviewRunMode {
					qw.Mode = mode
					handle := newScriptedHandle()
					handle.pushTurn(
						agent.InitEvent{SessionID: "trace-provider"},
						agent.AssistantTextEvent{Text: "Complete.\n" + interview.InterviewCompleteSentinel},
						agent.ResultEvent{Success: true},
					)
					provider, err := stub.New()
					if err != nil {
						t.Fatal(err)
					}
					if err := h.runner.registry.Register(&traceInterviewProvider{HarnessProvider: provider.(agent.HarnessProvider), handle: handle}); err != nil {
						t.Fatal(err)
					}
				}
				ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
				defer cancel()
				res, err := h.runner.Run(ctx, qw)
				if err != nil {
					t.Fatalf("Run: %v", err)
				}
				if res.Status != "completed" {
					t.Fatalf("status = %q; want completed (%s)", res.Status, res.Error)
				}
				if activities.Load() == 0 {
					t.Error("Run posted no activity")
				}
				if mode == interview.InterviewRunMode && deltas.Load() == 0 {
					t.Error("interview Run posted no token delta")
				}
			})
		}
	}
}
