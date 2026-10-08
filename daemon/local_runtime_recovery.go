package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"

	"github.com/RenseiAI/donmai/executioncell"
	"github.com/RenseiAI/donmai/internal/localqueue"
	"github.com/RenseiAI/donmai/sessionshim"
)

// SessionUnknown is a local observation of unresolved execution ownership.
// It is not a new execution-cell phase or evidence that a process is running.
const SessionUnknown SessionState = "unknown"

func (l *localRuntime) detail(projection localqueue.SessionProjection) (*SessionDetail, error) {
	envelope := projection.Admission.Envelope
	var item PollWorkItem
	if err := json.Unmarshal(envelope.OperationalPayload, &item); err != nil {
		return nil, fmt.Errorf("decode local operational payload: %w", err)
	}
	if item.OrganizationID != l.identity.ScopeID {
		return nil, errors.New("local operational scope disagrees with protected authority")
	}
	item.SessionID = envelope.Session.SessionID
	item.AdmissionReceipt = bytes.Clone(envelope.Receipt)
	item.EffectiveCell = bytes.Clone(envelope.EffectiveCell)
	item.ExecutionRuntimeBinding = bytes.Clone(envelope.RuntimeBinding)
	item.OperationalPayload = bytes.Clone(envelope.OperationalPayload)
	l.mu.Lock()
	endpoint := l.endpoint
	l.mu.Unlock()
	projects := l.daemon.Config().EffectiveProjectConfigs()
	detail := PollItemToSessionDetail(item, projects, endpoint+localRuntimePrefix, "", l.identity.WorkerID)
	detail.HostAdaptationReceipt = bytes.Clone(envelope.HostPreflight)
	if replay, ok := l.daemon.opts.ExecutionPreflightStore.(ExecutionPreflightReplayStore); ok {
		raw, err := replay.Load(item.SessionID)
		if err == nil {
			receipt, decodeErr := executioncell.DecodeHostAdaptationReceipt(raw)
			if decodeErr != nil || receipt.RequestID != item.SessionID || receipt.WorkerID != l.identity.WorkerID {
				return nil, errors.New("stored local host preflight disagrees with session authority")
			}
			detail.HostAdaptationReceipt = bytes.Clone(raw)
		} else if projection.Launch != nil {
			return nil, fmt.Errorf("recover exact consumed host preflight: %w", err)
		}
	}
	if detail.AuthToken != "" || detail.McpAuthToken != "" {
		return nil, errors.New("local detail cannot carry controller credentials")
	}
	return detail, nil
}

func (l *localRuntime) recover(ctx context.Context) error {
	sessions, err := l.store.SessionsNeedingRecovery(ctx)
	if err != nil {
		return err
	}
	for _, projection := range sessions {
		l.hold(projection, "execution ownership requires reconciliation")
	}
	return nil
}

func (l *localRuntime) hold(projection localqueue.SessionProjection, reason string) {
	envelope := projection.Admission.Envelope
	handle := SessionHandle{SessionID: envelope.Session.SessionID, State: SessionUnknown, AcceptedAt: envelope.InitialEvent.RecordedAt, ProjectName: envelope.Source.OwnerRepo, Repository: "https://github.com/" + envelope.Source.OwnerRepo + ".git", SeatBudget: l.daemon.seatBudgetHandleReport()}
	if projection.Launch != nil {
		identity, err := sessionshim.ProcessIdentityFor(projection.Launch.ProcessID)
		if err == nil && strconv.FormatInt(identity.StartedAt, 10) == projection.Launch.ProcessStart {
			handle.PID = projection.Launch.ProcessID
			handle.State = SessionRunning
		}
	}
	l.mu.Lock()
	l.held[handle.SessionID] = handle
	l.holdReasons[handle.SessionID] = reason
	l.mu.Unlock()
}

func (l *localRuntime) heldSessions() []SessionHandle {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := make([]SessionHandle, 0, len(l.held))
	for _, handle := range l.held {
		out = append(out, handle)
	}
	slices.SortFunc(out, func(a, b SessionHandle) int { return strings.Compare(a.SessionID, b.SessionID) })
	return out
}

func (l *localRuntime) releaseTerminal(sessionID string) {
	l.mu.Lock()
	delete(l.held, sessionID)
	delete(l.holdReasons, sessionID)
	l.mu.Unlock()
}

func (l *localRuntime) authenticate(ctx context.Context, sessionID, bearer string) (localqueue.SessionProjection, error) {
	projection, err := l.store.Session(ctx, l.identity.ScopeID, sessionID)
	if err != nil || projection.Attempt == nil {
		return localqueue.SessionProjection{}, errors.New("local attempt authentication refused")
	}
	// The allowed reference comes exclusively from the authoritative projection.
	// Terminal projections retain this exact key for byte-identical result replay.
	if err = l.auth.VerifyAttempt(projection.Admission.Envelope.CredentialHandle, *projection.Attempt, bearer); err != nil {
		return localqueue.SessionProjection{}, errors.New("local attempt authentication refused")
	}
	return projection, nil
}

func (l *localRuntime) validateSecretRelease(spec SessionSpec) (localqueue.SessionProjection, error) {
	if err := l.validateCoupledWorker(); err != nil {
		return localqueue.SessionProjection{}, err
	}
	if err := l.checkCurrentConfiguration(); err != nil {
		return localqueue.SessionProjection{}, err
	}
	projection, err := l.store.Session(context.Background(), l.identity.ScopeID, spec.SessionID)
	if err != nil || projection.Attempt == nil || projection.Terminal != nil || projection.Admission.DispatchState != localqueue.DispatchUnknown {
		return localqueue.SessionProjection{}, errors.New("local spawn lacks exact durable launch intent")
	}
	if l.currentLaunchPermit(*projection.Attempt) == nil {
		return localqueue.SessionProjection{}, errors.New("local credential release has no active dispatcher permit")
	}
	replay, ok := l.daemon.opts.ExecutionPreflightStore.(ExecutionPreflightReplayStore)
	if !ok {
		return localqueue.SessionProjection{}, errors.New("local secret release requires stored host preflight")
	}
	receipt, err := replay.Load(spec.SessionID)
	if err != nil {
		return localqueue.SessionProjection{}, err
	}
	if err = l.validateCurrentAdmissionPolicy(projection.Admission.Envelope.OperationalPayload, receipt); err != nil {
		return localqueue.SessionProjection{}, err
	}
	return projection, nil
}

func (l *localRuntime) preSpawn(spec SessionSpec, env []string) ([]string, error) {
	projection, err := l.validateSecretRelease(spec)
	if err != nil {
		return nil, err
	}
	credential, err := l.auth.LoadAttempt(projection.Admission.Envelope.CredentialHandle, *projection.Attempt)
	if err != nil {
		return nil, err
	}
	filtered := make([]string, 0, len(env)+1)
	for _, entry := range env {
		if !strings.HasPrefix(entry, "DONMAI_RUNTIME_JWT=") {
			filtered = append(filtered, entry)
		}
	}
	return append(filtered, "DONMAI_RUNTIME_JWT="+credential.BearerToken()), nil
}

func (l *localRuntime) heldIDs() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	ids := make([]string, 0, len(l.held))
	for id := range l.held {
		ids = append(ids, id)
	}
	return ids
}
