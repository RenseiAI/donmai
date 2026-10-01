package daemon

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"
)

type localDrainingSource struct {
	entered     chan struct{}
	release     chan struct{}
	active      atomic.Int32
	closed      atomic.Int32
	replacement bool
}

func (*localDrainingSource) Poll(context.Context) ([]LocalIntakeRequest, error) { return nil, nil }
func (s *localDrainingSource) Eligible(ctx context.Context, _ LocalIntakeRequest) (bool, error) {
	if s.replacement {
		return true, nil
	}
	s.active.Add(1)
	defer s.active.Add(-1)
	close(s.entered)
	select {
	case <-s.release:
		return true, nil
	case <-ctx.Done():
		return false, ctx.Err()
	}
}

func (*localDrainingSource) Publish(context.Context, LocalPublicationTask) (LocalPublicationResult, error) {
	return LocalPublicationResult{}, errors.New("unused fixture publication")
}

func (s *localDrainingSource) Close() error {
	if s.active.Load() != 0 {
		return errors.New("source closed before its active call drained")
	}
	s.closed.Add(1)
	return nil
}

func TestLocalSourceReplacementDrainsOldOperationBeforeClose(t *testing.T) {
	t.Parallel()
	old := &localDrainingSource{entered: make(chan struct{}), release: make(chan struct{})}
	next := &localDrainingSource{replacement: true}
	var made atomic.Int32
	f := startLocalHTTPFixture(t, func(d *Daemon) {
		d.opts.LocalRuntime.NewSource = func([]LocalGitHubRepository) (LocalRuntimeSource, error) {
			if made.Add(1) == 1 {
				return old, nil
			}
			return next, nil
		}
	})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	requested := LocalIntakeRequest{RepositoryID: 42, OwnerRepo: "example/project", IssueNumber: 19, IssueURL: "https://github.com/example/project/issues/19", Title: "Held source call", Body: "Implement the authored change."}
	intake := make(chan error, 1)
	go func() { _, err := f.runtime.intake(ctx, requested); intake <- err }()
	select {
	case <-old.entered:
	case <-ctx.Done():
		t.Fatal("old source call did not begin")
	}
	replaced := make(chan error, 1)
	go func() { replaced <- f.runtime.reconfigure(f.d.Config().LocalRuntime.Repositories) }()
	// A bounded forbidden-completion observation: the old call is held by an
	// explicit channel, not a guessed network delay.
	select {
	case err := <-replaced:
		t.Fatalf("replacement finished while old source active: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	if old.closed.Load() != 0 {
		t.Fatal("replacement closed an active source")
	}
	close(old.release)
	select {
	case err := <-intake:
		if err != nil {
			t.Fatal(err)
		}
	case <-ctx.Done():
		t.Fatal("owned source intake did not drain")
	}
	select {
	case err := <-replaced:
		if err != nil {
			t.Fatal(err)
		}
	case <-ctx.Done():
		t.Fatal("source replacement did not finish")
	}
	if old.closed.Load() != 1 || next.closed.Load() != 0 {
		t.Fatal("source replacement did not transfer ownership once")
	}
	source, release, err := f.runtime.sourceLease()
	if err != nil {
		t.Fatal(err)
	}
	if source != next {
		release()
		t.Fatal("replacement is not the current source")
	}
	release()
	if err = f.d.Stop(ctx); err != nil {
		t.Fatal(err)
	}
	if old.closed.Load() != 1 || next.closed.Load() != 1 {
		t.Fatal("daemon stop did not close only the current owned source")
	}
}
