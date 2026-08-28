package runner

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/RenseiAI/donmai/agent"
	"github.com/RenseiAI/donmai/runtime/executionevent"
)

func newExecutionEventSinkForWork(qw QueuedWork, primary activitySink, logger *slog.Logger, factory func() (*executionevent.Uploader, error)) (*executionEventSink, error) {
	if !qw.hasCapability(CapabilityExecutionEventIngest) {
		return nil, nil
	}
	uploader, err := factory()
	if err != nil {
		return nil, fmt.Errorf("execution-event uploader: %w", err)
	}
	return newExecutionEventSink(primary, uploader, logger), nil
}

// executionEventSink keeps the durable local append synchronous while
// delivering remotely on one coalescing background flusher. A slow platform
// must never apply network backpressure to the provider event channel.
type executionEventSink struct {
	primary  activitySink
	uploader *executionevent.Uploader
	logger   *slog.Logger
	wake     chan struct{}
	done     chan struct{}
	stop     chan struct{}
	once     sync.Once
}

type executionEventContext struct{ done <-chan struct{} }

func (c executionEventContext) Deadline() (time.Time, bool) { return time.Time{}, false }
func (c executionEventContext) Done() <-chan struct{}       { return c.done }
func (c executionEventContext) Err() error {
	select {
	case <-c.done:
		return context.Canceled
	default:
		return nil
	}
}
func (c executionEventContext) Value(any) any { return nil }

func newExecutionEventSink(primary activitySink, uploader *executionevent.Uploader, logger *slog.Logger) *executionEventSink {
	s := &executionEventSink{
		primary: primary, uploader: uploader, logger: logger,
		wake: make(chan struct{}, 1), done: make(chan struct{}), stop: make(chan struct{}),
	}
	go s.flushLoop(executionEventContext{done: s.stop})
	return s
}

func (s *executionEventSink) flushLoop(ctx context.Context) {
	defer close(s.done)
	for {
		select {
		case <-ctx.Done():
			return
		case <-s.wake:
			if _, err := s.uploader.Flush(ctx); err != nil && s.logger != nil {
				s.logger.Warn("execution-event upload deferred for journal resume", "err", err)
			}
		}
	}
}

func (s *executionEventSink) Send(ctx context.Context, event agent.Event) {
	if s.primary != nil {
		s.primary.Send(ctx, event)
	}
	if s.uploader == nil {
		return
	}
	if _, err := s.uploader.SendEvent(event); err != nil {
		if s.logger != nil {
			s.logger.Warn("execution-event journal append failed", "err", err)
		}
		return
	}
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

func (s *executionEventSink) Close(result *Result) {
	s.once.Do(func() {
		outcome, evidence := executionEventOutcome(result)
		var digest string
		if result != nil {
			digest = executionevent.DigestResult(result.Status, result.Summary, result.Error)
		}
		if err := s.uploader.SendSessionEndedWithEvidence(outcome, evidence, digest); err != nil && s.logger != nil {
			s.logger.Warn("execution-event terminal journal append failed", "err", err)
		}
		close(s.stop)
		<-s.done
		if _, err := s.uploader.Stop(); err != nil && s.logger != nil {
			s.logger.Warn("execution-event terminal drain incomplete; journal retained", "err", err)
		}
	})
}

func executionEventOutcome(result *Result) (string, string) {
	if result == nil {
		return "failed", "inferred"
	}
	switch result.FailureMode {
	case FailureOperatorCancelled:
		return "cancelled", "graceful"
	case FailureAgentBlocked:
		return "interrupted", "inferred"
	case FailureTimeout:
		return "expired", "forced"
	case FailureLostOwnership, FailureSilentExit:
		return "lost", "inferred"
	}
	switch result.Status {
	case "completed":
		return "succeeded", "graceful"
	case "stopped":
		// The platform terminal-evidence vocabulary has no "terminated"
		// value. A generic stopped result is still truthful as an inferred
		// terminated outcome; operator cancellation is handled above.
		return "terminated", "inferred"
	default:
		return "failed", "inferred"
	}
}
