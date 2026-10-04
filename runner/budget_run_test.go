package runner

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/RenseiAI/donmai/agent"
	"github.com/RenseiAI/donmai/prompt"
)

// Session token budget, enforced during the run. The scripted sessions below
// stream their model calls the way pi does — a call's assistant message, then
// its tool calls, then the call's usage (agent.LlmCallEvent) — against a
// 1,000-token cap, so the wrap-up point is 800 tokens.

const testTokenCap = 1_000

func tokenBudget() *prompt.StageBudget { return &prompt.StageBudget{MaxTokens: testTokenCap} }

func toolUse(id, command string) agent.ToolUseEvent {
	return agent.ToolUseEvent{ToolName: "bash", ToolUseID: id, Input: map[string]any{"command": command}}
}

func toolResult(id, content string) agent.ToolResultEvent {
	return agent.ToolResultEvent{ToolName: "bash", ToolUseID: id, Content: content}
}

func say(text string) agent.AssistantTextEvent { return agent.AssistantTextEvent{Text: text} }

// modelCall is one pi-style model call: its message (when any), its tool
// calls each with a result, then the usage the call spent.
func modelCall(text string, inputTokens int64, tools ...string) []agent.Event {
	var events []agent.Event
	if text != "" {
		events = append(events, say(text))
	}
	for _, id := range tools {
		events = append(events, toolUse(id, "go test ./..."), toolResult(id, "ok"))
	}
	return append(events, callUsage(inputTokens, 0))
}

func concat(groups ...[]agent.Event) []agent.Event {
	var out []agent.Event
	for _, g := range groups {
		out = append(out, g...)
	}
	return out
}

// wantMetered pins that the reported cost counts exactly what the budget meter
// observed, on the envelope and on the status the platform received.
func wantMetered(t *testing.T, res *Result, status map[string]json.RawMessage, tokens int64) {
	t.Helper()
	if res.Cost == nil {
		t.Fatalf("Cost = nil; want %d tokens", tokens)
	}
	if got := res.Cost.InputTokens + res.Cost.OutputTokens; got != tokens {
		t.Errorf("Cost tokens = %d; want %d", got, tokens)
	}
	if res.BudgetReport == nil || res.BudgetReport.ObservedTokens != tokens {
		t.Errorf("BudgetReport = %+v; want ObservedTokens %d", res.BudgetReport, tokens)
	}
	if status != nil {
		if got, want := string(status["inputTokens"]), fmt.Sprint(res.Cost.InputTokens); got != want {
			t.Errorf("posted inputTokens = %s; want %s", got, want)
		}
	}
}

// TestRun_TokenCapStopsAtTheNextTurnBoundary pins the in-run cap: the turn
// that crosses the cap is stopped at its next boundary — the next model
// call's usage with no tool call running — not when the turn ends, and no
// follow-up turn starts. Before this, the cap was checked only against the
// turn's ResultEvent, so a turn ran on past the cap until it ended.
func TestRun_TokenCapStopsAtTheNextTurnBoundary(t *testing.T) {
	platform := newRecordingPlatformServer(t)
	res, provider := runScriptedSession(t, scriptedSession{
		workType:   "development",
		repository: followUpRepository,
		pulls:      map[int]string{7: pullAtSessionCommit},
		budget:     tokenBudget(),
		platform:   platform,
		turns: []verdictScriptTurn{{events: concat(
			modelCall("Reading the parser.", 600, "a"),
			modelCall("", 300, "b"),
			// The meter crosses the cap while tool call c runs: the turn
			// is not cut there.
			[]agent.Event{toolUse("c", "go test ./..."), callUsage(200, 0), toolResult("c", "ok")},
			// The next boundary: the model call after c, with nothing running.
			modelCall("Next I will refactor the lexer.", 150),
			// Past the boundary: never reached.
			modelCall("Opened "+followUpPR+"\nWORK_RESULT: passed", 50, "d"),
		), cost: &agent.CostData{InputTokens: 1_300}}, {text: "a follow-up turn must not start"}},
	})
	if res.Status != "failed" || res.FailureMode != FailureBudgetExceeded {
		t.Fatalf("Status=%q FailureMode=%q (%s); want failed/%s", res.Status, res.FailureMode, res.Error, FailureBudgetExceeded)
	}
	if res.Summary != "Next I will refactor the lexer." {
		t.Errorf("Summary = %q; want the message at the boundary, where the turn was stopped", res.Summary)
	}
	if res.PullRequestURL != "" || res.WorkResult != "" {
		t.Errorf("PullRequestURL=%q WorkResult=%q; events past the boundary must not be observed", res.PullRequestURL, res.WorkResult)
	}
	if len(provider.prompts) != 0 {
		t.Errorf("follow-up prompts = %q; none may follow the cap", provider.prompts)
	}
	want := &agent.BudgetBreach{Cap: string(CapTokens), Detail: "max-tokens exceeded: observed=1100 limit=1000"}
	if res.BudgetBreach == nil || *res.BudgetBreach != *want {
		t.Errorf("BudgetBreach = %+v; want %+v", res.BudgetBreach, want)
	}
	status := platform.terminalStatus(t)
	if got := string(status["failureMode"]); got != `"budget-exceeded"` {
		t.Errorf("posted failureMode = %s; want budget-exceeded", got)
	}
	wantMetered(t, res, status, 1_250)
}

// TestRun_BudgetCapAfterDeliveryEndsCompleted is the replayed session that
// crosses the cap after `gh pr create`: the pull request is verified and the
// turn result says passed, so the session ends completed with the breach
// flag rather than failed. The controls stay failed: the cap reached before
// the pull request, before the passed result, or with a pull request that is
// not the session's.
func TestRun_BudgetCapAfterDeliveryEndsCompleted(t *testing.T) {
	opened := concat(
		[]agent.Event{toolUse("pr", "gh pr create --fill"), toolResult("pr", followUpPR)},
		[]agent.Event{callUsage(400, 0)},
	)
	passed := modelCall("Opened "+followUpPR+"\nWORK_RESULT: passed", 100)
	// After delivery the agent keeps polling CI and crosses the cap.
	keepsWorking := modelCall("Checking CI.", 700, "checks")
	neverReached := modelCall("Rebasing onto main.", 50, "rebase")
	cases := []struct {
		name     string
		turn     verdictScriptTurn
		pulls    map[int]string
		complete bool
	}{
		{name: "passed marker", turn: verdictScriptTurn{events: concat(opened, passed, keepsWorking, neverReached)}, complete: true},
		{name: "passed turn-result manifest", turn: verdictScriptTurn{manifest: passedManifest, events: concat(opened, modelCall("Opened "+followUpPR, 100), keepsWorking, neverReached)}, complete: true},
		{name: "cap before the pull request", turn: verdictScriptTurn{events: concat(keepsWorking, modelCall("", 400), opened, passed)}},
		{name: "pull request without a passed result yet", turn: verdictScriptTurn{events: concat(opened, modelCall("Opened "+followUpPR+", now checking CI.", 100), keepsWorking, neverReached)}},
		{name: "pull request that is not the session's", pulls: map[int]string{7: pullAtOtherCommit}, turn: verdictScriptTurn{events: concat(opened, passed, keepsWorking, neverReached)}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			pulls := tc.pulls
			if pulls == nil {
				pulls = map[int]string{7: pullAtSessionCommit}
			}
			platform := newRecordingPlatformServer(t)
			res, _ := runScriptedSession(t, scriptedSession{
				workType:   "development",
				repository: followUpRepository,
				pulls:      pulls,
				budget:     tokenBudget(),
				platform:   platform,
				turns:      []verdictScriptTurn{tc.turn},
			})
			status := platform.terminalStatus(t)
			if res.BudgetBreach == nil || res.BudgetBreach.Cap != string(CapTokens) {
				t.Fatalf("BudgetBreach = %+v; want the max-tokens breach", res.BudgetBreach)
			}
			if res.BudgetReport == nil || res.BudgetReport.CapBreached != CapTokens {
				t.Errorf("BudgetReport = %+v; want CapBreached %s", res.BudgetReport, CapTokens)
			}
			var posted agent.BudgetBreach
			if err := json.Unmarshal(status["budgetBreach"], &posted); err != nil || posted != *res.BudgetBreach {
				t.Errorf("posted budgetBreach = %s (%v); want %+v", status["budgetBreach"], err, *res.BudgetBreach)
			}
			if !tc.complete {
				if res.Status != "failed" || res.FailureMode != FailureBudgetExceeded {
					t.Fatalf("Status=%q FailureMode=%q; want failed/%s", res.Status, res.FailureMode, FailureBudgetExceeded)
				}
				return
			}
			if res.Status != "completed" || res.FailureMode != "" || res.Error != "" {
				t.Fatalf("Status=%q FailureMode=%q Error=%q; delivered work over the cap ends completed", res.Status, res.FailureMode, res.Error)
			}
			if res.PullRequestURL != followUpPR || res.WorkResult != "passed" {
				t.Errorf("PullRequestURL=%q WorkResult=%q; want %s passed", res.PullRequestURL, res.WorkResult, followUpPR)
			}
			if got := string(status["status"]); got != `"completed"` {
				t.Errorf("posted status = %s; want completed", got)
			}
			wantMetered(t, res, status, 1_200)
		})
	}
}

// TestRun_WrapUpAsTheNextFollowUpPrompt pins the wrap-up request on a harness
// with no steer channel: once the meter passes four fifths of the cap, the
// next follow-up prompt asks the agent to wrap up instead of the plain
// continuation, once.
func TestRun_WrapUpAsTheNextFollowUpPrompt(t *testing.T) {
	res, provider := runScriptedSession(t, scriptedSession{
		workType:   "development",
		repository: followUpRepository,
		pulls:      map[int]string{7: pullAtSessionCommit},
		budget:     tokenBudget(),
		turns: []verdictScriptTurn{
			{events: concat(modelCall("", 500, "a"), modelCall("Now I will run the tests.", 350, "b"))},
			{events: modelCall("Tests pass.", 40, "c")},
			{events: modelCall("Opened "+followUpPR+"\nWORK_RESULT: passed", 30, "pr")},
		},
	})
	if res.Status != "completed" || res.BudgetBreach != nil {
		t.Fatalf("Status=%q BudgetBreach=%+v (%s: %s); want completed within budget", res.Status, res.BudgetBreach, res.FailureMode, res.Error)
	}
	wantPrompts(t, provider.prompts, wrapUpPrompt, continuePrompt)
	wantMetered(t, res, nil, 920)
}

// TestRun_WrapUpDeliveredMidTurn pins the wrap-up request on a harness that
// takes a message into a running turn: at the first tool call after the
// meter passed the wrap-up point, the request is injected; the agent opens
// the pull request within budget instead of running on into the cap.
func TestRun_WrapUpDeliveredMidTurn(t *testing.T) {
	var provider *steerScriptProvider
	res, _ := runScriptedSession(t, scriptedSession{
		workType:   "development",
		repository: followUpRepository,
		pulls:      map[int]string{7: pullAtSessionCommit},
		budget:     tokenBudget(),
		provider: func(base agent.HarnessProvider) agent.Provider {
			provider = &steerScriptProvider{
				HarnessProvider: base,
				calls: [][]agent.Event{
					modelCall("Reading the code.", 500, "a"),
					modelCall("", 350, "b"), // passes 800 tokens
					modelCall("", 100, "c"), // the request is injected at this call's tool call
					modelCall("Refactoring the parser too.", 300, "d"),
					modelCall("Opened "+followUpPR+"\nWORK_RESULT: passed", 50, "pr"),
				},
				onSteer: [][]agent.Event{
					concat([]agent.Event{toolUse("pr", "gh pr create --fill"), toolResult("pr", followUpPR)}, []agent.Event{callUsage(30, 0)}),
					modelCall("Opened "+followUpPR+"\nWORK_RESULT: passed", 10),
				},
			}
			return provider
		},
	})
	if res.Status != "completed" || res.BudgetBreach != nil || res.PullRequestURL != followUpPR {
		t.Fatalf("Status=%q BudgetBreach=%+v PullRequestURL=%q (%s: %s); want completed within budget with %s",
			res.Status, res.BudgetBreach, res.PullRequestURL, res.FailureMode, res.Error, followUpPR)
	}
	wantPrompts(t, provider.injected(), wrapUpPrompt)
	wantMetered(t, res, nil, 990)
}

// TestRun_CostCountsEveryTurn pins the reported cost against the budget
// meter: every turn counts once — a follow-up turn of a harness that reports
// per-turn usage adds to the first, a running total is not counted twice, and
// the turn the cap stopped counts the model calls it spent, although no
// ResultEvent reported them. A cost carried only by the last ResultEvent
// dropped the earlier turns, and on a budget stop the last turn too.
func TestRun_CostCountsEveryTurn(t *testing.T) {
	cases := []struct {
		name   string
		budget *prompt.StageBudget
		turns  []verdictScriptTurn
		status string
		want   agent.CostData
	}{
		{
			name:   "cap stops a continued turn",
			budget: tokenBudget(),
			turns: []verdictScriptTurn{
				{events: concat(modelCall("", 300, "a"), modelCall("Now I will run the tests.", 200, "b")), cost: &agent.CostData{InputTokens: 500, TotalCostUsd: 0.05, NumTurns: 2}},
				{events: concat(modelCall("", 400, "c"), modelCall("", 250, "d"), modelCall("never reached", 50)), cost: &agent.CostData{InputTokens: 1_200, TotalCostUsd: 0.12, NumTurns: 5}},
			},
			status: "failed",
			want:   agent.CostData{InputTokens: 1_150, TotalCostUsd: 0.05, NumTurns: 4},
		},
		{
			name: "per-turn usage without a budget sums",
			turns: []verdictScriptTurn{
				{text: "Now I will run the tests.", cost: &agent.CostData{InputTokens: 300, OutputTokens: 100, CachedInputTokens: 50, TotalCostUsd: 0.01, NumTurns: 1}},
				{text: "Opened " + followUpPR + "\nWORK_RESULT: passed", cost: &agent.CostData{InputTokens: 200, OutputTokens: 50, TotalCostUsd: 0.02, NumTurns: 1}},
			},
			status: "completed",
			want:   agent.CostData{InputTokens: 500, OutputTokens: 150, CachedInputTokens: 50, TotalCostUsd: 0.03, NumTurns: 2},
		},
		{
			name:   "a running total counts once",
			budget: tokenBudget(),
			turns: []verdictScriptTurn{
				{events: modelCall("Now I will run the tests.", 300, "a"), cost: &agent.CostData{InputTokens: 300, TotalCostUsd: 0.03, NumTurns: 1}},
				{events: modelCall("Opened "+followUpPR+"\nWORK_RESULT: passed", 200, "pr"), cost: &agent.CostData{InputTokens: 500, TotalCostUsd: 0.05, NumTurns: 2}},
			},
			status: "completed",
			want:   agent.CostData{InputTokens: 500, TotalCostUsd: 0.05, NumTurns: 2},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			platform := newRecordingPlatformServer(t)
			res, _ := runScriptedSession(t, scriptedSession{
				workType:   "development",
				repository: followUpRepository,
				pulls:      map[int]string{7: pullAtSessionCommit},
				budget:     tc.budget,
				platform:   platform,
				turns:      tc.turns,
			})
			if res.Status != tc.status {
				t.Fatalf("Status = %q (%s: %s); want %q", res.Status, res.FailureMode, res.Error, tc.status)
			}
			if res.Cost == nil {
				t.Fatalf("Cost = nil; want %+v", tc.want)
			}
			got := *res.Cost
			if got.InputTokens != tc.want.InputTokens || got.OutputTokens != tc.want.OutputTokens ||
				got.CachedInputTokens != tc.want.CachedInputTokens || got.NumTurns != tc.want.NumTurns ||
				fmt.Sprintf("%.4f", got.TotalCostUsd) != fmt.Sprintf("%.4f", tc.want.TotalCostUsd) {
				t.Errorf("Cost = %+v; want %+v", got, tc.want)
			}
			status := platform.terminalStatus(t)
			wantMetered(t, res, status, tc.want.InputTokens+tc.want.OutputTokens)
			if got, want := string(status["outputTokens"]), fmt.Sprint(tc.want.OutputTokens); tc.want.OutputTokens > 0 && got != want {
				t.Errorf("posted outputTokens = %s; want %s", got, want)
			}
		})
	}
}

// TestAtTurnBoundary pins where a turn the token cap ended is stopped.
func TestAtTurnBoundary(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name     string
		ev       agent.Event
		inFlight int
		want     bool
	}{
		{name: "turn end", ev: agent.ResultEvent{Success: true}, want: true},
		{name: "model call usage, nothing running", ev: callUsage(10, 0), want: true},
		{name: "model call usage while a tool call runs", ev: callUsage(10, 0), inFlight: 1},
		{name: "usage derived from the turn's result", ev: agent.LlmCallEvent{InputTokens: 10, UsageSource: agent.LlmUsageAggregate, Synthetic: true}},
		{name: "a tool call's result", ev: toolResult("a", "ok")},
		{name: "a message", ev: say("working")},
		{name: "a tool call", ev: toolUse("a", "ls")},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := atTurnBoundary(tc.ev, tc.inFlight); got != tc.want {
				t.Errorf("atTurnBoundary = %v; want %v", got, tc.want)
			}
		})
	}
}

// TestTakesMidTurnWrapUp pins that only a steer channel takes the wrap-up
// request into a running turn.
func TestTakesMidTurnWrapUp(t *testing.T) {
	t.Parallel()
	for _, nd := range []agent.NoticeDelivery{
		"", agent.NoticeDeliveryNone, agent.NoticeDeliveryHook, agent.NoticeDeliveryMCPRPC, agent.NoticeDeliveryHTTPSession,
		agent.NoticeDeliveryACP, agent.NoticeDeliveryResumeInject, agent.NoticeDeliveryInBoxLoop, agent.NoticeDeliveryPTYNotice,
	} {
		if takesMidTurnWrapUp(nd) {
			t.Errorf("takesMidTurnWrapUp(%q) = true; want false", nd)
		}
	}
	if !takesMidTurnWrapUp(agent.NoticeDeliveryRPCSteer) {
		t.Errorf("takesMidTurnWrapUp(%q) = false; want true", agent.NoticeDeliveryRPCSteer)
	}
}

// steerScriptProvider plays one turn, model call by model call, on a harness
// that declares a steer channel: a message injected while the turn runs
// reaches the agent after the current call's tool calls, before its next
// call (pi's steer), and the agent then plays onSteer instead of the rest of
// calls. The stream is unbuffered, so the runner has handled every event of
// a call before the handle decides what the next call is.
type steerScriptProvider struct {
	agent.HarnessProvider
	calls   [][]agent.Event
	onSteer [][]agent.Event

	mu      sync.Mutex
	prompts []string
}

func (p *steerScriptProvider) Manifest() agent.HarnessManifest {
	m := p.HarnessProvider.Manifest()
	m.Caps.NoticeDelivery = agent.NoticeDeliveryRPCSteer
	return m
}

func (p *steerScriptProvider) Spawn(context.Context, agent.Spec) (agent.Handle, error) {
	h := &steerScriptHandle{p: p, events: make(chan agent.Event), stop: make(chan struct{}), done: make(chan struct{})}
	go h.run()
	return h, nil
}

func (p *steerScriptProvider) Resume(context.Context, string, agent.Spec) (agent.Handle, error) {
	return nil, agent.ErrUnsupported
}

func (p *steerScriptProvider) injected() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.prompts...)
}

type steerScriptHandle struct {
	p        *steerScriptProvider
	events   chan agent.Event
	stop     chan struct{}
	stopOnce sync.Once
	done     chan struct{}

	mu      sync.Mutex
	steered bool
	ended   bool
}

func (h *steerScriptHandle) SessionID() string          { return "steer-session" }
func (h *steerScriptHandle) Events() <-chan agent.Event { return h.events }

func (h *steerScriptHandle) Inject(_ context.Context, text string) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.ended {
		return errors.New("scripted: the turn has ended")
	}
	h.steered = true
	h.p.mu.Lock()
	h.p.prompts = append(h.p.prompts, text)
	h.p.mu.Unlock()
	return nil
}

func (h *steerScriptHandle) Stop(context.Context) error {
	h.stopOnce.Do(func() { close(h.stop) })
	<-h.done
	return nil
}

func (h *steerScriptHandle) send(ev agent.Event) bool {
	select {
	case h.events <- ev:
		return true
	case <-h.stop:
		return false
	}
}

// takeSteer reports, once, that a message arrived during the turn.
func (h *steerScriptHandle) takeSteer() bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	steered := h.steered
	h.steered = false
	return steered
}

func (h *steerScriptHandle) run() {
	defer close(h.done)
	defer close(h.events)
	if !h.send(agent.InitEvent{SessionID: "steer-session"}) {
		return
	}
	calls, total := h.p.calls, int64(0)
	for i := 0; i < len(calls); i++ {
		for _, ev := range calls[i] {
			if usage, ok := ev.(agent.LlmCallEvent); ok {
				total += usage.InputTokens + usage.OutputTokens
			}
			if !h.send(ev) {
				return
			}
		}
		if h.takeSteer() {
			calls, i = h.p.onSteer, -1
		}
	}
	h.mu.Lock()
	h.ended = true
	h.mu.Unlock()
	// The turn's terminal carries the running total of the calls it made,
	// as pi's does; the session then idles until it is stopped.
	if !h.send(agent.ResultEvent{Success: true, Cost: &agent.CostData{InputTokens: total}}) {
		return
	}
	<-h.stop
}

// TestRun_StepHeartbeatsCarryNonDecreasingUsage drives two turns through the
// real runner and asserts the step-heartbeat beats the platform double
// received carry non-decreasing cumulative usage that is non-zero after the
// first turn ends. The runner's emitter reads the budget accumulator live,
// so the usage on the wire is the same total the terminal result reports.
func TestRun_StepHeartbeatsCarryNonDecreasingUsage(t *testing.T) {
	platform := newRecordingPlatformServer(t)
	res, _ := runScriptedSession(t, scriptedSession{
		workType:              "development",
		repository:            followUpRepository,
		pulls:                 map[int]string{7: pullAtSessionCommit},
		platform:              platform,
		stepHeartbeatInterval: 5 * time.Millisecond,
		turns: []verdictScriptTurn{
			{text: "Now I will run the tests.", cost: &agent.CostData{InputTokens: 300, OutputTokens: 100, CachedInputTokens: 50, TotalCostUsd: 0.01, NumTurns: 1}},
			{text: "Opened " + followUpPR + "\nWORK_RESULT: passed", cost: &agent.CostData{InputTokens: 500, OutputTokens: 150, CachedInputTokens: 50, TotalCostUsd: 0.03, NumTurns: 2}},
		},
	})
	if res.Status != "completed" {
		t.Fatalf("Status = %q (%s: %s); want completed", res.Status, res.FailureMode, res.Error)
	}

	type usage struct {
		Input  int64   `json:"inputTokens"`
		Output int64   `json:"outputTokens"`
		Cached int64   `json:"cachedInputTokens"`
		Cost   float64 `json:"totalCostUsd"`
	}
	var (
		beats      []usage
		sawNonZero bool
	)
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		beats = beats[:0]
		sawNonZero = false
		for _, raw := range platform.stepBeats() {
			var body struct {
				Usage *usage `json:"usage"`
			}
			if err := json.Unmarshal(raw, &body); err != nil {
				t.Fatalf("step-heartbeat body is not JSON: %v", err)
			}
			if body.Usage == nil {
				continue
			}
			beats = append(beats, *body.Usage)
			if body.Usage.Input+body.Usage.Output > 0 {
				sawNonZero = true
			}
		}
		// Need at least two usage-carrying beats to pin non-decreasing.
		if len(beats) >= 2 && sawNonZero {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if len(beats) < 2 {
		t.Fatalf("got %d usage-carrying step-heartbeats; want at least 2", len(beats))
	}
	if !sawNonZero {
		t.Fatal("no step-heartbeat carried non-zero usage after the first turn")
	}
	for i := 1; i < len(beats); i++ {
		if beats[i].Input < beats[i-1].Input || beats[i].Output < beats[i-1].Output ||
			beats[i].Cached < beats[i-1].Cached || beats[i].Cost < beats[i-1].Cost {
			t.Fatalf("step-heartbeat usage decreased: %+v -> %+v", beats[i-1], beats[i])
		}
	}
	last := beats[len(beats)-1]
	if res.Cost == nil {
		t.Fatal("Cost = nil; want the metered total")
	}
	if last.Input != res.Cost.InputTokens || last.Output != res.Cost.OutputTokens ||
		last.Cached != res.Cost.CachedInputTokens || last.Cost != res.Cost.TotalCostUsd {
		t.Errorf("last step-heartbeat usage = %+v; want the terminal cost %+v", last, *res.Cost)
	}
}
