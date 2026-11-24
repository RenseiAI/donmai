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
	if !c.maybeReport(context.Background(), idle) {
		t.Fatal("first read did not initiate a POST")
	}
	if !c.waitIdle(10 * time.Second) {
		t.Fatal("background POST did not settle")
	}
	if n := len(posts); n != 0 {
		t.Fatalf("failed first POST advanced the watermark: %d posts", n)
	}
	if !c.maybeReport(context.Background(), idle) {
		t.Fatal("retry of failed first post did not initiate")
	}
	if !c.waitIdle(10 * time.Second) {
		t.Fatal("background POST did not settle")
	}
	if n := len(posts); n != 1 {
		t.Fatalf("posts = %d; want 1 after retry of failed first post", n)
	}
	if c.maybeReport(context.Background(), idle) {
		t.Fatal("unchanged read initiated a POST before the interval")
	}
	if n := len(posts); n != 1 {
		t.Fatalf("unchanged read reposted before the interval: %d posts", n)
	}
	mu.Lock()
	now = now.Add(60 * time.Second)
	mu.Unlock()
	if !c.maybeReport(context.Background(), idle) {
		t.Fatal("refresh read did not initiate a POST after 60s")
	}
	if !c.waitIdle(10 * time.Second) {
		t.Fatal("background POST did not settle")
	}
	if n := len(posts); n != 2 {
		t.Fatalf("posts = %d; want refresh after 60s", n)
	}
	busy := idle
	busy.State = agent.ChatStateBusy
	if !c.maybeReport(context.Background(), busy) {
		t.Fatal("change read did not initiate a POST on idle->busy")
	}
	if !c.waitIdle(10 * time.Second) {
		t.Fatal("background POST did not settle")
	}
	if n := len(posts); n != 3 {
		t.Fatalf("posts = %d; want change-driven post on idle->busy", n)
	}
	ended := busy
	ended.State = agent.ChatStateEnded
	if c.maybeReport(context.Background(), ended) {
		t.Fatal("ended read initiated a POST")
	}
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
	t.Cleanup(func() { close(sess.done); <-done; chatReadinessRefreshInterval = oldInterval })

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

// TestDispatchInteractive_SamplesAlternateScreenBetweenTicks proves the
// supervisor observes a modal excursion that opens and closes without any
// queued notice: a push PTY flips to the alternate screen and back, and the
// running wire must emit the modal state (B3). Without the 1s sample poll the
// Hour-grid ticker never fires during the window and the wire stays idle.
func TestDispatchInteractive_SamplesAlternateScreenBetweenTicks(t *testing.T) {
	oldRefresh, oldSample := chatReadinessRefreshInterval, chatReadinessSampleInterval
	chatReadinessRefreshInterval, chatReadinessSampleInterval = time.Hour, 50*time.Millisecond
	t.Cleanup(func() { chatReadinessRefreshInterval, chatReadinessSampleInterval = oldRefresh, oldSample })

	var mu sync.Mutex
	var states []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		var body struct {
			Status string `json:"status"`
			Chat   struct {
				State string `json:"state"`
			} `json:"chatReadiness"`
		}
		if json.Unmarshal(b, &body) == nil && body.Status == "running" {
			mu.Lock()
			states = append(states, body.Chat.State)
			mu.Unlock()
		}
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)

	wtm, err := worktree.NewManager(worktree.Options{ParentDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	p, err := result.NewPoster(result.Options{PlatformURL: srv.URL, WorkerID: "wkr-chat-modal", AuthToken: "tok", HTTPClient: srv.Client(), BaseDelay: 0})
	if err != nil {
		t.Fatal(err)
	}
	r, err := New(Options{Registry: NewRegistry(), WorktreeManager: wtm, Poster: p, HTTPClient: srv.Client(), MaxSessionDuration: -1, SkipBackstop: true, SkipSteering: true, SkipPostSession: true})
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv(envAttachURL, "")
	t.Setenv(envAttachToken, "")
	sess := liveRecordingInteractiveSession()
	injectCh := make(chan heartbeat.InjectPayload, 8)
	res := &Result{SessionID: "sess-chat-modal"}
	done := make(chan error, 1)
	handle := &testInteractiveHandle{Handle: &fakeHandle{events: make(chan agent.Event)}, session: sess}
	qw := QueuedWork{QueuedWork: prompt.QueuedWork{SessionID: "sess-chat-modal", Mode: interactiveRunMode}}
	go func() {
		_, err := r.dispatchInteractive(context.Background(), handle, t.TempDir(), qw, res, &recordingSink{}, newFakeInteractivePulser(), injectCh, agent.NoticeDeliveryPTYNotice)
		done <- err
	}()
	t.Cleanup(func() { close(sess.done); <-done })

	waitState := func(want string) {
		t.Helper()
		deadline := time.Now().Add(10 * time.Second)
		for {
			mu.Lock()
			found := false
			for _, s := range states {
				if s == want {
					found = true
					break
				}
			}
			mu.Unlock()
			if found {
				return
			}
			if time.Now().After(deadline) {
				mu.Lock()
				got := append([]string(nil), states...)
				mu.Unlock()
				t.Fatalf("timed out waiting for chat state %q; got %v", want, got)
			}
			time.Sleep(5 * time.Millisecond)
		}
	}
	waitState("idle")
	sess.setAlternateScreen(true)
	waitState("modal")
	sess.setAlternateScreen(false)
	deadline := time.Now().Add(10 * time.Second)
	for {
		mu.Lock()
		got := append([]string(nil), states...)
		mu.Unlock()
		if len(got) >= 3 && got[len(got)-1] == "idle" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for the post-modal idle refresh; got %v", got)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// TestDispatchInteractive_OperatorStopDuringStalledReadinessPost proves a slow
// status receiver never blocks operator stop: the running POST stalls, the
// heartbeat fires lost-ownership, and the dispatch must return promptly (B2).
// Without the background POST the supervisor sits inside the HTTP call for
// the full stall and the stop waits behind it.
func TestDispatchInteractive_OperatorStopDuringStalledReadinessPost(t *testing.T) {
	oldRefresh, oldSample := chatReadinessRefreshInterval, chatReadinessSampleInterval
	chatReadinessRefreshInterval, chatReadinessSampleInterval = time.Hour, time.Hour
	t.Cleanup(func() { chatReadinessRefreshInterval, chatReadinessSampleInterval = oldRefresh, oldSample })

	var mu sync.Mutex
	var running int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		var body struct {
			Status string `json:"status"`
		}
		if json.Unmarshal(b, &body) != nil || body.Status != "running" {
			w.WriteHeader(http.StatusOK)
			return
		}
		mu.Lock()
		running++
		n := running
		mu.Unlock()
		if n > 1 {
			select {
			case <-r.Context().Done():
			case <-time.After(5 * time.Second):
			}
		}
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)

	wtm, err := worktree.NewManager(worktree.Options{ParentDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	p, err := result.NewPoster(result.Options{PlatformURL: srv.URL, WorkerID: "wkr-chat-stall", AuthToken: "tok", HTTPClient: srv.Client(), BaseDelay: 0})
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
	res := &Result{SessionID: "sess-chat-stall"}
	done := make(chan error, 1)
	pulser := newFakeInteractivePulser()
	handle := &testNoticeChannelHandle{InteractiveCapable: &testInteractiveHandle{Handle: &fakeHandle{events: make(chan agent.Event)}, session: sess}, channel: nch}
	qw := QueuedWork{QueuedWork: prompt.QueuedWork{SessionID: "sess-chat-stall", Mode: interactiveRunMode}}
	go func() {
		_, err := r.dispatchInteractive(context.Background(), handle, t.TempDir(), qw, res, &recordingSink{}, pulser, injectCh, agent.NoticeDeliveryHook)
		done <- err
	}()
	returned := false
	t.Cleanup(func() {
		if !returned {
			close(sess.done)
			<-done
		}
	})

	waitRunning := func(n int) {
		t.Helper()
		deadline := time.Now().Add(10 * time.Second)
		for {
			mu.Lock()
			got := running
			mu.Unlock()
			if got >= n {
				return
			}
			if time.Now().After(deadline) {
				t.Fatalf("timed out waiting for %d running posts; got %d", n, got)
			}
			time.Sleep(5 * time.Millisecond)
		}
	}
	waitRunning(1)
	injectCh <- heartbeat.InjectPayload{DeliveryID: "dlv-chat-stall", Text: "hello stalled"}
	time.Sleep(200 * time.Millisecond)
	start := time.Now()
	close(pulser.lost)
	select {
	case <-done:
		returned = true
	case <-time.After(15 * time.Second):
		t.Fatal("dispatch did not return after operator stop")
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("operator stop took %v with a stalled readiness receiver; want <2s", elapsed)
	}
}

// TestDispatchInteractive_RefreshesSoonAfterHookConsumption proves the
// periodic refresh stays anchored to the last successful POST instead of a
// fixed 60s grid: after a hook offer (busy) and consumption (idle), the next
// refresh must land about one interval after the idle post (B1). On the old
// grid the first tick after the change is skipped and the receiver goes
// ~2x the interval without an observation.
func TestDispatchInteractive_RefreshesSoonAfterHookConsumption(t *testing.T) {
	oldRefresh, oldSample := chatReadinessRefreshInterval, chatReadinessSampleInterval
	chatReadinessRefreshInterval, chatReadinessSampleInterval = 2*time.Second, 100*time.Millisecond
	t.Cleanup(func() { chatReadinessRefreshInterval, chatReadinessSampleInterval = oldRefresh, oldSample })

	var mu sync.Mutex
	var at []time.Time
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		var body struct {
			Status string `json:"status"`
		}
		if json.Unmarshal(b, &body) == nil && body.Status == "running" {
			mu.Lock()
			at = append(at, time.Now())
			mu.Unlock()
		}
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)

	wtm, err := worktree.NewManager(worktree.Options{ParentDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	p, err := result.NewPoster(result.Options{PlatformURL: srv.URL, WorkerID: "wkr-chat-gap", AuthToken: "tok", HTTPClient: srv.Client(), BaseDelay: 0})
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
	res := &Result{SessionID: "sess-chat-gap"}
	done := make(chan error, 1)
	handle := &testNoticeChannelHandle{InteractiveCapable: &testInteractiveHandle{Handle: &fakeHandle{events: make(chan agent.Event)}, session: sess}, channel: nch}
	qw := QueuedWork{QueuedWork: prompt.QueuedWork{SessionID: "sess-chat-gap", Mode: interactiveRunMode}}
	go func() {
		_, err := r.dispatchInteractive(context.Background(), handle, t.TempDir(), qw, res, &recordingSink{}, newFakeInteractivePulser(), injectCh, agent.NoticeDeliveryHook)
		done <- err
	}()
	t.Cleanup(func() { close(sess.done); <-done })

	waitPosts := func(n int) []time.Time {
		t.Helper()
		deadline := time.Now().Add(15 * time.Second)
		for {
			mu.Lock()
			got := append([]time.Time(nil), at...)
			mu.Unlock()
			if len(got) >= n {
				return got
			}
			if time.Now().After(deadline) {
				t.Fatalf("timed out waiting for %d running posts; got %d", n, len(got))
			}
			time.Sleep(10 * time.Millisecond)
		}
	}
	waitPosts(1)
	injectCh <- heartbeat.InjectPayload{DeliveryID: "dlv-chat-gap", Text: "hello gap"}
	waitPosts(2)
	nch.setConsumed(true)
	got := waitPosts(4)
	if gap := got[3].Sub(got[2]); gap > 3*time.Second {
		t.Fatalf("idle refresh gap after hook consumption = %v; want <=3s (2s interval + 1s tolerance)", gap)
	}
}

// TestChatReadinessReporter_LostResponseCorrects pins the lost-response fix:
// the receiver applies a busy update but its HTTP response is lost, so the
// POST reports failure while the watermark still holds idle. The reverted
// idle read must still re-post (correcting the receiver back to idle)
// instead of reading as unchanged until the 60s refresh. Ended never posts
// even while uncertain.
func TestChatReadinessReporter_LostResponseCorrects(t *testing.T) {
	var mu sync.Mutex
	var posts []string
	failBusyOnce := true
	now := time.Now()
	clock := func() time.Time { mu.Lock(); defer mu.Unlock(); return now }
	post := func(_ context.Context, w *afclient.SessionChatReadiness) error {
		mu.Lock()
		defer mu.Unlock()
		if w.State == "busy" && failBusyOnce {
			failBusyOnce = false
			// Receiver applied busy before the connection dropped: record
			// the applied state, then report the lost response as failure.
			posts = append(posts, "applied-busy-lost-response")
			return errors.New("chat readiness response lost")
		}
		posts = append(posts, w.State)
		return nil
	}
	c := newChatReadinessReporter(post, clock, 60*time.Second)
	idle := agent.ChatReadiness{Channel: agent.NoticeDeliveryHook, State: agent.ChatStateIdle, LiveSupported: true, QueueSupported: true}
	busy := idle
	busy.State = agent.ChatStateBusy
	ended := idle
	ended.State = agent.ChatStateEnded

	if !c.maybeReport(context.Background(), idle) {
		t.Fatal("initial idle did not initiate a POST")
	}
	if !c.waitIdle(10 * time.Second) {
		t.Fatal("background POST did not settle")
	}
	if n := len(posts); n != 1 || posts[0] != "idle" {
		t.Fatalf("posts = %v; want [idle] after initial ack", posts)
	}
	if !c.maybeReport(context.Background(), busy) {
		t.Fatal("busy change did not initiate a POST")
	}
	if !c.waitIdle(10 * time.Second) {
		t.Fatal("background POST did not settle")
	}
	// The busy POST failed after the receiver applied it: the watermark is
	// still idle, but the cache is uncertain so the reverted idle read must
	// correct the receiver instead of reading as unchanged.
	if !c.maybeReport(context.Background(), idle) {
		t.Fatal("reverted idle after a lost busy response did not initiate a corrective POST")
	}
	if !c.waitIdle(10 * time.Second) {
		t.Fatal("background POST did not settle")
	}
	mu.Lock()
	got := append([]string(nil), posts...)
	mu.Unlock()
	if len(got) != 3 || got[0] != "idle" || got[1] != "applied-busy-lost-response" || got[2] != "idle" {
		t.Fatalf("posts = %v; want [idle applied-busy-lost-response idle] corrective sequence", got)
	}
	// The corrective POST cleared the uncertainty: a further unchanged idle
	// read stays quiet until the refresh bound.
	if c.maybeReport(context.Background(), idle) {
		t.Fatal("settled idle re-posted before the interval")
	}
}
