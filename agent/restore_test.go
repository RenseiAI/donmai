package agent

import (
	"errors"
	"strings"
	"testing"
)

func TestDecideRestore_ResumedWhenHarnessAcceptsToken(t *testing.T) {
	t.Parallel()

	var probed string
	receipt := DecideRestore(RestoreRequest{ResumeToken: "sess-1", Harness: HarnessClaudeCode}, func(token string) error {
		probed = token
		return nil
	}, nil)
	if receipt.Outcome != RestoreResumed {
		t.Fatalf("outcome = %q, want %q", receipt.Outcome, RestoreResumed)
	}
	if probed != "sess-1" {
		t.Fatalf("probed token = %q, want %q", probed, "sess-1")
	}
	if receipt.ResumeToken != "sess-1" || receipt.FreshReason != "" {
		t.Fatalf("resumed receipt must echo the token and set no fresh reason: %+v", receipt)
	}
}

func TestDecideRestore_NoTokenWithoutFlagAwaitDecision(t *testing.T) {
	t.Parallel()

	called := false
	receipt := DecideRestore(RestoreRequest{}, func(string) error { called = true; return nil }, nil)
	if receipt.Outcome != RestoreAwaitingDecision {
		t.Fatalf("outcome = %q, want %q", receipt.Outcome, RestoreAwaitingDecision)
	}
	if called {
		t.Fatal("no token must never probe the harness")
	}
}

func TestDecideRestore_NoTokenWithExplicitFlagFreshPrimed(t *testing.T) {
	t.Parallel()

	receipt := DecideRestore(RestoreRequest{AllowFresh: true}, func(string) error {
		t.Fatal("explicit fresh with no token must not probe")
		return nil
	}, nil)
	if receipt.Outcome != RestoreFreshPrimed {
		t.Fatalf("outcome = %q, want %q", receipt.Outcome, RestoreFreshPrimed)
	}
	if receipt.FreshReason != FreshReasonExplicitFlag {
		t.Fatalf("fresh reason = %q, want %q", receipt.FreshReason, FreshReasonExplicitFlag)
	}
}

func TestDecideRestore_ExplicitFlagOverridesTokenWithoutProbing(t *testing.T) {
	t.Parallel()

	receipt := DecideRestore(RestoreRequest{ResumeToken: "sess-1", AllowFresh: true}, func(string) error {
		t.Fatal("explicit fresh must not probe the held token")
		return nil
	}, nil)
	if receipt.Outcome != RestoreFreshPrimed || receipt.FreshReason != FreshReasonExplicitFlag {
		t.Fatalf("explicit flag must win with reason: %+v", receipt)
	}
	if receipt.ResumeToken != "sess-1" {
		t.Fatalf("receipt must still name the overridden token: %+v", receipt)
	}
}

func TestDecideRestore_NotFoundFreshPrimedWithReason(t *testing.T) {
	t.Parallel()

	for name, err := range map[string]error{
		"typed sentinel":   errors.New("thread/resume: agent: session id not found"),
		"claude CLI shape": errors.New("exit status 1: No conversation found with session ID: abc-123"),
		"codex shape":      errors.New("codex jsonrpc: thread/resume failed (code=-32600): no rollout found for thread id abc-123"),
	} {
		// Wrap the typed case so errors.Is must traverse a chain.
		if name == "typed sentinel" {
			err = &wrappingError{msg: "headless resume", cause: ErrSessionNotFound}
		}
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			receipt := DecideRestore(RestoreRequest{ResumeToken: "abc-123"}, nil, err)
			if receipt.Outcome != RestoreFreshPrimed {
				t.Fatalf("outcome = %q, want %q", receipt.Outcome, RestoreFreshPrimed)
			}
			if receipt.FreshReason != FreshReasonNoConversation {
				t.Fatalf("fresh reason = %q, want %q", receipt.FreshReason, FreshReasonNoConversation)
			}
		})
	}
}

// wrappingError is a minimal errors.Is chain link: it proves the machine
// unwraps rather than string-matching the typed sentinel.
type wrappingError struct {
	msg   string
	cause error
}

func (e *wrappingError) Error() string { return e.msg + ": " + e.cause.Error() }
func (e *wrappingError) Unwrap() error { return e.cause }

func TestDecideRestore_OtherFailureNeedsAttentionNeverFresh(t *testing.T) {
	t.Parallel()

	receipt := DecideRestore(
		RestoreRequest{ResumeToken: "sess-1"},
		nil,
		errors.New("codex jsonrpc: thread/resume failed (code=-32000): app-server unreachable"),
	)
	if receipt.Outcome != RestoreAttentionRequired {
		t.Fatalf("outcome = %q, want %q (a backend failure must never silently start fresh)", receipt.Outcome, RestoreAttentionRequired)
	}
	if receipt.FreshReason != "" {
		t.Fatalf("attention receipt must set no fresh reason: %+v", receipt)
	}
}

func TestDecideRestore_ProbeErrorBeatsNilAttemptErr(t *testing.T) {
	t.Parallel()

	// attemptErr carries a pre-run probe's failure; when it is nil the
	// machine runs resumeAttempt itself exactly once.
	calls := 0
	receipt := DecideRestore(RestoreRequest{ResumeToken: "sess-1"}, func(string) error {
		calls++
		return errors.New("boom")
	}, nil)
	if receipt.Outcome != RestoreAttentionRequired {
		t.Fatalf("outcome = %q, want %q", receipt.Outcome, RestoreAttentionRequired)
	}
	if calls != 1 {
		t.Fatalf("resumeAttempt calls = %d, want exactly 1", calls)
	}
}

func TestIsNoConversationFound(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		err  error
		want bool
	}{
		{"nil is not not-found", nil, false},
		{"typed sentinel", ErrSessionNotFound, true},
		{"wrapped sentinel", &wrappingError{msg: "resume", cause: ErrSessionNotFound}, true},
		{"claude CLI text", errors.New("No conversation found with session ID: x"), true},
		{"codex rollout text", errors.New("no rollout found for thread id x"), true},
		{"unrelated mentioning sessions", errors.New("too many sessions open"), false},
		{"backend failure", errors.New("connection refused"), false},
		{"unsupported capability", ErrUnsupported, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := IsNoConversationFound(tt.err); got != tt.want {
				t.Fatalf("IsNoConversationFound(%v) = %v, want %v", tt.err, got, tt.want)
			}
		})
	}
}

func TestRestoreReceipt_DetailNeverEchoesMultilineErrors(t *testing.T) {
	t.Parallel()

	receipt := DecideRestore(RestoreRequest{ResumeToken: "sess-1"}, nil,
		errors.New("backend down\nsecond line with secrets\nthird"))
	if strings.Contains(receipt.Detail, "\n") {
		t.Fatalf("detail must be a single line: %q", receipt.Detail)
	}
}
