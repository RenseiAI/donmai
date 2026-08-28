package runner

import (
	"context"
	"log/slog"
	"sync"

	"github.com/RenseiAI/donmai/agent"
	"github.com/RenseiAI/donmai/runtime/executionevent"
)

// executionEventSink keeps the durable local append synchronous while
// delivering remotely on one coalescing background flusher. A slow platform
// must never apply network backpressure to the provider event channel.
type executionEventSink struct {
	primary  activitySink
	uploader *executionevent.Uploader
	logger   *slog.Logger
	wake     chan struct{}
	done     chan struct{}
	cancel   context.CancelFunc
	once     sync.Once
}

func newExecutionEventSink(primary activitySink, uploader *executionevent.Uploader, logger *slog.Logger) *executionEventSink {
	ctx, cancel := context.WithCancel(context.Background())
	s := &executionEventSink{
		primary: primary, uploader: uploader, logger: logger,
		wake: make(chan struct{}, 1), done: make(chan struct{}), cancel: cancel,
	}
	go s.flushLoop(ctx)
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
		s.cancel()
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
	switch result.Status {
	case "completed":
		return "succeeded", "graceful"
	case "stopped":
		return "cancelled", "terminated"
	}
	switch result.FailureMode {
	case FailureOperatorCancelled:
		return "cancelled", "graceful"
	case FailureTimeout:
		return "expired", "forced"
	case FailureLostOwnership, FailureSilentExit:
		return "lost", "inferred"
	default:
		return "failed", "inferred"
	}
}
