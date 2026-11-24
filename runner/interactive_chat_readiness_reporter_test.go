package runner

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/RenseiAI/donmai/afclient"
	"github.com/RenseiAI/donmai/agent"
	"github.com/RenseiAI/donmai/prompt"
	"github.com/RenseiAI/donmai/result"
	"github.com/RenseiAI/donmai/runtime/heartbeat"
	"github.com/RenseiAI/donmai/runtime/worktree"
)

// TestChatReadinessReporter_ChangeRefreshEnded pins the reporter contract:
// change posts, unchanged refreshes after the interval, ended never posts,
// and a failed POST is swallowed without advancing the watermark.
func TestChatReadinessReporter_ChangeRefreshEnded(t *testing.T) {
	var mu sync.Mutex
	var posts []*afclient.SessionChatReadiness
	errTestChatReadiness := errors.New("chat readiness post failed")
	failOnce := true
	now := time.Now()
	clock := func() time.Time { mu.Lock(); defer mu.Unlock(); return now }
	post := func(_ context.Context, w *afclient.SessionChatReadiness) error {
		mu.Lock()
		defer mu.Unlock()
		if failOnce {
			failOnce = false
			return errTestChatReadiness
		}
		posts = append(posts, w)
		return nil
	}
	c := newChatReadinessReporter(post, clock, 60*time.Second)
	idle := agent.ChatReadiness{Channel: agent.NoticeDeliveryHook, State: agent.ChatStateIdle, LiveSupported: true, QueueSupported: true}
	c.maybeReport(context.Background(), idle)
	if n := len(posts); n != 0 {
		t.Fatalf("failed first POST advanced the watermark: %d posts", n)
	}
	c.maybeReport(context.Background(), idle)
	if n := len(posts); n != 1 {
		t.Fatalf("posts = %d; want 1 after retry of failed first post", n)
	}
	c.maybeReport(context.Background(), idle)
	if n := len(posts); n != 1 {
		t.Fatalf("unchanged read reposted before the interval: %d posts", n)
	}
	mu.Lock()
	now = now.Add(61 * time.Second)
	mu.Unlock()
	c.maybeReport(context.Background(), idle)
	if n := len(posts); n != 2 {
		t.Fatalf("posts = %d; want refresh after 60s", n)
	}
	busy := idle
	busy.State = agent.ChatStateBusy
	c.maybeReport(context.Background(), busy)
	if n := len(posts); n != 3 {
		t.Fatalf("posts = %d; want change-driven post on idle->busy", n)
	}
	ended := busy
	ended.State = agent.ChatStateEnded
	c.maybeReport(context.Background(), ended)
	if n := len(posts); n != 3 {
		t.Fatalf("ended read posted: %d posts", n)
	}
	if got := chatReadinessWire(idle, time.Now()); got.Version != 1 || got.Channel != "hook" || got.State != "idle" || got.Reason != "" {
		t.Fatalf("wire = %+v; want version 1 hook idle with empty reason", got)
	}
}

// TestDispatchInteractive_PostsChatReadiness drives dispatchInteractive and
// proves the running status wire carries chatReadiness on change: an idle
// post at start and a busy post after an inject, with the golden shape.
func TestDispatchInteractive_PostsChatReadiness(t *testing.T) {
	oldInterval := chatReadinessRefreshInterval
	chatReadinessRefreshInterval = time.Hour
	defer func() { chatReadinessRefreshInterval = oldInterval }()

	var mu sync.Mutex
	var running [][]byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		if strings.HasSuffix(r.URL.Path, "/status") {
			var body struct {
				Status string `json:"status"`
			}
			if json.Unmarshal(b, &body) == nil && body.Status == "running" {
				mu.Lock()
				running = append(running, b)
				mu.Unlock()
			}
		}
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)

	wtm, err := worktree.NewManager(worktree.Options{ParentDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	p, err := result.NewPoster(result.Options{PlatformURL: srv.URL, WorkerID: "wkr-chat", AuthToken: "tok", HTTPClient: srv.Client(), BaseDelay: 0})
	if err != nil {
		t.Fatal(err)
	}
	r, err := New(Options{Registry: NewRegistry(), WorktreeManager: wtm, Poster: p, HTTPClient: srv.Client(), MaxSessionDuration: -1, SkipBackstop: true, SkipSteering: true, SkipPostSession: true})
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv(envAttachURL, "")
	t.Setenv(envAttachToken, "")
	nch := &fakeNoticeChannel{}
	sess := liveRecordingInteractiveSession()
	injectCh := make(chan heartbeat.InjectPayload, 8)
	res := &Result{SessionID: "sess-chat-live"}
	done := make(chan error, 1)
	handle := &testNoticeChannelHandle{InteractiveCapable: &testInteractiveHandle{Handle: &fakeHandle{events: make(chan agent.Event)}, session: sess}, channel: nch}
	qw := QueuedWork{QueuedWork: prompt.QueuedWork{SessionID: "sess-chat-live", Mode: interactiveRunMode}}
	go func() {
		_, err := r.dispatchInteractive(context.Background(), handle, t.TempDir(), qw, res, &recordingSink{}, newFakeInteractivePulser(), injectCh, agent.NoticeDeliveryHook)
		done <- err
	}()
	t.Cleanup(func() { close(sess.done); <-done })

	waitPosts := func(n int) [][]byte {
		t.Helper()
		deadline := time.Now().Add(10 * time.Second)
		for {
			mu.Lock()
			got := append([][]byte(nil), running...)
			mu.Unlock()
			if len(got) >= n {
				return got
			}
			if time.Now().After(deadline) {
				t.Fatalf("timed out waiting for %d running posts; got %d", n, len(got))
			}
			time.Sleep(5 * time.Millisecond)
		}
	}
	got := waitPosts(1)
	var first map[string]any
	if err := json.Unmarshal(got[0], &first); err != nil {
		t.Fatalf("first running body not JSON: %v", err)
	}
	chat, ok := first["chatReadiness"].(map[string]any)
	if !ok {
		t.Fatalf("first running post has no chatReadiness: %v", first)
	}
	if chat["version"] != float64(1) || chat["channel"] != "hook" || chat["state"] != "idle" || chat["liveSupported"] != true || chat["queueSupported"] != true || chat["reason"] != "" {
		t.Fatalf("chatReadiness golden mismatch: %v", chat)
	}
	if _, err := time.Parse(time.RFC3339, chat["observedAt"].(string)); err != nil {
		t.Fatalf("observedAt not RFC3339: %v", chat["observedAt"])
	}
	injectCh <- heartbeat.InjectPayload{DeliveryID: "dlv-chat-busy", Text: "hello live"}
	got = waitPosts(2)
	var second map[string]any
	if err := json.Unmarshal(got[1], &second); err != nil {
		t.Fatalf("second running body not JSON: %v", err)
	}
	if second["chatReadiness"].(map[string]any)["state"] != "busy" {
		t.Fatalf("second chatReadiness state = %v; want busy after inject", second["chatReadiness"])
	}
}
