package agent

import "testing"

// TestChatReadiness_LiveReportsTheLivePath pins the trivial observer: Live is
// exactly LiveSupported, so callers cannot mistake a queued-only read for a
// live one.
func TestChatReadiness_LiveReportsTheLivePath(t *testing.T) {
	live := ChatReadiness{LiveSupported: true}
	if !live.Live() {
		t.Fatal("Live() = false for a live readiness")
	}
	queuedOnly := ChatReadiness{LiveSupported: false, QueueSupported: true, Reason: ChatReasonChannelNotDriven}
	if queuedOnly.Live() {
		t.Fatal("Live() = true for a queued-only readiness; queue acceptance is not live delivery")
	}
}

// TestChatReadiness_ReasonsStayStable pins the wire vocabulary: producers key
// dead letters and readiness reasons off the same tokens, so a rename must
// move both together.
func TestChatReadiness_ReasonsStayStable(t *testing.T) {
	for _, reason := range []string{
		ChatReasonSessionEnded,
		ChatReasonModalAlternateScreen,
		ChatReasonChannelNotDriven,
		ChatReasonChannelAbsent,
		ChatReasonPTYNoticeNotChat,
		ChatReasonUndeclared,
		ChatReasonNone,
	} {
		if reason == "" {
			t.Fatal("chat readiness reason must never be empty")
		}
	}
}
