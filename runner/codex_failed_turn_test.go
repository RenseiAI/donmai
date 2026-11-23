package runner

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/RenseiAI/donmai/agent"
	"github.com/RenseiAI/donmai/provider/harness/stub"
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

func newCodexFailedTurnHandle(progress string, terminalErrors []string) *codexFailedTurnHandle {
	ch := make(chan agent.Event, 8)
	ch <- agent.InitEvent{SessionID: "codex-failed-turn"}
	// Interim progress note: without the fix the terminal reason falls back
	// to this line.
	ch <- agent.AssistantTextEvent{Text: progress}
	// Mapped form of turn/moderationMetadata.
	ch <- agent.SystemEvent{Subtype: "codex_moderation", Message: "Codex turn moderation: {\"review\":true}"}
	// Mapped form of the error notification.
	ch <- agent.SystemEvent{Subtype: "codex_error", Message: "Codex error"}
	ch <- agent.ResultEvent{Success: false, Errors: terminalErrors, ErrorSubtype: "turn_failed"}
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
		wantMode       string
		wantError      string
	}{
		{
			name:           "provider refusal is the failure reason",
			terminalErrors: []string{refusal},
			wantMode:       FailureProviderError,
			wantError:      refusal,
		},
		{
			name:           "synthesized fallback keeps silent-exit",
			terminalErrors: []string{"Turn failed"},
			wantMode:       FailureSilentExit,
		},
		{
			name:           "no error text keeps silent-exit",
			terminalErrors: nil,
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
				spawnHandle:     newCodexFailedTurnHandle(progress, tc.terminalErrors),
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
