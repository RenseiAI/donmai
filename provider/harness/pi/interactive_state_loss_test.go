package pi

import "testing"

func TestInteractiveStateLossScanner_ENOENTForOwnSessionJSONLEmitsOnce(t *testing.T) {
	t.Parallel()
	scanner := newInteractiveStateLossScanner("/work/repo/.pi")
	if _, ok := scanner.Observe([]byte("Error: ENO")); ok {
		t.Fatal("partial output emitted a state-loss event before the errno/path evidence arrived")
	}
	event, ok := scanner.Observe([]byte("ENT: no such file or directory, open '/work/repo/.pi/sessions/turn.jsonl'"))
	if !ok {
		t.Fatal("own session JSONL ENOENT did not emit harness_state_lost")
	}
	if event.Subtype != harnessStateLostSubtype || event.Message != harnessStateLostMessage {
		t.Fatalf("event = %+v, want typed harness-state-loss condition", event)
	}
	if _, ok := scanner.Observe([]byte("ENOENT: /work/repo/.pi/sessions/again.jsonl")); ok {
		t.Fatal("state-loss condition emitted more than once")
	}
}

func TestInteractiveStateLossScanner_IgnoresUnrelatedENOENT(t *testing.T) {
	t.Parallel()
	scanner := newInteractiveStateLossScanner("/work/repo/.pi")
	for _, output := range [][]byte{
		[]byte("ENOENT: no such file or directory, open '/work/repo/.pi-cache/session.jsonl'\n"),
		[]byte("ENOENT: no such file or directory, open '/other/repo/.pi/sessions/turn.jsonl'\n"),
		[]byte("open '/work/repo/.pi/sessions/turn.jsonl' succeeded\n"),
	} {
		if event, ok := scanner.Observe(output); ok {
			t.Fatalf("unrelated output emitted %+v", event)
		}
	}
}

func TestInteractiveStateLossScanner_DoesNotCombineSeparateDiagnostics(t *testing.T) {
	t.Parallel()
	scanner := newInteractiveStateLossScanner("/work/repo/.pi")
	if event, ok := scanner.Observe([]byte("ENOENT: open '/elsewhere/ordinary.jsonl'\n")); ok {
		t.Fatalf("unrelated ENOENT emitted %+v", event)
	}
	if event, ok := scanner.Observe([]byte("resuming from /work/repo/.pi/sessions/turn.jsonl\n")); ok {
		t.Fatalf("separate normal path mention combined with earlier ENOENT: %+v", event)
	}
}
