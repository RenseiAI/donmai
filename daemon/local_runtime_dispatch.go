package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"strconv"
	"time"

	"github.com/RenseiAI/donmai/executioncell"
	"github.com/RenseiAI/donmai/internal/localqueue"
	"github.com/RenseiAI/donmai/internal/localruntimeauth"
	"github.com/RenseiAI/donmai/sessionshim"
)

func (l *localRuntime) publishEndpoint(ctx context.Context) error {
	bound := l.daemon.controlURL.Load()
	if bound == nil || *bound == "" {
		return errors.New("local runtime requires an actually bound loopback listener")
	}
	prior, err := l.auth.Endpoint()
	if err == nil && prior.Origin != *bound {
		return errors.New("local receiver origin changed; preserve pending authority for explicit reconciliation")
	}
	if err != nil && !errors.Is(err, localruntimeauth.ErrMissingEndpoint) {
		return err
	}
	if _, err = l.auth.PublishEndpoint(ctx, *bound, l.instanceID); err != nil {
		return err
	}
	l.mu.Lock()
	l.endpoint = *bound
	l.mu.Unlock()
	return nil
}

func (l *localRuntime) start(ctx context.Context) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.closed || l.cancel != nil || l.endpoint == "" || ctx.Err() != nil {
		return
	}
	runCtx, cancel := context.WithCancel(ctx)
	l.cancel = cancel
	l.runContext = runCtx
	l.done = make(chan struct{})
	go l.run(runCtx, l.done)
}

func (l *localRuntime) run(ctx context.Context, done chan struct{}) {
	defer close(done)
	ticker := time.NewTicker(l.options.PollInterval)
	defer ticker.Stop()
	for {
		if ctx.Err() != nil {
			return
		}
		if err := l.cycle(ctx); err != nil && ctx.Err() == nil {
			slog.Warn("local runtime cycle held", "error", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (l *localRuntime) beginStop() <-chan struct{} {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.cancel != nil {
		l.cancel()
	}
	return l.done
}

func (l *localRuntime) close(ctx context.Context) error {
	l.mu.Lock()
	if l.closeDone == nil {
		l.closed = true
		if l.cancel != nil {
			l.cancel()
		}
		l.closeDone = make(chan struct{})
		done := l.done
		go func() {
			if done != nil {
				<-done
			}
			l.sourceMu.Lock()
			var sourceErr error
			if l.options.NewSource != nil {
				sourceErr = closeLocalSource(l.source)
			}
			l.sourceMu.Unlock()
			l.ops.Lock()
			err := errors.Join(sourceErr, l.store.Close(), l.auth.Close())
			l.ops.Unlock()
			l.mu.Lock()
			l.closeErr = err
			close(l.closeDone)
			l.mu.Unlock()
		}()
	}
	done := l.closeDone
	l.mu.Unlock()
	if err := waitCompletionContext(ctx, done); err != nil {
		return err
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.closeErr
}

func (l *localRuntime) cycle(ctx context.Context) error {
	source, release, err := l.sourceLease()
	if err != nil {
		return err
	}
	defer release()
	if l.daemon.State() == StateRunning {
		issues, err := source.Poll(ctx)
		if err != nil {
			return err
		}
		if len(issues) > 1000 {
			return errors.New("local source exceeded bounded intake batch")
		}
		for _, issue := range issues {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			eligible, err := source.Eligible(ctx, issue)
			if err != nil {
				return err
			}
			if !eligible {
				continue
			}
			if _, err = l.admitIntake(ctx, issue); err != nil {
				return err
			}
		}
		pending, err := l.store.PendingDispatch(ctx)
		if err != nil {
			return err
		}
		for _, record := range pending {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if l.daemon.State() != StateRunning || l.daemon.ActiveSessionCount() >= l.daemon.MaxConcurrentSessions() {
				break
			}
			if err = l.dispatch(ctx, record, source); err != nil {
				slog.Warn("local dispatch held", "session_id", record.Envelope.Session.SessionID, "error", err)
			}
		}
	}
	if err := l.sourcePolicyCurrent(); err != nil {
		return err
	}
	return l.publishPending(ctx, source)
}

func (l *localRuntime) dispatch(ctx context.Context, record localqueue.AdmissionRecord, source LocalRuntimeSource) error {
	if err := l.validateCoupledWorker(); err != nil {
		return err
	}
	l.policyMu.RLock()
	defer l.policyMu.RUnlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := l.checkCurrentConfiguration(); err != nil {
		return err
	}
	if err := l.validateCurrentAdmissionPolicy(record.Envelope.OperationalPayload, record.Envelope.HostPreflight); err != nil {
		return err
	}
	request, err := l.sourceRequest(record.Envelope)
	if err != nil {
		return err
	}
	if err = l.validateIntake(request); err != nil {
		return err
	}
	eligible, err := source.Eligible(ctx, request)
	if err != nil || !eligible {
		return err
	}
	if err = l.sourcePolicyCurrent(); err != nil {
		return err
	}
	l.ops.Lock()
	if l.daemon.State() != StateRunning {
		l.ops.Unlock()
		return errors.New("local daemon is not ready to claim")
	}
	attempt, err := l.store.Claim(ctx, l.store.CurrentRevision(), record.Envelope.Session)
	l.ops.Unlock()
	if err != nil {
		return err
	}
	hold := func(cause error) error {
		projection, loadErr := l.store.Session(context.Background(), l.identity.ScopeID, attempt.SessionID)
		if loadErr == nil && projection.Terminal == nil {
			l.hold(projection, cause.Error())
		}
		return cause
	}
	compiler, err := l.options.NewCompiler(l.identity)
	if err == nil && compiler == nil {
		err = errors.New("local compiler factory returned no compiler")
	}
	if err != nil {
		return l.denyBeforeLaunch(ctx, attempt, err)
	}
	defer func() { _ = closeLocalCompiler(compiler) }()
	if err = compiler.Revalidate(ctx, localEvidence(record.Envelope)); err != nil {
		return l.denyBeforeLaunch(ctx, attempt, err)
	}
	// Eligibility may have changed while current binary/auth/config observations
	// were collected. A second refusal holds this exact claim; it never respawns.
	eligible, err = source.Eligible(ctx, request)
	if err != nil {
		return hold(err)
	}
	if !eligible {
		return hold(errors.New("local source eligibility changed before launch"))
	}
	if err = l.sourcePolicyCurrent(); err != nil {
		return hold(err)
	}
	if err = l.checkCurrentConfiguration(); err != nil {
		return hold(err)
	}
	if err = l.validateCurrentAdmissionPolicy(record.Envelope.OperationalPayload, record.Envelope.HostPreflight); err != nil {
		return hold(err)
	}
	if err = l.validateCoupledWorker(); err != nil {
		return hold(err)
	}
	if _, err = l.auth.CreateAttempt(ctx, record.Envelope.CredentialHandle, attempt); err != nil {
		return hold(err)
	}
	l.ops.Lock()
	if l.daemon.State() != StateRunning {
		l.ops.Unlock()
		return hold(errors.New("local daemon admission paused before launch"))
	}
	err = l.store.BeginLaunch(ctx, l.store.CurrentRevision(), attempt)
	l.ops.Unlock()
	if err != nil {
		return hold(err)
	}
	projection, err := l.store.Session(ctx, l.identity.ScopeID, attempt.SessionID)
	if err != nil {
		return hold(err)
	}
	detail, err := l.detail(projection)
	if err != nil {
		return hold(err)
	}
	var item PollWorkItem
	if err = json.Unmarshal(record.Envelope.OperationalPayload, &item); err != nil {
		return hold(err)
	}
	item.SessionID = attempt.SessionID
	spec := PollItemToSessionSpec(item, l.daemon.Config().EffectiveProjectConfigs())
	spec.OrganizationID = l.identity.ScopeID
	permit, err := l.registerLaunchPermit(attempt)
	if err != nil {
		return hold(err)
	}
	defer l.retireLaunchPermit(permit)
	handle, err := l.daemon.acceptWorkWithDetail(spec, detail, permit)
	if err != nil {
		return hold(err)
	}
	identity, err := sessionshim.ProcessIdentityFor(handle.PID)
	if err != nil {
		return hold(err)
	}
	actual, ok := l.daemon.SessionDetail(attempt.SessionID)
	if !ok {
		return hold(errors.New("started local session detail is unavailable"))
	}
	if actual.AuthToken != "" || actual.McpAuthToken != "" {
		return hold(errors.New("local session detail contains remote credentials"))
	}
	detailDigest, err := executioncell.DigestContractValue(actual)
	if err != nil {
		return hold(err)
	}
	l.ops.Lock()
	defer l.ops.Unlock()
	latest, err := l.store.Session(ctx, l.identity.ScopeID, attempt.SessionID)
	if err != nil {
		return hold(err)
	}
	if latest.Terminal != nil {
		return nil
	}
	err = l.store.ObserveLaunch(ctx, l.store.CurrentRevision(), attempt, localqueue.OwnedProcessCorrelation{ProcessID: handle.PID, ProcessStart: strconv.FormatInt(identity.StartedAt, 10), SessionDetailSHA256: detailDigest})
	if err != nil {
		return hold(err)
	}
	return nil
}

func (l *localRuntime) denyBeforeLaunch(ctx context.Context, attempt localqueue.AttemptRef, cause error) error {
	l.ops.Lock()
	defer l.ops.Unlock()
	if err := l.store.RecordObservation(ctx, attempt, localqueue.BoundedTypedObservation{Kind: localqueue.ObservationPrestartDeny, ObservedAt: time.Now().UTC().Format(time.RFC3339Nano), Evidence: localDigest([]byte(cause.Error()))}); err != nil {
		return errors.Join(cause, err)
	}
	body, err := json.Marshal(map[string]string{"workerId": l.identity.WorkerID, "status": "failed", "failureMode": "local-prestart-denied", "summary": "Local runtime admission was denied before launch."})
	if err != nil {
		return err
	}
	if _, err = l.store.CommitTerminal(ctx, attempt, body); err != nil {
		return errors.Join(cause, err)
	}
	return cause
}

func (l *localRuntime) publishPending(ctx context.Context, source LocalRuntimeSource) error {
	pending, err := l.store.PendingPublication(ctx)
	if err != nil {
		return err
	}
	for _, outbox := range pending {
		projection, err := l.store.Session(ctx, l.identity.ScopeID, outbox.SessionID)
		if err != nil {
			return err
		}
		if projection.Terminal == nil {
			return errors.New("local publication lacks durable terminal authority")
		}
		correlation := projection.Admission.Envelope.Source
		task := LocalPublicationTask{Key: outbox.Key, Kind: outbox.Operation, SessionID: outbox.SessionID, ResultSHA256: outbox.ResultSHA256, Status: projection.Terminal.Status, PullRequestURL: outbox.URL, CommitSHA: outbox.HeadSHA, Source: LocalIntakeRequest{RepositoryID: correlation.RepositoryID, OwnerRepo: correlation.OwnerRepo, IssueNumber: correlation.IssueNumber, IssueURL: correlation.IssueURL}}
		var payload struct {
			Branch  string `json:"branch"`
			BaseRef string `json:"baseRef"`
		}
		if err = json.Unmarshal(projection.Admission.Envelope.OperationalPayload, &payload); err != nil {
			return err
		}
		task.Branch = payload.Branch
		task.BaseRef = payload.BaseRef
		result, err := source.Publish(ctx, task)
		if err != nil {
			return err
		}
		l.ops.Lock()
		err = l.store.CompletePublication(ctx, outbox.Key, localqueue.PublicationEvidence{URL: result.URL, HeadSHA: result.HeadSHA, ReadAt: result.ReadAt})
		l.ops.Unlock()
		if err != nil {
			return err
		}
	}
	return nil
}

func (l *localRuntime) allowAccept(spec SessionSpec, detail *SessionDetail, permit *localLaunchPermit) error {
	if permit == nil {
		return errors.New("local launch requires its private dispatcher permit")
	}
	if err := l.checkCurrentConfiguration(); err != nil {
		return err
	}
	if detail == nil || spec.Mode != "" || spec.OrganizationID != l.identity.ScopeID {
		return errors.New("local sessions require issuer-owned headless detail")
	}
	projection, err := l.store.Session(context.Background(), l.identity.ScopeID, spec.SessionID)
	if err != nil || projection.Attempt == nil || projection.Terminal != nil || projection.Admission.DispatchState != localqueue.DispatchUnknown {
		return errors.New("local admission has no durable launch intent")
	}
	if !l.matchesLaunchPermit(permit, *projection.Attempt) {
		return errors.New("local launch permit does not match the current attempt")
	}
	envelope := projection.Admission.Envelope
	if err := l.validateCurrentAdmissionPolicy(envelope.OperationalPayload, envelope.HostPreflight); err != nil {
		return err
	}
	if !bytes.Equal(detail.AdmissionReceipt, envelope.Receipt) || !bytes.Equal(detail.OperationalPayload, envelope.OperationalPayload) || !bytes.Equal(detail.EffectiveCell, envelope.EffectiveCell) || !bytes.Equal(detail.ExecutionRuntimeBinding, envelope.RuntimeBinding) || detail.AuthToken != "" || detail.McpAuthToken != "" {
		return errors.New("local session detail disagrees with immutable admission")
	}
	return nil
}

func (l *localRuntime) ended(sessionID string) {
	projection, err := l.store.Session(context.Background(), l.identity.ScopeID, sessionID)
	if err != nil {
		slog.Warn("local ended-session state unavailable", "session_id", sessionID, "error", err)
		return
	}
	if projection.Terminal != nil {
		l.releaseTerminal(sessionID)
		return
	}
	l.hold(projection, "owned worker exited without a durable terminal result")
}

func (l *localRuntime) reconfigure(repositories []LocalGitHubRepository) error {
	if err := validateLocalRepositories(repositories); err != nil {
		return err
	}
	if l.options.NewSource == nil {
		return errors.New("local source composition cannot refresh configured repositories")
	}
	source, err := l.options.NewSource(append([]LocalGitHubRepository(nil), repositories...))
	if err == nil && source == nil {
		err = errors.New("local source factory returned no adapter")
	}
	if err != nil {
		return err
	}
	l.sourceMu.Lock()
	defer l.sourceMu.Unlock()
	l.mu.Lock()
	if l.closed {
		l.mu.Unlock()
		_ = closeLocalSource(source)
		return errors.New("local runtime is closing")
	}
	previous := l.source
	l.repositories = append([]LocalGitHubRepository(nil), repositories...)
	l.source = source
	l.mu.Unlock()
	return closeLocalSource(previous)
}
