package ptyhost

import (
	"bytes"
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/RenseiAI/donmai/agent"
	"github.com/RenseiAI/donmai/attachwire"
)

// rawCatCommand echoes every byte written to the PTY master straight back out,
// with the line discipline's own echo AND its CR/LF translation disabled — so
// the Output frames are EXACTLY the bytes that were written, in write order.
// That makes it the right instrument for "did this reach the PTY, whole and
// un-spliced".
//
// It prints ptyReadyMarker once raw mode is in force: writing before that
// point would be translated by the still-cooked line discipline, so every
// test waits for the marker first (spawnRawCat).
func rawCatCommand() []string {
	return []string{"/bin/sh", "-c", "stty raw -echo; printf '" + ptyReadyMarker + "'; exec cat"}
}

const ptyReadyMarker = "PTY-RAW-READY\n"

// spawnRawCat spawns the raw echo child and returns it with a subscription
// positioned AFTER the readiness marker, so the caller's first write is the
// first byte in the assertions.
func spawnRawCat(t *testing.T) (*Session, agent.InteractiveSubscription) {
	t.Helper()
	s := mustSpawn(t, Spec{Command: rawCatCommand()})
	sub := subscribeOutput(t, s)
	waitForBytes(t, sub, ptyReadyMarker, 10*time.Second)
	return s, sub
}

// subscribeOutput returns a subscription from the start of the stream.
func subscribeOutput(t *testing.T, s *Session) agent.InteractiveSubscription {
	t.Helper()
	sub, err := s.Subscribe(0)
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	t.Cleanup(func() { _ = sub.Close() })
	return sub
}

// waitForBytes drains sub until the accumulated output contains want, and
// returns everything seen. Fails the test on timeout.
func waitForBytes(t *testing.T, sub agent.InteractiveSubscription, want string, d time.Duration) string {
	t.Helper()
	var seen []byte
	deadline := time.After(d)
	for {
		select {
		case f, ok := <-sub.Frames():
			if !ok {
				t.Fatalf("subscription closed before %q arrived; saw %q", want, seen)
			}
			if f.Type != attachwire.TypeOutput {
				continue
			}
			seen = append(seen, attachwire.DecodeOutput(f.Payload).Data...)
			if bytes.Contains(seen, []byte(want)) {
				return string(seen)
			}
		case <-deadline:
			t.Fatalf("timed out waiting for %q; saw %q", want, seen)
		}
	}
}

// TestSession_TryWriteNoticeRespectsPendingCompose is the composition gate:
// a notice is REFUSED (and nothing is written) while the human has
// unsubmitted bytes in the line editor, and accepted once the line has been
// submitted (CR/LF) or discarded (Ctrl-C / Ctrl-U).
//
// This is the guarantee that keeps a runtime notice from splicing itself into
// a half-typed command.
func TestSession_TryWriteNoticeRespectsPendingCompose(t *testing.T) {
	const notice = "NOTICE-LANDED\n"

	tests := []struct {
		name       string
		humanInput string // written through WriteInput first ("" = none)
		wantWrite  bool
	}{
		{name: "no input yet", wantWrite: true},
		{name: "mid composition", humanInput: "abcdef", wantWrite: false},
		{name: "submitted with CR", humanInput: "abcdef\r", wantWrite: true},
		{name: "submitted with LF", humanInput: "abcdef\n", wantWrite: true},
		{name: "interrupted with ctrl-c", humanInput: "abcdef\x03", wantWrite: true},
		{name: "killed line with ctrl-u", humanInput: "abcdef\x15", wantWrite: true},
		{name: "typed again after submitting", humanInput: "abc\rdef", wantWrite: false},
		{name: "whitespace still counts as composing", humanInput: "  ", wantWrite: false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s, sub := spawnRawCat(t)

			if tc.humanInput != "" {
				if _, err := s.WriteInput([]byte(tc.humanInput)); err != nil {
					t.Fatalf("WriteInput: %v", err)
				}
			}

			written, err := s.TryWriteNotice([]byte(notice))
			if err != nil {
				t.Fatalf("TryWriteNotice: unexpected error %v", err)
			}
			if written != tc.wantWrite {
				t.Fatalf("TryWriteNotice written=%v; want %v", written, tc.wantWrite)
			}

			if tc.wantWrite {
				waitForBytes(t, sub, notice, 10*time.Second)
				return
			}

			// Refused: the human's bytes must have reached the PTY and the
			// notice must NOT have. Waiting for the human echo first makes
			// the absence check meaningful rather than merely early.
			seen := waitForBytes(t, sub, tc.humanInput, 10*time.Second)
			if strings.Contains(seen, notice) {
				t.Fatalf("refused notice still reached the PTY: %q", seen)
			}
		})
	}
}

// TestSession_TryWriteNoticeUnblocksAfterSubmit pins the recovery half of the
// gate: the SAME session refuses while composing and accepts once the line is
// submitted. A gate that latched shut would pass the table above and fail here.
func TestSession_TryWriteNoticeUnblocksAfterSubmit(t *testing.T) {
	const notice = "NOTICE-AFTER-SUBMIT\n"
	s, sub := spawnRawCat(t)

	if _, err := s.WriteInput([]byte("half-typed")); err != nil {
		t.Fatalf("WriteInput: %v", err)
	}
	if written, err := s.TryWriteNotice([]byte(notice)); err != nil || written {
		t.Fatalf("TryWriteNotice while composing = (%v, %v); want (false, nil)", written, err)
	}

	if _, err := s.WriteInput([]byte("\r")); err != nil {
		t.Fatalf("WriteInput submit: %v", err)
	}
	written, err := s.TryWriteNotice([]byte(notice))
	if err != nil {
		t.Fatalf("TryWriteNotice after submit: %v", err)
	}
	if !written {
		t.Fatal("notice still refused after the human submitted the line")
	}
	waitForBytes(t, sub, notice, 10*time.Second)
}

// TestSession_TryWriteNoticeConcurrentWithInput runs notices and human input
// concurrently. Two properties:
//
//   - No data race on the composition state (this test earns its keep under
//     -race, which is why the suite runs with it).
//   - Every notice that was accepted appears in the child's output WHOLE:
//     one notice is one PTY write, so a concurrent keystroke can never land
//     inside it.
func TestSession_TryWriteNoticeConcurrentWithInput(t *testing.T) {
	const (
		notice     = "NOTICE-ATOMIC-0123456789\n"
		humanLine  = "hhhhhhhhhhhhhhhhhhhhhhhhhhhhhhhh\n"
		iterations = 60
	)

	s, sub := spawnRawCat(t)

	var (
		wg       sync.WaitGroup
		mu       sync.Mutex
		accepted int
	)
	wg.Add(2)
	go func() {
		defer wg.Done()
		for i := 0; i < iterations; i++ {
			if _, err := s.WriteInput([]byte(humanLine)); err != nil {
				return
			}
		}
	}()
	go func() {
		defer wg.Done()
		for i := 0; i < iterations; i++ {
			written, err := s.TryWriteNotice([]byte(notice))
			if err != nil {
				return
			}
			if written {
				mu.Lock()
				accepted++
				mu.Unlock()
			}
			time.Sleep(time.Millisecond)
		}
	}()
	wg.Wait()

	mu.Lock()
	want := accepted
	mu.Unlock()
	if want == 0 {
		t.Skip("no notice was accepted (every attempt raced a composition) — nothing to assert")
	}

	// Drain until every accepted notice has been echoed back whole.
	deadline := time.After(20 * time.Second)
	var seen []byte
	for {
		if strings.Count(string(seen), notice) >= want {
			break
		}
		select {
		case f, ok := <-sub.Frames():
			if !ok {
				t.Fatalf("subscription closed with %d/%d whole notices echoed",
					strings.Count(string(seen), notice), want)
			}
			if f.Type != attachwire.TypeOutput {
				continue
			}
			seen = append(seen, attachwire.DecodeOutput(f.Payload).Data...)
		case <-deadline:
			t.Fatalf("only %d of %d accepted notices came back whole — a notice was spliced",
				strings.Count(string(seen), notice), want)
		}
	}

	// Nothing may carry the notice's leading marker outside a whole notice:
	// a partial occurrence is exactly what byte-interleaving would look like.
	if got, whole := bytes.Count(seen, []byte("NOTICE-")), strings.Count(string(seen), notice); got != whole {
		t.Fatalf("%d notice markers but only %d whole notices — the notice was split", got, whole)
	}
}

// TestSession_TryWriteNoticeAfterExit refuses with an error once the session
// has exited (§12.2: nothing reaches a dead PTY).
func TestSession_TryWriteNoticeAfterExit(t *testing.T) {
	s := mustSpawn(t, Spec{Command: []string{"/bin/sh", "-c", "exit 0"}})
	waitDone(t, s, 10*time.Second)

	written, err := s.TryWriteNotice([]byte("too late\n"))
	if written {
		t.Fatal("TryWriteNotice reported a write on an exited session")
	}
	if err == nil {
		t.Fatal("TryWriteNotice on an exited session must error")
	}
}

// TestSession_TryWriteNoticeEmptyIsNoop keeps the caller contract simple: an
// empty notice is neither written nor an error.
func TestSession_TryWriteNoticeEmptyIsNoop(t *testing.T) {
	s, _ := spawnRawCat(t)
	written, err := s.TryWriteNotice(nil)
	if written || err != nil {
		t.Fatalf("TryWriteNotice(nil) = (%v, %v); want (false, nil)", written, err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Second)
	defer cancel()
	_ = s.Stop(ctx)
}
