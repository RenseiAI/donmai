package pi

// real_binary_test.go — real `pi` binary evidence for the endpoint-threading
// seam (Spec.Endpoint -> applyEndpoint -> pi.registerProvider("donmai", ...))
// and for the Inject/Resume capabilities the manifest declares.
//
// Every other test in this package either scripts the RPC wire over an
// io.Pipe (spawnScripted, skipProcess:true) or never reaches a completed
// model turn at all. That proves donmai's INTERPRETATION of the pi protocol
// is internally consistent, but not that it matches what the real pinned
// binary actually does end-to-end — the exact class of gap donmai#215 found
// and fixed (see step20_pi_harness_test.go in donmai-smokes). These tests
// close that gap for the three capabilities a real completed turn can prove:
// a full prompt round trip through a stub OpenAI-compatible endpoint,
// Inject's steer/follow_up routing, and Resume's structural behavior.
//
// # Why these live here and not in donmai-smokes
//
// donmai-smokes drives ONLY the compiled `donmai` binary's CLI surface,
// never donmai as a Go library (donmai-smokes/AGENTS.md). The production
// `donmai agent run` dispatch path never calls Handle.Inject except at the
// post-terminal seam (runner/loop.go drainMemoryInjects, itself gated on a
// platform-delivered heartbeat inject) and never calls Provider.Resume at
// all outside the generic agent/conformance harness (agent/conformance/
// checks.go). Steering a turn WHILE it is in flight, and Resume, are
// therefore unreachable from `donmai agent run`'s black-box CLI today —
// donmai-smokes' step20 lane proves the endpoint-threaded completed-turn
// path instead (assistant round trip + provider-pin lockout), which IS
// reachable there. These tests use the Go-level Provider/Handle API
// directly (legitimate here: this package IS the library under test) to
// reach the two capabilities the CLI path cannot.
//
// # CI scope (read before trusting a green run)
//
// Gated on `pi` (and therefore `node`, since pi is a node script) being on
// PATH; skips cleanly otherwise. donmai's own hosted CI (.github/workflows/
// ci.yml) does NOT install node/pi — only donmai-smokes' CI does. So a green
// run of this file is real evidence on a machine with pi installed (a
// developer machine, or a future donmai CI job that installs it), but is NOT
// currently exercised by donmai's hosted CI. Treat it as local/manual
// real-binary evidence layered on top of the scripted protocol tests, not as
// a hosted-CI gate.
//
// Version churn: pinned against the same @earendil-works/pi-coding-agent
// version this package's probe.go pins (PinnedVersion). If these tests start
// failing after an operator upgrades their local `pi`, check the pin first.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/RenseiAI/donmai/agent"
)

// realBinaryAvailable reports whether both `pi` and `node` are resolvable on
// PATH. pi is a `#!/usr/bin/env node` script (step20_pi_harness_test.go's
// finding 1 in donmai-smokes), so node must be present too or the child dies
// at exec before ever reaching the RPC protocol.
func realBinaryAvailable(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("pi"); err != nil {
		t.Skip("real-binary pi test: `pi` not on PATH (npm i -g @earendil-works/pi-coding-agent@" + PinnedVersion + ") — skipping")
	}
	if _, err := exec.LookPath("node"); err != nil {
		t.Skip("real-binary pi test: `node` not on PATH (pi is a node script) — skipping")
	}
}

// stubTurn is one recorded chat-completions request the real-binary tests'
// local stub endpoint observed.
type stubTurn struct {
	Model    string
	Stream   bool
	Messages []map[string]any
}

// stubUserTexts flattens every user-role message's text content in this
// turn's request, so a test can assert an injected nonce reached the model
// as real conversation content (not just that some RPC frame was written).
func (s stubTurn) stubUserTexts() []string {
	var out []string
	for _, m := range s.Messages {
		if fmt.Sprint(m["role"]) != "user" {
			continue
		}
		switch c := m["content"].(type) {
		case string:
			out = append(out, c)
		case []any:
			for _, part := range c {
				if pm, ok := part.(map[string]any); ok {
					out = append(out, fmt.Sprint(pm["text"]))
				}
			}
		}
	}
	return out
}

// realBinaryStub is a local httptest OpenAI-chat-compatible endpoint. It
// answers every /v1/chat/completions call with a short streamed reply
// carrying a distinctive per-call marker, and records every request so a
// test can assert both WHAT reached the model (provider-pin lockout,
// injected content) and that NOTHING else was reachable (the child has no
// other credential — env hygiene is exercised by the existing scripted
// tests; this fixture only needs to prove single-endpoint exclusivity).
type realBinaryStub struct {
	srv   *httptest.Server
	model string

	mu        sync.Mutex
	turns     []stubTurn
	responses []stubResponse
	// Optional fixture barrier after recording a real request, before replying.
	beforeResponse func(*http.Request, int)
}

// stubResponse lets a focused conformance test make the real pi binary drive
// one native tool call before the next model turn completes. The default stub
// behavior remains the simple one-text-turn reply used by the existing tests.
type stubResponse struct {
	Text     string
	ToolCall *stubToolCall
}

type stubToolCall struct {
	ID        string
	Name      string
	Arguments string
}

func newRealBinaryStub(t *testing.T, model string) *realBinaryStub {
	t.Helper()
	s := &realBinaryStub{model: model}
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/chat/completions", s.handle)
	mux.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte("real_binary_test stub: unsupported route"))
	})
	s.srv = httptest.NewServer(mux)
	t.Cleanup(s.srv.Close)
	return s
}

func (s *realBinaryStub) baseURL() string { return s.srv.URL + "/v1" }

func (s *realBinaryStub) handle(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Model    string           `json:"model"`
		Stream   bool             `json:"stream"`
		Messages []map[string]any `json:"messages"`
	}
	_ = json.NewDecoder(r.Body).Decode(&body)

	s.mu.Lock()
	n := len(s.turns) + 1
	s.turns = append(s.turns, stubTurn{Model: body.Model, Stream: body.Stream, Messages: body.Messages})
	response := stubResponse{Text: fmt.Sprintf("stub-reply-%d", n)}
	if n <= len(s.responses) {
		response = s.responses[n-1]
	}
	beforeResponse := s.beforeResponse
	s.mu.Unlock()

	if beforeResponse != nil {
		beforeResponse(r, n)
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.WriteHeader(http.StatusOK)
	flusher, _ := w.(http.Flusher)
	chunk := func(payload map[string]any) {
		payload["id"] = "chatcmpl-real-binary-test"
		payload["object"] = "chat.completion.chunk"
		payload["created"] = time.Now().Unix()
		payload["model"] = s.model
		data, _ := json.Marshal(payload)
		_, _ = fmt.Fprintf(w, "data: %s\n\n", data)
		if flusher != nil {
			flusher.Flush()
		}
	}
	chunk(map[string]any{"choices": []map[string]any{{"index": 0, "delta": map[string]any{"role": "assistant"}}}})
	finishReason := "stop"
	if response.ToolCall != nil {
		finishReason = "tool_calls"
		chunk(map[string]any{"choices": []map[string]any{{"index": 0, "delta": map[string]any{"tool_calls": []map[string]any{{
			"index": 0, "id": response.ToolCall.ID, "type": "function", "function": map[string]any{
				"name": response.ToolCall.Name, "arguments": response.ToolCall.Arguments,
			},
		}}}}}})
	} else {
		chunk(map[string]any{"choices": []map[string]any{{"index": 0, "delta": map[string]any{"content": response.Text}}}})
	}
	chunk(map[string]any{
		"choices": []map[string]any{{"index": 0, "delta": map[string]any{}, "finish_reason": finishReason}},
		"usage":   map[string]any{"prompt_tokens": 5, "completion_tokens": 3, "total_tokens": 8},
	})
	_, _ = fmt.Fprint(w, "data: [DONE]\n\n")
	if flusher != nil {
		flusher.Flush()
	}
}

func (s *realBinaryStub) recordedTurns() []stubTurn {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]stubTurn(nil), s.turns...)
}

// realBinaryModel is the model id the stub's registered "donmai" provider
// presents. Unlike step20's real-catalog constraint (pi validates --model
// against its BUILT-IN catalog only when no --provider is pinned), a value
// arbitrary is fine here: applyEndpoint pins `--provider donmai --model
// <id>` whenever Spec.Endpoint.BaseURL is set, and the embedded policy
// extension registers exactly that id on the "donmai" provider at load
// (extensions/donmai-policy.ts) — so pi never consults its built-in catalog
// for this id at all.
const realBinaryModel = "real-binary-stub-model"

func realBinarySpec(cwd, prompt, baseURL string) agent.Spec {
	return agent.Spec{
		Prompt: prompt,
		Cwd:    cwd,
		Endpoint: &agent.EndpointBinding{
			Company:  agent.CompanyStub,
			Model:    realBinaryModel,
			BaseURL:  baseURL,
			Protocol: agent.ProtoOpenAIChat,
			Host:     agent.HostDirect,
			// A placeholder credential (test-only). Never a real key: the stub
			// answers unconditionally and never checks it. Delivered via
			// Endpoint.Env, mirroring applyEndpoint's own pickAPIKey contract.
			Env: map[string]string{"OPENAI_API_KEY": "real-binary-stub-key"}, //nolint:gosec // G101: fixture placeholder, not a credential.
		},
	}
}

func frozenProfiledProvider(t *testing.T, beforePrompt func(*Handle) error) *Provider {
	t.Helper()
	binary := filepath.Join(frozenArtifactRoot(t), "pi")
	if info, err := os.Stat(binary); err != nil || !info.Mode().IsRegular() {
		t.Fatalf("required frozen profiled Pi artifact is unavailable at %s: %v", binary, err)
	}
	p, err := New(Options{
		PiBin:               binary,
		HandshakeTimeout:    30 * time.Second,
		beforeInitialPrompt: beforePrompt,
	})
	if err != nil {
		t.Fatalf("construct frozen profiled provider: %v", err)
	}
	t.Cleanup(func() { _ = p.Shutdown(context.Background()) })
	if p.artifact == nil {
		t.Fatal("frozen artifact did not select the compiled receipt profile")
	}
	if p.unverified {
		t.Fatal("exact measured artifact retained the generic unverified-version label")
	}
	return p
}

func TestFrozenProfiledRealBinaryMalformedWriteRetriesThenAdjudicatesValidWrite(t *testing.T) {
	workdir := t.TempDir()
	permitted := filepath.Join(workdir, "permitted.txt")
	stub := newRealBinaryStub(t, realBinaryModel)
	validArgs, _ := json.Marshal(map[string]any{"path": permitted, "content": "permitted\n"})
	stub.mu.Lock()
	stub.responses = []stubResponse{
		{ToolCall: &stubToolCall{ID: "call-malformed", Name: "write", Arguments: `{"content":"missing path"}`}},
		{ToolCall: &stubToolCall{ID: "call-valid", Name: "write", Arguments: string(validArgs)}},
		{Text: "complete"},
	}
	stub.mu.Unlock()

	var argv []string
	p := frozenProfiledProvider(t, func(h *Handle) error {
		argv = append([]string(nil), h.cmd.Args...)
		return nil
	})
	spec := realBinarySpec(workdir, "perform the requested write", stub.baseURL())
	spec.Autonomous = true
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	h, err := p.Spawn(ctx, spec)
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	t.Cleanup(func() { _ = h.Stop(context.Background()) })
	events := drainToResult(t, h, 60*time.Second)

	if !argvContains(argv, "--mode") || !argvContains(argv, "rpc") || !argvContains(argv, "--no-extensions") {
		t.Fatalf("profiled child argv did not use the actual headless --mode rpc --no-extensions path: %q", argv)
	}
	decisions := 0
	malformedResult := false
	for _, event := range events {
		switch value := event.(type) {
		case agent.ErrorEvent:
			t.Fatalf("profiled retry became fatal: %+v", value)
		case agent.SystemEvent:
			if value.Subtype == "permission_decision" {
				decisions++
			}
		case agent.ToolResultEvent:
			if value.ToolUseID == "call-malformed" && value.IsError {
				malformedResult = true
			}
		}
	}
	if !malformedResult {
		t.Fatal("malformed write did not surface as a recoverable error ToolResultEvent")
	}
	if decisions != 1 {
		t.Fatalf("Go policy adjudications=%d, want exactly one for the valid retry", decisions)
	}
	ph := h.(*Handle)
	if ph.wasAdjudicated("call-malformed") || !ph.wasAdjudicated("call-valid") {
		t.Fatalf("adjudication identities malformed=%v valid=%v, want false/true", ph.wasAdjudicated("call-malformed"), ph.wasAdjudicated("call-valid"))
	}
	content, err := os.ReadFile(permitted)
	if err != nil || string(content) != "permitted\n" {
		t.Fatalf("permitted retry file content=%q error=%v", content, err)
	}
}

func TestFrozenProfiledRealBinaryUnknownAndDeniedCallsHaveZeroEffects(t *testing.T) {
	workdir := t.TempDir()
	outside := filepath.Join(t.TempDir(), "must-not-exist.txt")
	outsideArgs, _ := json.Marshal(map[string]any{"path": outside, "content": "forbidden"})
	stub := newRealBinaryStub(t, realBinaryModel)
	stub.mu.Lock()
	stub.responses = []stubResponse{
		{ToolCall: &stubToolCall{ID: "call-unknown", Name: "definitely_unknown_tool", Arguments: `{}`}},
		{ToolCall: &stubToolCall{ID: "call-outside", Name: "write", Arguments: string(outsideArgs)}},
		{Text: "complete"},
	}
	stub.mu.Unlock()

	p := frozenProfiledProvider(t, nil)
	spec := realBinarySpec(workdir, "exercise refusal controls", stub.baseURL())
	spec.Autonomous = true
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	h, err := p.Spawn(ctx, spec)
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	t.Cleanup(func() { _ = h.Stop(context.Background()) })
	events := drainToResult(t, h, 60*time.Second)
	unknownResult := false
	decisions := 0
	for _, event := range events {
		switch value := event.(type) {
		case agent.ErrorEvent:
			t.Fatalf("known refusal became fatal: %+v", value)
		case agent.SystemEvent:
			if value.Subtype == "permission_decision" {
				decisions++
			}
		case agent.ToolResultEvent:
			if value.ToolUseID == "call-unknown" && value.IsError {
				unknownResult = true
			}
		}
	}
	if !unknownResult {
		t.Fatal("unknown tool refusal did not surface as a recoverable error ToolResultEvent")
	}
	if decisions != 1 {
		t.Fatalf("Go policy adjudications=%d, want only the denied out-of-workarea write", decisions)
	}
	if _, err := os.Stat(outside); !os.IsNotExist(err) {
		t.Fatalf("out-of-workarea write had an effect: %v", err)
	}
	ph := h.(*Handle)
	if ph.wasAdjudicated("call-unknown") || !ph.wasAdjudicated("call-outside") {
		t.Fatalf("adjudication identities unknown=%v outside=%v, want false/true", ph.wasAdjudicated("call-unknown"), ph.wasAdjudicated("call-outside"))
	}
}

func drainToResult(t *testing.T, h agent.Handle, timeout time.Duration) []agent.Event {
	t.Helper()
	var out []agent.Event
	deadline := time.After(timeout)
	for {
		select {
		case ev, ok := <-h.Events():
			if !ok {
				return out
			}
			out = append(out, ev)
			switch ev.(type) {
			case agent.ResultEvent, agent.ErrorEvent:
				return out
			}
		case <-deadline:
			t.Fatalf("timed out after %s draining events; got %d so far: %+v", timeout, len(out), out)
		}
	}
}

// TestRealBinary_Endpoint_CompletedTurn drives ONE full prompt round trip
// through a stub OpenAI-compatible endpoint against the REAL pi binary:
// Spec.Endpoint -> applyEndpoint -> the embedded extension's
// pi.registerProvider("donmai", ...) -> a real streamed model turn that
// completes. This is the real-binary half of item 3 (assistant-text round
// trip) and item 9 (provider-pin lockout) from
// runs/2026-07-21-open-harness-strategy/09-design-pi-adapter.md §8 — the
// black-box CLI half of the same evidence lives in donmai-smokes' step20
// lane (this package cannot drive `donmai agent run` itself; see the file
// doc comment).
func TestRealBinary_Endpoint_CompletedTurn(t *testing.T) {
	realBinaryAvailable(t)

	stub := newRealBinaryStub(t, realBinaryModel)
	spec := realBinarySpec(t.TempDir(), "reply with a short greeting and nothing else", stub.baseURL())

	p, err := New(Options{HandshakeTimeout: 30 * time.Second})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	h, err := p.Spawn(ctx, spec)
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	t.Cleanup(func() { _ = h.Stop(context.Background()) })

	evs := drainToResult(t, h, 30*time.Second)

	var sawInit, sawAssistantText bool
	var result *agent.ResultEvent
	for _, ev := range evs {
		switch e := ev.(type) {
		case agent.InitEvent:
			sawInit = true
		case agent.AssistantTextEvent:
			if strings.HasPrefix(e.Text, "stub-reply-") {
				sawAssistantText = true
			}
		case agent.ResultEvent:
			r := e
			result = &r
		case agent.ErrorEvent:
			t.Fatalf("session ended with an ErrorEvent instead of completing: %+v", e)
		}
	}
	if !sawInit {
		t.Error("no InitEvent observed")
	}
	if !sawAssistantText {
		t.Error("no assistant text from the stub observed — the prompt -> event-stream round trip did not deliver the model's reply")
	}
	if result == nil {
		t.Fatal("no terminal ResultEvent observed")
	}
	if !result.Success {
		t.Errorf("ResultEvent.Success = false; want true (errors=%v)", result.Errors)
	}

	// Item 9, provider-pin lockout: every turn the child produced hit this
	// stub, pinned to exactly the resolved model — nothing else was
	// reachable (the child has no other provider credential; env_security_test.go
	// covers the blocklist half of that claim).
	turns := stub.recordedTurns()
	if len(turns) == 0 {
		t.Fatal("the stub recorded zero requests — the endpoint-threading seam (applyEndpoint -> pi.registerProvider) did not route the turn here at all")
	}
	for i, turn := range turns {
		if turn.Model != realBinaryModel {
			t.Errorf("turn %d: model = %q; want the pinned model %q — a session that reaches a different model than the resolved cell is a provider-pin lockout failure", i, turn.Model, realBinaryModel)
		}
	}
}

// TestRealBinary_Inject_SteerMidTurn_FollowUpIdle is the real-binary half of
// item 6 (steer/queue) from the design doc's §8 smoke-coverage list: `Inject`
// mid-turn lands as `steer`, `Inject` while idle lands as `follow_up`. The
// scripted unit test (TestInject_SteerWhenInFlight_FollowUpWhenIdle in
// handle_test.go) proves the mapping from Go state to the RPC command frame
// against a fake replay; this test proves the REAL pi binary actually
// delivers both kinds of injected text into a subsequent model turn.
//
// The first prompt is held behind a private test barrier until the REAL pi
// queue_update confirms idle follow_up acceptance. Spawn's return cannot
// establish an idle window: the child may already have emitted turn_start.
// The loopback endpoint then holds its first response until turn_start and
// the REAL steering queue update have both arrived. No sleeps or fabricated
// Handle state stand in for those protocol facts. Post-terminal injection is
// separately exercised below by TestRealBinary_Inject_AfterTerminal_DeliversFollowUp.
func TestRealBinary_Inject_SteerMidTurn_FollowUpIdle(t *testing.T) {
	realBinaryAvailable(t)

	stub := newRealBinaryStub(t, realBinaryModel)
	spec := realBinarySpec(t.TempDir(), "reply with a short greeting and nothing else", stub.baseURL())

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	firstRequest := make(chan struct{})
	releaseResponse := make(chan struct{})
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(releaseResponse) }) }
	t.Cleanup(release)
	stub.mu.Lock()
	stub.beforeResponse = func(r *http.Request, turn int) {
		if turn != 1 {
			return
		}
		close(firstRequest)
		select {
		case <-releaseResponse:
		case <-r.Context().Done():
		}
	}
	stub.mu.Unlock()

	p, err := New(Options{
		HandshakeTimeout: 30 * time.Second,
		beforeInitialPrompt: func(ph *Handle) error {
			if ph.turnInFlight.Load() {
				return fmt.Errorf("turn started before the initial prompt command")
			}
			if err := ph.Inject(ctx, followUpNonce); err != nil {
				return fmt.Errorf("idle Inject: %w", err)
			}
			return waitRealPiQueue(ctx, ph, "followUp", followUpNonce)
		},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	h, err := p.Spawn(ctx, spec)
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	t.Cleanup(func() { _ = h.Stop(context.Background()) })
	ph, ok := h.(*Handle)
	if !ok {
		t.Fatalf("Spawn returned %T; want *Handle", h)
	}
	select {
	case <-firstRequest:
	case <-ctx.Done():
		t.Fatalf("initial model request did not arrive: %v", ctx.Err())
	}
	if err := waitRealPiSystem(ctx, h, "turn_start", nil); err != nil {
		t.Fatal(err)
	}
	if !ph.turnInFlight.Load() {
		t.Fatal("turn_start did not establish in-flight state while response is held")
	}
	if err := h.Inject(ctx, steerNonce); err != nil {
		t.Fatalf("Inject (mid-turn, steer): %v", err)
	}
	if err := waitRealPiQueue(ctx, h, "steering", steerNonce); err != nil {
		t.Fatal(err)
	}
	release()

	evs := drainToResult(t, h, 45*time.Second)
	for _, ev := range evs {
		if e, ok := ev.(agent.ErrorEvent); ok {
			t.Fatalf("session ended with an ErrorEvent: %+v", e)
		}
	}

	turns := stub.recordedTurns()
	if len(turns) < 3 {
		t.Fatalf("got %d model turn(s); want at least 3 (initial prompt, steer delivery, follow_up delivery) — recorded: %+v", len(turns), turns)
	}
	initial := strings.Join(turns[0].stubUserTexts(), "\n")
	steered := strings.Join(turns[1].stubUserTexts(), "\n")
	followedUp := strings.Join(turns[2].stubUserTexts(), "\n")
	if strings.Contains(initial, steerNonce) || strings.Contains(initial, followUpNonce) ||
		!strings.Contains(steered, steerNonce) || strings.Contains(steered, followUpNonce) ||
		!strings.Contains(followedUp, followUpNonce) {
		t.Fatal("model requests did not preserve initial -> steer -> follow_up delivery order")
	}

	var sawSteer, sawFollowUp bool
	for _, turn := range turns {
		for _, text := range turn.stubUserTexts() {
			if strings.Contains(text, steerNonce) {
				sawSteer = true
			}
			if strings.Contains(text, followUpNonce) {
				sawFollowUp = true
			}
		}
	}
	if !sawSteer {
		t.Errorf("the steer nonce %q never reached the stub as conversation content — steer delivered an RPC frame pi accepted, but it did not reach the model", steerNonce)
	}
	if !sawFollowUp {
		t.Errorf("the follow_up nonce %q never reached the stub as conversation content — the idle-inject queued at Spawn was never delivered", followUpNonce)
	}
}

func TestWaitRealPiQueueFiltersUnrelatedSnapshots(t *testing.T) {
	for _, tc := range []struct {
		name      string
		updates   []map[string]any
		wantError bool
	}{
		{name: "empty and unrelated before accepted nonce", updates: []map[string]any{
			{"steering": []string{}, "followUp": []string{}},
			{"steering": []string{"another message"}, "followUp": []string{"unrelated"}},
			{"steering": []string{steerNonce}, "followUp": []string{}},
		}},
		{name: "wrong queue before later correct queue", wantError: true, updates: []map[string]any{
			{"steering": []string{}, "followUp": []string{steerNonce}},
			{"steering": []string{steerNonce}, "followUp": []string{}},
		}},
		{name: "nonce in both queues", wantError: true, updates: []map[string]any{
			{"steering": []string{steerNonce}, "followUp": []string{steerNonce}},
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := getStateResponse("queue-observation")
			for _, update := range tc.updates {
				update["type"] = "queue_update"
				body += event(update)
			}
			_, h, err := spawnScripted(t, agent.Spec{Prompt: "queue observation"}, handshakeEvent("queue-handshake"), body)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = h.Stop(context.Background()) })
			err = waitRealPiQueue(context.Background(), h, "steering", steerNonce)
			if (err != nil) != tc.wantError {
				t.Fatalf("queue observation error=%v, wantError=%v", err, tc.wantError)
			}
		})
	}
}

// waitRealPiSystem observes native events from the real child. A command write
// alone does not prove the child has accepted it before the endpoint settles.
func waitRealPiSystem(ctx context.Context, h agent.Handle, subtype string, check func(agent.SystemEvent) (bool, error)) error {
	timer := time.NewTimer(20 * time.Second)
	defer timer.Stop()
	for {
		select {
		case event, ok := <-h.Events():
			if !ok {
				return fmt.Errorf("pi event stream closed before %s", subtype)
			}
			if failure, ok := event.(agent.ErrorEvent); ok {
				return fmt.Errorf("pi error before %s: %+v", subtype, failure)
			}
			if _, ok := event.(agent.ResultEvent); ok {
				return fmt.Errorf("pi settled before %s", subtype)
			}
			if system, ok := event.(agent.SystemEvent); ok && system.Subtype == subtype {
				if check != nil {
					matched, err := check(system)
					if err != nil {
						return err
					}
					if !matched {
						continue
					}
				}
				return nil
			}
		case <-timer.C:
			return fmt.Errorf("pi did not emit %s before the deadline", subtype)
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

func waitRealPiQueue(ctx context.Context, h agent.Handle, queue, nonce string) error {
	if queue != "steering" && queue != "followUp" {
		return fmt.Errorf("unknown native queue %q", queue)
	}
	return waitRealPiSystem(ctx, h, "queue_update", func(event agent.SystemEvent) (bool, error) {
		raw, ok := event.Raw.(string)
		if !ok {
			return false, fmt.Errorf("queue_update has no native JSON payload")
		}
		var update struct {
			Steering []string `json:"steering"`
			FollowUp []string `json:"followUp"`
		}
		if err := json.Unmarshal([]byte(raw), &update); err != nil {
			return false, fmt.Errorf("decode native queue_update: %w", err)
		}
		messages, other := update.Steering, update.FollowUp
		if queue == "followUp" {
			messages, other = update.FollowUp, update.Steering
		}
		for _, message := range other {
			if strings.Contains(message, nonce) {
				return false, fmt.Errorf("injected nonce entered the wrong native queue (wanted %s): %+v", queue, update)
			}
		}
		for _, message := range messages {
			if strings.Contains(message, nonce) {
				return true, nil
			}
		}
		// Pi also emits snapshots on consumption; an earlier empty/unrelated
		// update is not an acknowledgement of this injected message.
		return false, nil
	})
}

const (
	steerNonce    = "real-binary-steer-nonce-1a2b3c"
	followUpNonce = "real-binary-followup-nonce-4d5e6f"
)

// TestRealBinary_Inject_AfterTerminal_DeliversFollowUp pins the FIX for the
// finding TestRealBinary_Inject_SteerMidTurn_FollowUpIdle's doc comment
// describes: Inject after a terminal ResultEvent now succeeds and reaches
// the model as a follow_up turn, instead of failing with "pi: session
// closed". This matters beyond this package — runner/loop.go's
// drainMemoryInjects (the ONLY production call site of Handle.Inject for a
// headless session) fires exactly at this post-terminal seam, and
// runner/steering.go's attemptSteering calls Inject at the very same seam
// when a completed turn produced no PR. Both need a live rail to deliver
// into once a pi turn has completed — the Handle's event pump (run in
// handle.go) now keeps consuming the RPC stream past a non-fatal
// agent_settled terminal for exactly that reason (dispatch's fatal/
// non-fatal split in event_mapping.go / handle.go).
func TestRealBinary_Inject_AfterTerminal_DeliversFollowUp(t *testing.T) {
	realBinaryAvailable(t)

	stub := newRealBinaryStub(t, realBinaryModel)
	spec := realBinarySpec(t.TempDir(), "reply with a short greeting and nothing else", stub.baseURL())

	p, err := New(Options{HandshakeTimeout: 30 * time.Second})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	h, err := p.Spawn(ctx, spec)
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	t.Cleanup(func() { _ = h.Stop(context.Background()) })

	evs := drainToResult(t, h, 30*time.Second)
	var sawTerminal bool
	for _, ev := range evs {
		switch ev.(type) {
		case agent.ResultEvent, agent.ErrorEvent:
			sawTerminal = true
		}
	}
	if !sawTerminal {
		t.Fatal("session never reached a terminal event — cannot test post-terminal Inject behavior")
	}

	const postTerminalNonce = "real-binary-post-terminal-nonce-7g8h9i"
	if err := h.Inject(ctx, postTerminalNonce); err != nil {
		t.Fatalf("Inject after a terminal ResultEvent = %v; want nil — the pump must stay alive past "+
			"agent_settled so a post-terminal memory-inject or steering follow-up has somewhere to land", err)
	}

	followUpEvs := drainToResult(t, h, 30*time.Second)
	var sawFollowUpTerminal bool
	for _, ev := range followUpEvs {
		if e, ok := ev.(agent.ErrorEvent); ok {
			t.Fatalf("follow-up turn ended with an ErrorEvent: %+v", e)
		}
		if _, ok := ev.(agent.ResultEvent); ok {
			sawFollowUpTerminal = true
		}
	}
	if !sawFollowUpTerminal {
		t.Fatal("post-terminal Inject was accepted but produced no follow-up turn (no second ResultEvent observed)")
	}

	var sawNonce bool
	for _, turn := range stub.recordedTurns() {
		for _, text := range turn.stubUserTexts() {
			if strings.Contains(text, postTerminalNonce) {
				sawNonce = true
			}
		}
	}
	if !sawNonce {
		t.Errorf("the post-terminal inject nonce %q never reached the stub as conversation content — "+
			"Inject returned nil but the follow_up never reached the model", postTerminalNonce)
	}
}

// TestRealBinary_Resume_StructuralReplay is the real-binary half of item 7
// (resume/cursor replay). Provider.Resume has exactly one production call
// site outside this package: agent/conformance/checks.go, which nothing
// currently drives for pi (see runs/2026-08-11-feature-program/02-pi.md §2
// gap 1d). `donmai agent run` never calls it. This test proves what Resume
// against the REAL binary actually does, which is narrower than the design
// doc's item 7 ("replayed entries deduped, session completes"):
//
//   - Resume re-execs pi with `--session <id>` against the persisted session
//     directory and completes a FRESH handshake (proven: a second InitEvent
//     arrives, and get_state's messageCount reflects the prior session's
//     history, so the right session file was found and loaded).
//   - The `get_entries` reply pi sends back is routed by the event mapper
//     (event_mapping.go's `case "response":` branch) as a SystemEvent —
//     observable, but NOT decoded into replayed Assistant/Tool/etc. history.
//     So "replayed entries" still don't reach the caller as reconstructed
//     session events today, and a bare Resume (no follow-up
//     prompt/steer/follow_up after it) still never reaches a terminal on its
//     own — this test bounds its drain and asserts that absence explicitly,
//     so a future change to event_mapping.go that starts decoding get_entries
//     into full historical replay will fail this test loudly instead of the
//     gap re-hiding.
func TestRealBinary_Resume_StructuralReplay(t *testing.T) {
	realBinaryAvailable(t)

	stub := newRealBinaryStub(t, realBinaryModel)
	cwd := t.TempDir()
	spec := realBinarySpec(cwd, "reply with a short greeting and nothing else", stub.baseURL())

	p, err := New(Options{HandshakeTimeout: 30 * time.Second})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	h, err := p.Spawn(ctx, spec)
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}

	var sessionID string
	for _, ev := range drainToResult(t, h, 30*time.Second) {
		if ie, ok := ev.(agent.InitEvent); ok {
			sessionID = ie.SessionID
		}
	}
	_ = h.Stop(context.Background())
	if sessionID == "" {
		t.Fatal("no session id observed from the first session — cannot resume")
	}

	// Resume against the SAME Cwd (sessionLayout derives --session-dir from
	// spec.Cwd), so the resumed process finds the same persisted session.
	h2, err := p.Resume(ctx, sessionID, spec)
	if err != nil {
		t.Fatalf("Resume(%q): %v — Resume must not error against a session it just wrote", sessionID, err)
	}
	t.Cleanup(func() { _ = h2.Stop(context.Background()) })

	// Bounded drain: Resume alone should produce exactly the fresh InitEvent
	// and then stay open with nothing further (see doc comment) — it must
	// NOT error and must NOT silently report the old "policy extension
	// failed to load" spawn-failure shape.
	var evs2 []agent.Event
	deadline := time.After(10 * time.Second)
loop:
	for {
		select {
		case ev, ok := <-h2.Events():
			if !ok {
				break loop
			}
			evs2 = append(evs2, ev)
			if _, ok := ev.(agent.ErrorEvent); ok {
				break loop
			}
		case <-deadline:
			break loop
		}
	}

	var resumeInit *agent.InitEvent
	for _, ev := range evs2 {
		switch e := ev.(type) {
		case agent.InitEvent:
			ie := e
			resumeInit = &ie
		case agent.ErrorEvent:
			t.Fatalf("Resume produced an ErrorEvent: %+v", e)
		case agent.ResultEvent:
			t.Errorf("Resume alone (no follow-up prompt/steer) produced a terminal ResultEvent; "+
				"want none — the get_entries-dropped finding this test pins may have changed: %+v", e)
		}
	}
	if resumeInit == nil {
		t.Fatal("Resume produced no InitEvent — the fresh handshake/get_state round trip did not complete")
	}
	if resumeInit.SessionID != sessionID {
		t.Errorf("resumed session id = %q; want the original %q — Resume loaded the wrong (or a new) session", resumeInit.SessionID, sessionID)
	}
}
