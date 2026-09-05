package stubagent

import (
	"context"
	"os"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// recordingSleeper stands in for the clock so a scenario's timing is decided
// by the test. It returns immediately and records what it was asked to wait,
// which is what the assertions below are actually about: a run that "waited
// 5 minutes" instantly is exactly the observation an integration environment
// wants, and a real 5-minute test would not be run.
type recordingSleeper struct {
	mu   sync.Mutex
	seen []time.Duration
}

func (r *recordingSleeper) sleep(ctx context.Context, d time.Duration) error {
	r.mu.Lock()
	r.seen = append(r.seen, d)
	r.mu.Unlock()
	return ctx.Err()
}

func (r *recordingSleeper) durations() []time.Duration {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]time.Duration(nil), r.seen...)
}

// syncBuffer is an io.Writer safe for the run's two writing goroutines.
type syncBuffer struct {
	mu  sync.Mutex
	buf strings.Builder
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func mustParse(t *testing.T, raw string) Scenario {
	t.Helper()
	scenario, err := Parse([]byte(raw))
	if err != nil {
		t.Fatalf("Parse(%s): %v", raw, err)
	}
	return scenario
}

func TestRunScripts(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		scenario  string
		stdin     string
		wantCode  int
		wantErr   string
		wantOut   []string
		absentOut []string
		wantWaits []time.Duration
	}{
		{
			name:     "prints lines and exits 0",
			scenario: `{"version":1,"steps":[{"print":"one"},{"print":"two"}]}`,
			wantOut:  []string{"one\n", "two\n"},
		},
		{
			name:     "scenario exit code is returned",
			scenario: `{"version":1,"exitCode":1,"steps":[{"print":"failing run"}]}`,
			wantCode: 1,
			wantOut:  []string{"failing run\n"},
		},
		{
			name:      "an explicit exit step skips every later step",
			scenario:  `{"version":1,"exitCode":9,"steps":[{"print":"before"},{"exit":3},{"print":"after"}]}`,
			wantCode:  3,
			wantOut:   []string{"before\n"},
			absentOut: []string{"after"},
		},
		{
			name:      "idle and hangFor are waits, not output",
			scenario:  `{"version":1,"steps":[{"idle":"2s"}],"hangFor":"30s"}`,
			wantWaits: []time.Duration{2 * time.Second, 30 * time.Second},
		},
		{
			name:     "a2a step emits one marked line",
			scenario: `{"version":1,"name":"a2a-run","seed":3,"steps":[{"a2a":{"text":"delivered"}}]}`,
			wantOut:  []string{A2ALinePrefix, `"delivered"`},
		},
		{
			name:     "awaitInput consumes a line",
			scenario: `{"version":1,"steps":[{"awaitInput":{"timeout":"5s"}},{"print":"got it"}]}`,
			stdin:    "a seeded prompt\n",
			wantOut:  []string{"got it\n"},
		},
		{
			name:     "awaitInput echoes when asked",
			scenario: `{"version":1,"steps":[{"awaitInput":{"timeout":"5s","echo":true}}]}`,
			stdin:    "seeded\n",
			wantOut:  []string{EchoPrefix + "seeded\n"},
		},
		{
			name:     "awaitInput on a closed stream fails the scenario",
			scenario: `{"version":1,"steps":[{"awaitInput":{"timeout":"5s"}}]}`,
			stdin:    "",
			wantCode: ExitScenarioFailure,
			wantErr:  "input stream closed",
		},
		{
			name:     "awaitInput that times out fails the scenario rather than continuing",
			scenario: `{"version":1,"steps":[{"awaitInput":{"timeout":"1ms"}},{"print":"never"}]}`,
			// No stdin at all: nil reader, so the wait is the only outcome.
			wantCode:  ExitScenarioFailure,
			wantErr:   "awaitInput",
			absentOut: []string{"never"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var out syncBuffer
			sleeper := &recordingSleeper{}
			opts := Options{Stdout: &out, Sleep: sleeper.sleep}
			if tc.stdin != "" {
				opts.Stdin = strings.NewReader(tc.stdin)
			}

			code, err := Run(context.Background(), mustParse(t, tc.scenario), opts)

			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("Run err = %v, want one containing %q", err, tc.wantErr)
				}
			} else if err != nil {
				t.Fatalf("Run: %v", err)
			}
			if code != tc.wantCode {
				t.Errorf("exit code = %d, want %d", code, tc.wantCode)
			}
			got := out.String()
			for _, want := range tc.wantOut {
				if !strings.Contains(got, want) {
					t.Errorf("output missing %q; got:\n%s", want, got)
				}
			}
			for _, absent := range tc.absentOut {
				if strings.Contains(got, absent) {
					t.Errorf("output contains %q, which the scenario never reaches; got:\n%s", absent, got)
				}
			}
			if tc.wantWaits != nil {
				waits := sleeper.durations()
				if len(waits) != len(tc.wantWaits) {
					t.Fatalf("waits = %v, want %v", waits, tc.wantWaits)
				}
				for i, want := range tc.wantWaits {
					if waits[i] != want {
						t.Errorf("wait %d = %s, want %s", i, waits[i], want)
					}
				}
			}
		})
	}
}

// TestRunIsDeterministic pins the property the harness is FOR. Two runs of the
// same scenario, including its agent-to-agent traffic, are byte-identical.
func TestRunIsDeterministic(t *testing.T) {
	t.Parallel()

	scenario := mustParse(t, `{"version":1,"name":"repeat","seed":11,"steps":[`+
		`{"print":"start"},{"a2a":{"text":"one","contextId":"c"}},{"idle":"1s"},`+
		`{"a2a":{"text":"two","taskId":"t"}},{"print":"end"}]}`)

	run := func() string {
		var out syncBuffer
		sleeper := &recordingSleeper{}
		if _, err := Run(context.Background(), scenario, Options{Stdout: &out, Sleep: sleeper.sleep}); err != nil {
			t.Fatalf("Run: %v", err)
		}
		return out.String()
	}

	first, second := run(), run()
	if first != second {
		t.Fatalf("two runs of one scenario differ:\n--- first ---\n%s\n--- second ---\n%s", first, second)
	}
	if !strings.Contains(first, A2ALinePrefix) {
		t.Fatalf("transcript carries no agent-to-agent line:\n%s", first)
	}
}

func TestRunCooperativeStop(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		scenario   string
		wantCode   int
		wantExit   bool
		wantNotice string
	}{
		{
			// The whole point of the harness: a stop it answers.
			name:       "respond exits with the stop's code",
			scenario:   `{"version":1,"hangFor":"forever","exitCode":0,"stop":{"mode":"respond","exitCode":4}}`,
			wantCode:   4,
			wantExit:   true,
			wantNotice: "mode=respond",
		},
		{
			name:       "slow waits then exits with the stop's code",
			scenario:   `{"version":1,"hangFor":"forever","stop":{"mode":"slow","delay":"3s","exitCode":5}}`,
			wantCode:   5,
			wantExit:   true,
			wantNotice: "mode=slow",
		},
		{
			// The RED control. An ignoring harness must still SAY it saw the
			// signal, or a transcript cannot tell "refused the stop" from
			// "never got the stop" — and the assertion that a cooperative stop
			// worked would pass against a harness that never received one.
			name:       "ignore keeps running and says so",
			scenario:   `{"version":1,"hangFor":"forever","stop":{"mode":"ignore"}}`,
			wantExit:   false,
			wantNotice: "mode=ignore",
		},
		{
			name:       "custom notice",
			scenario:   `{"version":1,"hangFor":"forever","stop":{"mode":"respond","print":"bye now"}}`,
			wantCode:   0,
			wantExit:   true,
			wantNotice: "bye now",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			var out syncBuffer
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()

			sigCh := make(chan os.Signal, 1)
			// The outcome travels on a channel rather than through shared
			// variables the test reads after a t.Fatal: a goroutine writing
			// them after the test has finished is a data race on a dead test.
			type outcome struct {
				code int
				err  error
			}
			done := make(chan outcome, 1)
			go func() {
				code, err := Run(ctx, mustParse(t, tc.scenario), Options{
					Stdout: &out,
					Stop:   sigCh,
					// hangFor forever ignores Sleep, and the stop's own delay
					// must not cost the test three seconds.
					Sleep: func(ctx context.Context, _ time.Duration) error { return ctx.Err() },
				})
				done <- outcome{code: code, err: err}
			}()

			sigCh <- syscall.SIGTERM

			if tc.wantExit {
				select {
				case got := <-done:
					if got.err != nil {
						t.Fatalf("Run: %v", got.err)
					}
					if got.code != tc.wantCode {
						t.Errorf("exit code = %d, want %d", got.code, tc.wantCode)
					}
				case <-time.After(5 * time.Second):
					t.Fatal("Run did not return after a stop it declared it would answer")
				}
			} else {
				select {
				case <-done:
					t.Fatal("Run returned after a stop the scenario declared it would ignore")
				case <-time.After(300 * time.Millisecond):
				}
			}

			if notice := out.String(); !strings.Contains(notice, tc.wantNotice) {
				t.Errorf("transcript missing %q; got:\n%s", tc.wantNotice, notice)
			}

			if !tc.wantExit {
				cancel()
				select {
				case <-done:
				case <-time.After(5 * time.Second):
					t.Fatal("Run did not return after its context ended")
				}
			}
		})
	}
}

// TestRunReturnsWithAStopChannelThatNeverFires is a regression pin, and the
// reason it exists is worth keeping: every table case above passes a nil Stop,
// so none of them start the stop watcher, and the watcher parks on the run's
// context. With the cancel and the wait as two separate defers they unwind in
// the wrong order and a scenario that ends on its own never returns — a
// deadlock invisible to every scripted case here and immediate in a real
// session, which is exactly what happened.
func TestRunReturnsWithAStopChannelThatNeverFires(t *testing.T) {
	t.Parallel()

	var out syncBuffer
	signals := make(chan os.Signal, 1) // never written to

	// The outcome is passed out rather than asserted in the goroutine: if the
	// deadline below fires, the test has already finished, and a t.Errorf from
	// a goroutine after that is a use-after-finish, not a report.
	type outcome struct {
		code int
		err  error
	}
	done := make(chan outcome, 1)
	go func() {
		code, err := Run(context.Background(), mustParse(t, `{"version":1,"steps":[{"print":"and done"}]}`),
			Options{Stdout: &out, Stop: signals})
		done <- outcome{code: code, err: err}
	}()

	select {
	case got := <-done:
		if got.err != nil {
			t.Fatalf("Run: %v", got.err)
		}
		if got.code != 0 {
			t.Errorf("exit code = %d, want 0", got.code)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return after its script finished while a stop channel was wired")
	}
	if got := out.String(); got != "and done\n" {
		t.Errorf("output = %q, want %q", got, "and done\n")
	}
}

func TestRunOutputRatePacesWithoutChangingTheBytes(t *testing.T) {
	t.Parallel()

	var out syncBuffer
	sleeper := &recordingSleeper{}
	scenario := mustParse(t, `{"version":1,"outputRate":20,"steps":[{"print":"0123456789"}]}`)

	if _, err := Run(context.Background(), scenario, Options{Stdout: &out, Sleep: sleeper.sleep}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got, want := out.String(), "0123456789\n"; got != want {
		t.Errorf("output = %q, want %q — throttling must not change the bytes", got, want)
	}
	waits := sleeper.durations()
	if len(waits) != 1 {
		t.Fatalf("throttled write produced %d waits (%v), want exactly 1 — the debt is paid once, after the line is out", len(waits), waits)
	}
	// 11 bytes at 20 bytes/second is 550ms.
	if want := 550 * time.Millisecond; waits[0] != want {
		t.Errorf("pacing = %s, want %s", waits[0], want)
	}
}

// TestRunOutputRateDoesNotStarveTheStopNotice is the regression for the
// deadlock-shaped bug that pacing inside the write lock would create.
//
// A 100-byte line at 20 bytes/second is five seconds of pacing — the whole
// cooperative-stop grace window. If the throttle held the writer's lock while
// it waited, the stop notice would be written only AFTER the parent had given
// up and escalated, and the transcript would be indistinguishable from a child
// that never received the signal. That silently disarms the ignore mode, which
// exists precisely to make that distinction.
//
// The sleeper here is real, not instant: a fake clock cannot exhibit lock
// starvation, so the assertion is that the notice lands promptly in wall-clock
// terms while a long throttled line is still being paced.
func TestRunOutputRateDoesNotStarveTheStopNotice(t *testing.T) {
	t.Parallel()

	var out syncBuffer
	signals := make(chan os.Signal, 1)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// 100 characters at 20 bytes/second: ~5s of pacing debt after the line.
	long := strings.Repeat("x", 100)
	scenario := mustParse(t, `{"version":1,"outputRate":20,"hangFor":"forever",`+
		`"steps":[{"print":"`+long+`"}],"stop":{"mode":"ignore"}}`)

	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = Run(ctx, scenario, Options{Stdout: &out, Stop: signals})
	}()

	// Wait for the line itself to be out, so the run is inside its pacing debt
	// when the signal arrives — the exact window the bug lived in.
	deadline := time.Now().Add(5 * time.Second)
	for !strings.Contains(out.String(), long) {
		if time.Now().After(deadline) {
			t.Fatalf("throttled line never reached the buffer; got:\n%s", out.String())
		}
		time.Sleep(10 * time.Millisecond)
	}
	signals <- syscall.SIGTERM

	noticeBy := time.Now().Add(2 * time.Second)
	for !strings.Contains(out.String(), "mode=ignore") {
		if time.Now().After(noticeBy) {
			t.Fatalf("stop notice did not appear within 2s while a %d-byte line was pacing at 20 B/s; "+
				"the throttle is holding the write lock across its wait", len(long))
		}
		time.Sleep(10 * time.Millisecond)
	}

	cancel()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("Run did not return after its context ended")
	}
}

func TestRunRejectsBadInput(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		scenario Scenario
		opts     Options
		wantSub  string
	}{
		{
			name:     "no stdout",
			scenario: DefaultScenario(),
			opts:     Options{},
			wantSub:  "Stdout is required",
		},
		{
			name:     "invalid scenario",
			scenario: Scenario{Version: 99},
			opts:     Options{Stdout: &syncBuffer{}},
			wantSub:  "not the supported version",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			code, err := Run(context.Background(), tc.scenario, tc.opts)
			if err == nil || !strings.Contains(err.Error(), tc.wantSub) {
				t.Fatalf("Run err = %v, want one containing %q", err, tc.wantSub)
			}
			if code != ExitScenarioFailure {
				t.Errorf("exit code = %d, want ExitScenarioFailure (%d)", code, ExitScenarioFailure)
			}
		})
	}
}
