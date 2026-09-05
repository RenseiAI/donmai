package stubagent

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"time"
)

// Defaults the scenario may leave unset.
const (
	// DefaultAwaitTimeout bounds an AwaitInput step that names no timeout.
	DefaultAwaitTimeout = 30 * time.Second
	// DefaultStopNotice is printed when a cooperative stop is observed.
	DefaultStopNotice = "stub agent: stop observed"
	// EchoPrefix precedes a line echoed by an AwaitInput step with Echo set.
	EchoPrefix = "stub agent: input "
	// ExitScenarioFailure is the status returned when the scenario itself
	// could not be carried out — an input that never arrived, an output that
	// could not be written. It is deliberately NOT 1: a scenario asking for
	// exit 1 is a passing run of a failure scenario, and the two must be
	// distinguishable by the thing that reads the exit status.
	ExitScenarioFailure = 70
)

// Options are the injectable edges of a run. Every field has a production
// default; tests supply their own so a scenario's timing is decided by the
// test rather than by the clock.
type Options struct {
	// Stdout receives every scripted line. Required.
	Stdout io.Writer
	// Stdin feeds AwaitInput steps. Nil behaves as an immediately-empty
	// stream, so an AwaitInput step against a nil Stdin fails its wait
	// rather than blocking forever.
	Stdin io.Reader
	// Stop delivers cooperative-stop signals. Nil means the run can only be
	// ended by ctx or by the script itself.
	Stop <-chan os.Signal
	// Sleep is how the run waits. Nil uses a real timer that also honours
	// ctx. It must return ctx.Err() when ctx ends first.
	Sleep func(ctx context.Context, d time.Duration) error
}

// Run executes the scenario and returns the process exit status.
//
// The returned error describes a scenario that could not be carried out. A
// scenario that deliberately fails — exit 1, a stop it refuses to answer —
// returns that outcome with a nil error: the harness did exactly what it was
// asked, and the caller's assertion is about the exit status, not about
// whether this function complained.
func Run(ctx context.Context, scenario Scenario, opts Options) (int, error) {
	if err := scenario.Validate(); err != nil {
		return ExitScenarioFailure, err
	}
	if opts.Stdout == nil {
		return ExitScenarioFailure, errors.New("stubagent: Options.Stdout is required")
	}
	sleep := opts.Sleep
	if sleep == nil {
		sleep = realSleep
	}

	out := &syncWriter{w: opts.Stdout, rate: scenario.OutputRate, sleep: sleep}

	runCtx, cancel := context.WithCancel(ctx)

	stopped := newStopLatch()
	var watcher sync.WaitGroup
	if opts.Stop != nil {
		watcher.Add(1)
		go func() {
			defer watcher.Done()
			watchStop(runCtx, scenario, opts.Stop, out, sleep, stopped, cancel)
		}()
	}
	// One deferred closure, not two: the watcher parks on runCtx, so cancel
	// MUST run before the wait. Separate `defer cancel()` / `defer
	// watcher.Wait()` statements unwind last-in-first-out and deadlock a run
	// that ends on its own — the exact shape a clean scenario takes.
	defer func() {
		cancel()
		watcher.Wait()
	}()

	lines := startLineReader(runCtx, opts.Stdin)

	code, err := runSteps(runCtx, scenario, out, lines, sleep)
	if err != nil || code != nil {
		if final, ok := stopped.exitCode(); ok {
			return final, nil
		}
		if err != nil {
			return ExitScenarioFailure, err
		}
		return *code, nil
	}

	if err := hangOut(runCtx, scenario, sleep); err != nil {
		if final, ok := stopped.exitCode(); ok {
			return final, nil
		}
		return ExitScenarioFailure, err
	}
	if final, ok := stopped.exitCode(); ok {
		return final, nil
	}
	return scenario.ExitCode, nil
}

// runSteps walks the script. A non-nil returned code is an explicit `exit`
// step; nil means the script ran to its end.
func runSteps(ctx context.Context, scenario Scenario, out *syncWriter, lines <-chan string, sleep sleepFunc) (*int, error) {
	for i, step := range scenario.Steps {
		switch {
		case step.Print != nil:
			if err := out.line(ctx, *step.Print); err != nil {
				return nil, fmt.Errorf("stubagent: step %d: print: %w", i, err)
			}
		case step.Idle != nil:
			if err := sleep(ctx, step.Idle.Duration()); err != nil {
				return nil, fmt.Errorf("stubagent: step %d: idle: %w", i, err)
			}
		case step.A2A != nil:
			line, err := EncodeA2ALine(scenario, i, *step.A2A)
			if err != nil {
				return nil, fmt.Errorf("stubagent: step %d: %w", i, err)
			}
			if err := out.line(ctx, line); err != nil {
				return nil, fmt.Errorf("stubagent: step %d: a2a: %w", i, err)
			}
		case step.AwaitInput != nil:
			if err := awaitInput(ctx, *step.AwaitInput, out, lines); err != nil {
				return nil, fmt.Errorf("stubagent: step %d: %w", i, err)
			}
		case step.Exit != nil:
			code := *step.Exit
			return &code, nil
		case step.Hang != nil && *step.Hang:
			<-ctx.Done()
			return nil, fmt.Errorf("stubagent: step %d: hang: %w", i, ctx.Err())
		}
	}
	return nil, nil
}

func awaitInput(ctx context.Context, await AwaitInput, out *syncWriter, lines <-chan string) error {
	timeout := await.Timeout.Duration()
	if timeout == 0 {
		timeout = DefaultAwaitTimeout
	}
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case line, ok := <-lines:
		if !ok {
			return errors.New("awaitInput: input stream closed before a line arrived")
		}
		if !await.Echo {
			return nil
		}
		return out.line(ctx, EchoPrefix+line)
	case <-timer.C:
		return fmt.Errorf("awaitInput: no line within %s", timeout)
	case <-ctx.Done():
		return fmt.Errorf("awaitInput: %w", ctx.Err())
	}
}

func hangOut(ctx context.Context, scenario Scenario, sleep sleepFunc) error {
	duration, forever, err := scenario.HangDuration()
	if err != nil {
		return err
	}
	switch {
	case forever:
		<-ctx.Done()
		return fmt.Errorf("stubagent: hangFor %s: %w", HangForever, ctx.Err())
	case duration > 0:
		if err := sleep(ctx, duration); err != nil {
			return fmt.Errorf("stubagent: hangFor: %w", err)
		}
	}
	return nil
}

// watchStop turns the first cooperative-stop signal into the outcome the
// scenario declared. Every mode PRINTS, including StopIgnore, and that is the
// load-bearing part: a still-running child looks identical whether it refused
// the stop or never received one, and the notice is the only evidence that
// separates them. Without it, an assertion that a stop was DELIVERED would
// pass against a stop path that sent nothing at all.
func watchStop(
	ctx context.Context,
	scenario Scenario,
	signals <-chan os.Signal,
	out *syncWriter,
	sleep sleepFunc,
	latch *stopLatch,
	cancel context.CancelFunc,
) {
	select {
	case <-ctx.Done():
		return
	case _, ok := <-signals:
		if !ok {
			return
		}
	}
	notice := scenario.Stop.Print
	if notice == "" {
		notice = DefaultStopNotice
	}
	// The notice is best-effort: a stop often arrives precisely because the
	// far end is going away, and failing the run because the goodbye could
	// not be delivered would misreport a stop that did work.
	_ = out.line(ctx, notice+" mode="+string(scenario.StopModeOrDefault()))

	switch scenario.StopModeOrDefault() {
	case StopIgnore:
		return
	case StopSlow:
		if err := sleep(ctx, scenario.Stop.Delay.Duration()); err != nil {
			return
		}
	case StopRespond:
	}
	latch.set(scenario.Stop.ExitCode)
	cancel()
}

// stopLatch carries the stop-decided exit status across goroutines.
type stopLatch struct {
	mu      sync.Mutex
	decided bool
	value   int
}

func newStopLatch() *stopLatch { return &stopLatch{} }

func (l *stopLatch) set(code int) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if !l.decided {
		l.decided = true
		l.value = code
	}
}

func (l *stopLatch) exitCode() (int, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.value, l.decided
}

type sleepFunc func(ctx context.Context, d time.Duration) error

func realSleep(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-timer.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// startLineReader drains r in the background so an AwaitInput step reads from
// a channel rather than blocking on a file descriptor it cannot abandon. A
// nil reader yields an immediately-closed channel.
func startLineReader(ctx context.Context, r io.Reader) <-chan string {
	out := make(chan string, 16)
	if r == nil {
		close(out)
		return out
	}
	go func() {
		defer close(out)
		scanner := bufio.NewScanner(r)
		scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
		for scanner.Scan() {
			line := strings.TrimRight(scanner.Text(), "\r")
			select {
			case out <- line:
			case <-ctx.Done():
				return
			}
		}
	}()
	return out
}

// syncWriter serializes the script's writes against the stop watcher's, and
// applies the scenario's output-rate throttle. Without the mutex two
// goroutines could interleave inside one line and the transcript would stop
// being line-addressable, which is the one property every consumer relies on.
type syncWriter struct {
	mu    sync.Mutex
	w     io.Writer
	rate  int
	sleep sleepFunc
}

func (s *syncWriter) line(ctx context.Context, text string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.write(ctx, []byte(text+"\n"))
}

func (s *syncWriter) write(ctx context.Context, data []byte) error {
	if s.rate <= 0 {
		if _, err := s.w.Write(data); err != nil {
			return fmt.Errorf("stubagent: write: %w", err)
		}
		return nil
	}
	// Chunk small enough that the pacing is visible to a reader watching the
	// stream, large enough that a long line does not become a syscall per byte.
	chunk := s.rate / 20
	if chunk < 1 {
		chunk = 1
	}
	for len(data) > 0 {
		n := min(chunk, len(data))
		if _, err := s.w.Write(data[:n]); err != nil {
			return fmt.Errorf("stubagent: write: %w", err)
		}
		data = data[n:]
		delay := time.Duration(float64(n) / float64(s.rate) * float64(time.Second))
		if err := s.sleep(ctx, delay); err != nil {
			return fmt.Errorf("stubagent: throttled write: %w", err)
		}
	}
	return nil
}
