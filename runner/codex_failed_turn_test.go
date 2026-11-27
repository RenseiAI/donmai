package runner

import (
	"context"
	"io"
	"log/slog"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/RenseiAI/donmai/agent"
	"github.com/RenseiAI/donmai/provider/harness/stub"
	"github.com/RenseiAI/donmai/result"
	"github.com/RenseiAI/donmai/runtime/worktree"
	"github.com/RenseiAI/donmai/stagewire"
)

// codexFailedTurnHandle is a scripted app-server-like session: it replays
// the agent.Events the codex mapper emits for a moderation warning, an
// error notification and a failed turn/completed, in order. The terminal
// ResultEvent carries the table's Errors; the assistant progress note stays
// on the stream so the test can prove the failure reason does not fall back
// to it.
type codexFailedTurnHandle struct {
	events chan agent.Event
}

func (h *codexFailedTurnHandle) SessionID() string { return "codex-failed-turn" }

func (h *codexFailedTurnHandle) Events() <-chan agent.Event { return h.events }

func (h *codexFailedTurnHandle) Inject(context.Context, string) error {
	return agent.ErrUnsupported
}

func (h *codexFailedTurnHandle) Stop(context.Context) error { return nil }

func newCodexFailedTurnHandle(progress string, terminalErrors []string, subtype string) *codexFailedTurnHandle {
	if subtype == "" {
		subtype = "turn_failed"
	}
	ch := make(chan agent.Event, 8)
	ch <- agent.InitEvent{SessionID: "codex-failed-turn"}
	// Interim progress note: without the fix the terminal reason falls back
	// to this line.
	ch <- agent.AssistantTextEvent{Text: progress}
	// Mapped form of turn/moderationMetadata.
	ch <- agent.SystemEvent{Subtype: "codex_moderation", Message: "Codex moderation blocked: prompt/cyber_security"}
	// Mapped form of the error notification.
	ch <- agent.SystemEvent{Subtype: "codex_error", Message: "Codex error"}
	ch <- agent.ResultEvent{Success: false, Errors: terminalErrors, ErrorSubtype: subtype}
	close(ch)
	return &codexFailedTurnHandle{events: ch}
}

// TestRun_CodexFailedTurnReportsProviderError pins that a failed codex turn
// carrying the provider's error text ends as a provider-error failure whose
// reason is that text — not the last progress note — while a failed terminal
// with no error text keeps silent-exit.
func TestRun_CodexFailedTurnReportsProviderError(t *testing.T) {
	t.Parallel()
	const progress = "I'll now check the repository layout"
	const refusal = "this session was not authenticated with one of your authorized hardware keys"

	tests := []struct {
		name           string
		terminalErrors []string
		subtype        string
		wantMode       string
		wantError      string
	}{
		{
			name:           "provider refusal is the failure reason",
			terminalErrors: []string{refusal},
			subtype:        "turn_failed",
			wantMode:       FailureProviderError,
			wantError:      refusal,
		},
		{
			name:           "synthesized fallback keeps silent-exit",
			terminalErrors: []string{"Turn failed"},
			subtype:        "turn_failed",
			wantMode:       FailureSilentExit,
		},
		{
			name:           "no error text keeps silent-exit",
			terminalErrors: nil,
			subtype:        "turn_failed",
			wantMode:       FailureSilentExit,
		},
		{
			name:           "interrupted turn keeps silent-exit",
			terminalErrors: []string{"interrupted by user"},
			subtype:        "interrupted",
			wantMode:       FailureSilentExit,
		},
		{
			name:           "turn cap keeps silent-exit",
			terminalErrors: []string{"max turns exceeded"},
			subtype:        "error_max_turns",
			wantMode:       FailureSilentExit,
		},
	}
	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			h := newRunnerHarness(t)
			base, err := stub.New()
			if err != nil {
				t.Fatalf("stub.New: %v", err)
			}
			hp, ok := base.(agent.HarnessProvider)
			if !ok {
				t.Fatal("stub provider does not satisfy HarnessProvider")
			}
			provider := &evidenceProvider{
				HarnessProvider: hp,
				spawnHandle:     newCodexFailedTurnHandle(progress, tc.terminalErrors, tc.subtype),
			}
			reg := NewRegistry()
			if err := reg.Register(provider); err != nil {
				t.Fatalf("Register: %v", err)
			}
			h.runner.registry = reg
			qw := h.queuedWork("CODEX-FAILED-TURN")
			qw.WorkType = "qa"
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			res, err := h.runner.Run(ctx, qw)
			if err != nil {
				t.Fatalf("Run: %v", err)
			}
			if res.Status != "failed" {
				t.Fatalf("Status = %q; want failed (mode=%q error=%q)", res.Status, res.FailureMode, res.Error)
			}
			if res.FailureMode != tc.wantMode {
				t.Fatalf("FailureMode = %q; want %q (Error=%q OutputTail=%q)", res.FailureMode, tc.wantMode, res.Error, res.OutputTail)
			}
			if tc.wantError != "" {
				if res.Error != tc.wantError {
					t.Fatalf("Error = %q; want %q", res.Error, tc.wantError)
				}
				if !strings.Contains(res.OutputTail, tc.wantError) {
					t.Fatalf("OutputTail = %q; want it to contain %q", res.OutputTail, tc.wantError)
				}
				if res.Error == progress {
					t.Fatalf("Error fell back to the progress note %q", progress)
				}
				if res.Summary != progress {
					t.Fatalf("Summary = %q; want the progress note %q (the terminal carries no message)", res.Summary, progress)
				}
			} else if res.FailureMode == FailureProviderError {
				t.Fatalf("FailureMode = provider-error with no error text (Error=%q)", res.Error)
			}
		})
	}
}

// outputCapHandle replays the terminal shape a mapper emits when the model
// hits a configured output cap: a failed ResultEvent whose Errors carries
// the finish reason with an ErrorSubtype of "finish_<REASON>" (here
// Gemini "MAX_TOKENS"). It carries no tool call, so the run is eligible
// for the deferred provider-error transition — which must NOT apply: a cap
// is a truncation, not a provider failure.
type outputCapHandle struct {
	events chan agent.Event
}

func (h *outputCapHandle) SessionID() string { return "output-cap" }

func (h *outputCapHandle) Events() <-chan agent.Event { return h.events }

func (h *outputCapHandle) Inject(context.Context, string) error {
	return agent.ErrUnsupported
}

func (h *outputCapHandle) Stop(context.Context) error { return nil }

// TestRun_OutputCapKeepsFailureTransition pins that a finish-reason cap
// stays a silent-exit failure with its diagnostic preserved and takes the
// declared failure transition, even when the work item opts into deferring
// provider-error failures. It drives the production entry point
// (Runner.Run) with stage transitions so the deferred-vs-failure branch is
// observed through the real state writes, not only the mode string.
func TestRun_OutputCapKeepsFailureTransition(t *testing.T) {
	t.Parallel()
	srv, calls, mu := stubProxyServer(t)
	wtm, err := worktree.NewManager(worktree.Options{ParentDir: t.TempDir()})
	if err != nil {
		t.Fatalf("worktree.NewManager: %v", err)
	}
	poster, err := result.NewPoster(result.Options{
		PlatformURL: srv.URL, WorkerID: "wkr-cap", AuthToken: "tok-cap", HTTPClient: srv.Client(), BaseDelay: 1,
	})
	if err != nil {
		t.Fatalf("result.NewPoster: %v", err)
	}
	base, err := stub.New()
	if err != nil {
		t.Fatalf("stub.New: %v", err)
	}
	hp, ok := base.(agent.HarnessProvider)
	if !ok {
		t.Fatal("stub provider does not satisfy HarnessProvider")
	}
	ch := make(chan agent.Event, 8)
	ch <- agent.InitEvent{SessionID: "output-cap"}
	ch <- agent.AssistantTextEvent{Text: "partial output..."}
	ch <- agent.ResultEvent{Success: false, Errors: []string{"MAX_TOKENS"}, ErrorSubtype: "finish_MAX_TOKENS"}
	close(ch)
	provider := &evidenceProvider{HarnessProvider: hp, spawnHandle: &outputCapHandle{events: ch}}
	reg := NewRegistry()
	if err := reg.Register(provider); err != nil {
		t.Fatalf("Register: %v", err)
	}
	r, err := New(Options{
		Registry: reg, WorktreeManager: wtm, Poster: poster, HTTPClient: srv.Client(),
		Logger:                 slog.New(slog.NewTextHandler(io.Discard, nil)),
		SkipBackstop:           true,
		SkipSteering:           true,
		PreserveWorktreeAlways: true,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	qw := QueuedWork{
		QueuedWork:             queuedWorkBase("OUTPUT-CAP"),
		WorkerID:               "wkr-cap",
		AuthToken:              "tok-cap",
		PlatformURL:            srv.URL,
		ResolvedProfile:        ResolvedProfile{Provider: agent.ProviderStub},
		DeferFailureTransition: true,
	}
	qw.WorkType = WorkTypeQAStr
	qw.Repository = makeBareRepo(t)
	qw.Stage = &stagewire.Stage{ID: "stg", Key: "verify", Name: "Verify", Role: stagewire.RoleReview}
	qw.Card = &stagewire.Card{ID: "card", Handle: "reviewer", Contract: &stagewire.Contract{Verdict: true, WorkResult: true}}
	qw.Transitions = stageTransitionsFixture()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	res, err := r.Run(ctx, qw)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res == nil {
		t.Fatal("Run returned nil result")
	}
	if res.Status != "failed" {
		t.Fatalf("Status = %q; want failed (mode=%q error=%q)", res.Status, res.FailureMode, res.Error)
	}
	if res.FailureMode != FailureSilentExit {
		t.Fatalf("FailureMode = %q; want %q (a cap is not a provider failure; Error=%q OutputTail=%q)", res.FailureMode, FailureSilentExit, res.Error, res.OutputTail)
	}
	if res.Error != "" {
		t.Fatalf("Error = %q; want empty (the cap diagnostic rides OutputTail, not the retry-bearing Error)", res.Error)
	}
	if res.Resumable {
		t.Fatal("Resumable = true; want false (only provider-error failures are resumable)")
	}
	if res.ToolCalls != 0 {
		t.Fatalf("ToolCalls = %d; want 0 (the cap turn made no tool call)", res.ToolCalls)
	}
	if !strings.Contains(res.OutputTail, "MAX_TOKENS") {
		t.Fatalf("OutputTail = %q; want it to preserve the MAX_TOKENS diagnostic", res.OutputTail)
	}
	if res.StageTransition == nil {
		t.Fatal("StageTransition is nil; want the failure transition record")
	}
	if res.StageTransition.Reason == "deferred-provider-error" {
		t.Fatalf("StageTransition reason = deferred-provider-error; want the failure transition (states=%v)", res.StageTransition)
	}
	if res.StageTransition.Outcome != string(stagewire.OutcomeFailure) || !res.StageTransition.Succeeded {
		t.Fatalf("StageTransition = %+v; want failure outcome succeeded", res.StageTransition)
	}
	if res.LinearStatusTransition != nil {
		t.Fatalf("legacy post-session ran: %+v", res.LinearStatusTransition)
	}
	states, _ := stateWrites(t, calls, mu)
	if !reflect.DeepEqual(states, []string{"state-start", "state-failure"}) {
		t.Fatalf("state writes = %v; want [state-start state-failure] (the failure transition ran)", states)
	}
}

// TestRun_NonProviderTerminalKeepsFailureTransition pins that an
// interrupted turn (a host reporting turn/completed cancelled before a tool
// ran) and a turn-cap stop (error_max_turns from the checked-in CLI result)
// stay silent-exit failures with the declared failure transition, even when
// the work item opts into deferring provider-error failures. It drives the
// production entry point (Runner.Run) with stage transitions so the
// deferred-vs-failure branch is observed through the real state writes, not
// only the mode string.
func TestRun_NonProviderTerminalKeepsFailureTransition(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		subtype  string
		errors   []string
		tailWant string
	}{
		{name: "interrupted by user before tool runs", subtype: "interrupted", errors: []string{"interrupted by user"}, tailWant: "interrupted by user"},
		{name: "turn cap is not a provider failure", subtype: "error_max_turns", errors: []string{"max turns exceeded"}, tailWant: "max turns exceeded"},
	}
	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			srv, calls, mu := stubProxyServer(t)
			wtm, err := worktree.NewManager(worktree.Options{ParentDir: t.TempDir()})
			if err != nil {
				t.Fatalf("worktree.NewManager: %v", err)
			}
			poster, err := result.NewPoster(result.Options{
				PlatformURL: srv.URL, WorkerID: "wkr-cap", AuthToken: "tok-cap", HTTPClient: srv.Client(), BaseDelay: 1,
			})
			if err != nil {
				t.Fatalf("result.NewPoster: %v", err)
			}
			base, err := stub.New()
			if err != nil {
				t.Fatalf("stub.New: %v", err)
			}
			hp, ok := base.(agent.HarnessProvider)
			if !ok {
				t.Fatal("stub provider does not satisfy HarnessProvider")
			}
			ch := make(chan agent.Event, 8)
			ch <- agent.InitEvent{SessionID: "non-provider-terminal"}
			ch <- agent.AssistantTextEvent{Text: "partial output..."}
			ch <- agent.ResultEvent{Success: false, Errors: tc.errors, ErrorSubtype: tc.subtype}
			close(ch)
			provider := &evidenceProvider{HarnessProvider: hp, spawnHandle: &outputCapHandle{events: ch}}
			reg := NewRegistry()
			if err := reg.Register(provider); err != nil {
				t.Fatalf("Register: %v", err)
			}
			r, err := New(Options{
				Registry: reg, WorktreeManager: wtm, Poster: poster, HTTPClient: srv.Client(),
				Logger:                 slog.New(slog.NewTextHandler(io.Discard, nil)),
				SkipBackstop:           true,
				SkipSteering:           true,
				PreserveWorktreeAlways: true,
			})
			if err != nil {
				t.Fatalf("New: %v", err)
			}
			qw := QueuedWork{
				QueuedWork:             queuedWorkBase("NON-PROVIDER-TERMINAL"),
				WorkerID:               "wkr-cap",
				AuthToken:              "tok-cap",
				PlatformURL:            srv.URL,
				ResolvedProfile:        ResolvedProfile{Provider: agent.ProviderStub},
				DeferFailureTransition: true,
			}
			qw.WorkType = WorkTypeQAStr
			qw.Repository = makeBareRepo(t)
			qw.Stage = &stagewire.Stage{ID: "stg", Key: "verify", Name: "Verify", Role: stagewire.RoleReview}
			qw.Card = &stagewire.Card{ID: "card", Handle: "reviewer", Contract: &stagewire.Contract{Verdict: true, WorkResult: true}}
			qw.Transitions = stageTransitionsFixture()
			ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
			defer cancel()
			res, err := r.Run(ctx, qw)
			if err != nil {
				t.Fatalf("Run: %v", err)
			}
			if res == nil {
				t.Fatal("Run returned nil result")
			}
			if res.Status != "failed" {
				t.Fatalf("Status = %q; want failed (mode=%q error=%q)", res.Status, res.FailureMode, res.Error)
			}
			if res.FailureMode != FailureSilentExit {
				t.Fatalf("FailureMode = %q; want %q (subtype %q is not a provider failure; Error=%q OutputTail=%q)", res.FailureMode, FailureSilentExit, tc.subtype, res.Error, res.OutputTail)
			}
			if res.Error != "" {
				t.Fatalf("Error = %q; want empty (the diagnostic rides OutputTail, not the retry-bearing Error)", res.Error)
			}
			if res.Resumable {
				t.Fatal("Resumable = true; want false (only provider-error failures are resumable)")
			}
			if res.ToolCalls != 0 {
				t.Fatalf("ToolCalls = %d; want 0 (the turn made no tool call)", res.ToolCalls)
			}
			if !strings.Contains(res.OutputTail, tc.tailWant) {
				t.Fatalf("OutputTail = %q; want it to preserve %q", res.OutputTail, tc.tailWant)
			}
			if res.StageTransition == nil {
				t.Fatal("StageTransition is nil; want the failure transition record")
			}
			if res.StageTransition.Reason == "deferred-provider-error" {
				t.Fatalf("StageTransition reason = deferred-provider-error; want the failure transition (states=%v)", res.StageTransition)
			}
			if res.StageTransition.Outcome != string(stagewire.OutcomeFailure) || !res.StageTransition.Succeeded {
				t.Fatalf("StageTransition = %+v; want failure outcome succeeded", res.StageTransition)
			}
			if res.LinearStatusTransition != nil {
				t.Fatalf("legacy post-session ran: %+v", res.LinearStatusTransition)
			}
			states, _ := stateWrites(t, calls, mu)
			if !reflect.DeepEqual(states, []string{"state-start", "state-failure"}) {
				t.Fatalf("state writes = %v; want [state-start state-failure] (the failure transition ran)", states)
			}
		})
	}
}
