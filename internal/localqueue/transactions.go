package localqueue

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"strconv"
	"strings"

	"github.com/RenseiAI/donmai/executioncell"
)

type (
	claimTransaction struct {
		Attempt AttemptRef `json:"attempt"`
	}
	launchIntentTransaction struct {
		Attempt AttemptRef `json:"attempt"`
	}
	observeLaunchTransaction struct {
		Attempt AttemptRef              `json:"attempt"`
		Process OwnedProcessCorrelation `json:"process"`
	}
)

type observationTransaction struct {
	Attempt     AttemptRef              `json:"attempt"`
	Observation BoundedTypedObservation `json:"observation"`
}
type terminalEvent struct {
	Type       string `json:"type"`
	ScopeID    string `json:"scopeId"`
	SessionID  string `json:"sessionId"`
	Status     string `json:"status"`
	BodySHA256 string `json:"bodySha256"`
}
type terminalTransaction struct {
	Receipt     DurableReceipt    `json:"receipt"`
	Event       terminalEvent     `json:"event"`
	Publication PublicationOutbox `json:"publication"`
}
type publicationTransaction struct {
	Key      string              `json:"key"`
	Evidence PublicationEvidence `json:"evidence"`
}
type terminalCorrelation struct {
	PullRequestURL string
	CommitSHA      string
}

func decodeTransactionData[T any](raw []byte) (T, error) {
	var value T
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&value); err != nil {
		return value, err
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		return value, errors.New("trailing transaction data")
	}
	return value, nil
}

func (s *Store) stateFor(attempt AttemptRef) (*sessionState, error) {
	state := s.sessions[sessionKey(attempt.ScopeID, attempt.SessionID)]
	if state == nil {
		return nil, ErrUnknownSession
	}
	if state.attempt == nil || *state.attempt != attempt || attempt.WorkerID != s.authority.WorkerID {
		return nil, ErrAttemptMismatch
	}
	return state, nil
}

func (s *Store) applyTransaction(tx journalTransaction) error {
	switch tx.Kind {
	case kindAuthority:
		if s.revision != 0 || s.authority.ScopeID != "" {
			return errors.New("duplicate authority")
		}
		authority, err := decodeTransactionData[Authority](tx.Data)
		if err != nil || !validLocalID(authority.ScopeID) || !validLocalID(authority.HostID) ||
			!validLocalID(authority.WorkerID) || !validTimestamp(authority.CreatedAt) {
			return errors.New("invalid authority")
		}
		if authority.ScopeID != s.binding.ScopeID || authority.HostID != s.binding.HostID || authority.WorkerID != s.binding.WorkerID {
			return ErrBindingMismatch
		}
		s.authority = authority
	case kindAdmit:
		if s.authority.ScopeID == "" {
			return errors.New("admission before authority")
		}
		persisted, err := decodeTransactionData[persistedAdmissionRecord](tx.Data)
		record := restoreRecord(persisted)
		if err != nil || record.Revision != tx.Revision || record.DispatchState != DispatchPending ||
			record.CurrentAttemptID != "" {
			return errors.New("invalid admission record")
		}
		envelope := cloneAdmissionEnvelope(record.Envelope)
		digest, err := validateAdmission(&envelope, s.binding, false)
		if err != nil {
			return fmt.Errorf("invalid admitted envelope: %w", err)
		}
		if digest != record.EnvelopeSHA256 ||
			envelope.Dispatch.EnvelopeSHA256 != digest {
			return errors.New("invalid admitted envelope")
		}
		key := sessionKey(envelope.ScopeID, envelope.Session.SessionID)
		source := sourceKey(envelope.Source)
		if _, exists := s.sessions[key]; exists {
			return errors.New("duplicate session")
		}
		if _, exists := s.sources[source]; exists {
			return errors.New("duplicate source issue")
		}
		s.sessions[key] = &sessionState{record: record}
		s.sources[source] = key
	case kindClaim:
		data, err := decodeTransactionData[claimTransaction](tx.Data)
		if err != nil || !validLocalID(data.Attempt.AttemptID) || data.Attempt.Generation != 1 ||
			data.Attempt.WorkerID != s.authority.WorkerID {
			return errors.New("invalid claim attempt")
		}
		state := s.sessions[sessionKey(data.Attempt.ScopeID, data.Attempt.SessionID)]
		if state == nil || state.attempt != nil || state.record.DispatchState != DispatchPending {
			return errors.New("claim is not pending")
		}
		attempt := data.Attempt
		state.attempt = &attempt
		state.record.CurrentAttemptID = attempt.AttemptID
		state.record.DispatchState = DispatchClaimed
	case kindLaunchIntent:
		data, err := decodeTransactionData[launchIntentTransaction](tx.Data)
		if err != nil {
			return err
		}
		state, err := s.stateFor(data.Attempt)
		if err != nil || state.record.DispatchState != DispatchClaimed || hasPrestartDenial(state) {
			return errors.New("launch intent is not owned by a claimed attempt")
		}
		// Until an actual process observation or terminal result arrives, a
		// crash here has unknown launch outcome. Never auto-reenqueue it.
		state.record.DispatchState = DispatchUnknown
	case kindObserveLaunch:
		data, err := decodeTransactionData[observeLaunchTransaction](tx.Data)
		if err != nil || !validProcess(data.Process) {
			return errors.New("invalid owned-process correlation")
		}
		state, err := s.stateFor(data.Attempt)
		if err != nil || state.record.DispatchState != DispatchUnknown || state.launch != nil {
			return errors.New("launch observation is not the current unknown attempt")
		}
		process := data.Process
		state.launch = &process
		state.record.DispatchState = DispatchLaunched
	case kindObservation:
		data, err := decodeTransactionData[observationTransaction](tx.Data)
		if err != nil || !validObservation(data.Observation) {
			return errors.New("invalid typed observation")
		}
		state, err := s.stateFor(data.Attempt)
		if err != nil || state.terminal != nil {
			return errors.New("observation is not on the current active attempt")
		}
		if data.Observation.Kind == ObservationPrestartDeny && state.record.DispatchState != DispatchClaimed {
			return errors.New("prestart denial arrived after launch intent")
		}
		if data.Observation.Kind != ObservationPrestartDeny && state.record.DispatchState == DispatchClaimed {
			return errors.New("liveness cannot precede launch intent")
		}
		state.observations = append(state.observations, data.Observation)
	case kindTerminal:
		data, err := decodeTransactionData[terminalTransaction](tx.Data)
		if err != nil {
			return err
		}
		state, err := s.stateFor(data.Receipt.Attempt)
		if err != nil || state.terminal != nil || data.Receipt.Revision != tx.Revision {
			return errors.New("terminal receipt is not a new exact attempt")
		}
		if state.record.DispatchState != DispatchUnknown && state.record.DispatchState != DispatchLaunched &&
			!(state.record.DispatchState == DispatchClaimed && hasPrestartDenial(state)) {
			return errors.New("terminal arrived without launch or prestart denial evidence")
		}
		status, bodyDigest, correlation, err := validateTerminalBody(data.Receipt.Attempt, state.record.Envelope.Source, data.Receipt.Body)
		if err != nil || status != data.Receipt.Status || bodyDigest != data.Receipt.BodySHA256 {
			return errors.New("terminal body and stored digest/status disagree")
		}
		if state.record.DispatchState == DispatchClaimed && status != "failed" {
			return errors.New("prestart denial cannot become successful or cancelled terminal status")
		}
		if data.Event.Type != "session.ended" || data.Event.ScopeID != data.Receipt.Attempt.ScopeID ||
			data.Event.SessionID != data.Receipt.Attempt.SessionID || data.Event.Status != status ||
			data.Event.BodySHA256 != bodyDigest {
			return errors.New("terminal lifecycle event disagrees with receipt")
		}
		operation := publicationOperation(status)
		if data.Publication.Key != publicationKey(data.Receipt.Attempt.SessionID, bodyDigest, operation) ||
			data.Publication.SessionID != data.Receipt.Attempt.SessionID ||
			data.Publication.ResultSHA256 != bodyDigest || data.Publication.State != DispatchPending ||
			data.Publication.Operation != operation {
			return errors.New("terminal publication outbox disagrees with receipt")
		}
		if operation == PublicationGitHubPR {
			if data.Publication.URL != correlation.PullRequestURL || data.Publication.HeadSHA != correlation.CommitSHA || data.Publication.ReadAt != "" {
				return errors.New("successful publication is not bound to terminal PR/head")
			}
		} else if data.Publication.URL != "" || data.Publication.HeadSHA != "" || data.Publication.ReadAt != "" {
			return errors.New("failure comment publication cannot pre-claim external readback")
		}
		receipt := cloneValue(data.Receipt)
		state.terminal = &receipt
		state.record.DispatchState = DispatchTerminal
		s.publications[data.Publication.Key] = data.Publication
	case kindPublication:
		data, err := decodeTransactionData[publicationTransaction](tx.Data)
		if err != nil || !validPublicationEvidence(data.Evidence) {
			return errors.New("invalid publication evidence")
		}
		item, exists := s.publications[data.Key]
		state := s.sessions[sessionKey(s.binding.ScopeID, item.SessionID)]
		if !exists || item.State != DispatchPending || state == nil ||
			!publicationMatchesSource(data.Evidence, state.record.Envelope.Source, item.Operation) {
			return errors.New("publication outbox is not pending")
		}
		if item.Operation == PublicationGitHubPR && (data.Evidence.URL != item.URL || data.Evidence.HeadSHA != item.HeadSHA) {
			return errors.New("publication readback differs from terminal PR/head")
		}
		item.State = "delivered"
		item.URL = data.Evidence.URL
		item.HeadSHA = data.Evidence.HeadSHA
		item.ReadAt = data.Evidence.ReadAt
		s.publications[data.Key] = item
	default:
		return errors.New("unknown transaction kind")
	}
	return nil
}

func validProcess(value OwnedProcessCorrelation) bool {
	return value.ProcessID > 0 && validLocalID(value.ProcessStart) && validSHA256(value.SessionDetailSHA256)
}

func validObservation(value BoundedTypedObservation) bool {
	switch value.Kind {
	case ObservationLiveness, ObservationStopRequest, ObservationPrestartDeny:
	default:
		return false
	}
	return validTimestamp(value.ObservedAt) && (value.Evidence == "" || validSHA256(value.Evidence))
}

func hasPrestartDenial(state *sessionState) bool {
	for _, observation := range state.observations {
		if observation.Kind == ObservationPrestartDeny {
			return true
		}
	}
	return false
}

func publicationOperation(status string) string {
	if status == "completed" {
		return PublicationGitHubPR
	}
	return PublicationGitHubComment
}

func publicationKey(sessionID, digest, operation string) string {
	return "publication:" + sessionID + ":" + digest + ":" + operation
}

func validateTerminalBody(attempt AttemptRef, source GitHubSource, body []byte) (string, string, terminalCorrelation, error) {
	if len(body) == 0 || len(body) > 1<<20 {
		return "", "", terminalCorrelation{}, ErrInvalidEnvelope
	}
	if _, err := executioncell.NormalizeOperationalPayload(body); err != nil {
		return "", "", terminalCorrelation{}, fmt.Errorf("%w: terminal body must be unique-key JSON object: %v", ErrInvalidEnvelope, err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(body, &fields); err != nil {
		return "", "", terminalCorrelation{}, ErrInvalidEnvelope
	}
	// The exact terminal request is private 0600 evidence and legitimately
	// includes a public PR URL. Never project it into a lifecycle event or
	// outbox; reject explicit credential-bearing request members here.
	for key := range fields {
		switch strings.ToLower(key) {
		case "authorization", "token", "accesstoken", "refreshtoken", "apikey", "headers", "env", "credentialvalue":
			return "", "", terminalCorrelation{}, fmt.Errorf("%w: terminal body contains credential field", ErrInvalidEnvelope)
		}
	}
	var workerID, status string
	if err := json.Unmarshal(fields["workerId"], &workerID); err != nil || workerID != attempt.WorkerID {
		return "", "", terminalCorrelation{}, ErrAttemptMismatch
	}
	if err := json.Unmarshal(fields["status"], &status); err != nil {
		return "", "", terminalCorrelation{}, ErrInvalidEnvelope
	}
	switch status {
	case "completed", "failed", "cancelled", "blocked":
	default:
		return "", "", terminalCorrelation{}, fmt.Errorf("%w: unsupported terminal status", ErrInvalidEnvelope)
	}
	var correlation terminalCorrelation
	if raw := fields["pullRequestUrl"]; len(raw) != 0 {
		if err := json.Unmarshal(raw, &correlation.PullRequestURL); err != nil {
			return "", "", terminalCorrelation{}, ErrInvalidEnvelope
		}
	}
	if raw := fields["commitSha"]; len(raw) != 0 {
		if err := json.Unmarshal(raw, &correlation.CommitSHA); err != nil {
			return "", "", terminalCorrelation{}, ErrInvalidEnvelope
		}
	}
	if correlation.PullRequestURL != "" && !terminalPRMatchesSource(correlation.PullRequestURL, source) {
		return "", "", terminalCorrelation{}, ErrInvalidEnvelope
	}
	if correlation.CommitSHA != "" && !validHeadSHA(correlation.CommitSHA) {
		return "", "", terminalCorrelation{}, ErrInvalidEnvelope
	}
	if status == "completed" && (correlation.PullRequestURL == "" || correlation.CommitSHA == "") {
		return "", "", terminalCorrelation{}, fmt.Errorf("%w: completed development requires exact PR URL and head SHA", ErrInvalidEnvelope)
	}
	sum := sha256.Sum256(body)
	return status, hex.EncodeToString(sum[:]), correlation, nil
}

func validPublicationEvidence(value PublicationEvidence) bool {
	parsed, ok := parseCanonicalGitHubURL(value.URL)
	if !ok || !validTimestamp(value.ReadAt) {
		return false
	}
	if value.HeadSHA == "" {
		// A failed run may publish only an issue comment, with no pushed head.
		const prefix = "issuecomment-"
		if !strings.HasPrefix(parsed.Fragment, prefix) {
			return false
		}
		commentID, err := strconv.ParseUint(strings.TrimPrefix(parsed.Fragment, prefix), 10, 64)
		return err == nil && commentID > 0
	}
	return validHeadSHA(value.HeadSHA)
}

// parseCanonicalGitHubURL is shared by terminal expectation and final
// publication readback. A path-only match may not turn another origin into
// immutable PR authority.
func parseCanonicalGitHubURL(raw string) (*url.URL, bool) {
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Scheme != "https" || parsed.Host != "github.com" ||
		parsed.User != nil || parsed.Opaque != "" || parsed.Path == "" ||
		parsed.RawPath != "" || parsed.RawQuery != "" || parsed.ForceQuery {
		return nil, false
	}
	return parsed, true
}

func validHeadSHA(value string) bool {
	if len(value) != 40 && len(value) != 64 {
		return false
	}
	for _, char := range value {
		if !strings.ContainsRune("0123456789abcdef", char) {
			return false
		}
	}
	return true
}

func publicationMatchesSource(value PublicationEvidence, source GitHubSource, operation string) bool {
	if operation == PublicationGitHubPR {
		return value.HeadSHA != "" && terminalPRMatchesSource(value.URL, source)
	}
	parsed, ok := parseCanonicalGitHubURL(value.URL)
	if !ok {
		return false
	}
	path := strings.ToLower(parsed.Path)
	prefix := "/" + strings.ToLower(source.OwnerRepo) + "/"
	if !strings.HasPrefix(path, prefix) {
		return false
	}
	parts := strings.Split(strings.TrimPrefix(path, prefix), "/")
	if len(parts) != 2 || (parts[0] != "pull" && parts[0] != "issues") {
		return false
	}
	number, err := strconv.Atoi(parts[1])
	if err != nil || number <= 0 {
		return false
	}
	switch operation {
	case PublicationGitHubComment:
		return parts[0] == "issues" && value.HeadSHA == "" && number == source.IssueNumber &&
			strings.HasPrefix(parsed.Fragment, "issuecomment-")
	default:
		return false
	}
}

func terminalPRMatchesSource(raw string, source GitHubSource) bool {
	parsed, ok := parseCanonicalGitHubURL(raw)
	if !ok || parsed.Fragment != "" {
		return false
	}
	path := strings.ToLower(parsed.Path)
	prefix := "/" + strings.ToLower(source.OwnerRepo) + "/pull/"
	if !strings.HasPrefix(path, prefix) {
		return false
	}
	parts := strings.Split(strings.TrimPrefix(path, prefix), "/")
	if len(parts) != 1 {
		return false
	}
	number, err := strconv.Atoi(parts[0])
	return err == nil && number > 0
}

// Admit atomically commits canonical admission, first lifecycle event, and
// pending dispatch outbox. The live producer owns real registry/auth checks;
// this store independently rechecks the immutable shape and provenance.
func (s *Store) Admit(ctx context.Context, expected Revision, envelope AdmissionEnvelope) (AdmissionRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed || s.poisoned {
		return AdmissionRecord{}, ErrCorruptStore
	}
	if expected != s.revision {
		return AdmissionRecord{}, ErrConflict
	}
	digest, err := validateAdmission(&envelope, s.binding, true)
	if err != nil {
		return AdmissionRecord{}, err
	}
	envelope = cloneAdmissionEnvelope(envelope)
	if _, exists := s.sources[sourceKey(envelope.Source)]; exists {
		return AdmissionRecord{}, ErrAlreadyAdmitted
	}
	if _, exists := s.sessions[sessionKey(envelope.ScopeID, envelope.Session.SessionID)]; exists {
		return AdmissionRecord{}, ErrConflict
	}
	record := AdmissionRecord{
		Revision: expected + 1, Envelope: envelope, EnvelopeSHA256: digest,
		DispatchState: DispatchPending,
	}
	if err := s.commitLocked(ctx, expected, kindAdmit, persistRecord(record)); err != nil {
		return AdmissionRecord{}, err
	}
	return cloneAdmissionRecord(record), nil
}

// Claim privately fences one durable pending session to one local attempt.
func (s *Store) Claim(ctx context.Context, expected Revision, ref executioncell.SessionRef) (AttemptRef, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed || s.poisoned {
		return AttemptRef{}, ErrCorruptStore
	}
	if expected != s.revision {
		return AttemptRef{}, ErrConflict
	}
	raw, err := json.Marshal(ref)
	if err != nil {
		return AttemptRef{}, ErrInvalidEnvelope
	}
	decoded, err := executioncell.DecodeSessionRef(raw)
	if err != nil || decoded.Value().ClaimReceiptID != "" {
		return AttemptRef{}, ErrInvalidEnvelope
	}
	state := s.sessions[sessionKey(s.binding.ScopeID, ref.SessionID)]
	if state == nil {
		return AttemptRef{}, ErrUnknownSession
	}
	if state.record.DispatchState != DispatchPending || state.attempt != nil ||
		!bytes.Equal(decoded.Bytes(), mustCanonical(state.record.Envelope.Session)) {
		return AttemptRef{}, ErrAttemptMismatch
	}
	suffix, err := randomSuffix()
	if err != nil {
		return AttemptRef{}, fmt.Errorf("localqueue: create attempt: %w", err)
	}
	attempt := AttemptRef{
		ScopeID: s.binding.ScopeID, SessionID: ref.SessionID, AttemptID: "att_" + suffix,
		WorkerID: s.binding.WorkerID, Generation: 1,
	}
	if err := s.commitLocked(ctx, expected, kindClaim, claimTransaction{Attempt: attempt}); err != nil {
		return AttemptRef{}, err
	}
	return attempt, nil
}

func mustCanonical(value any) []byte {
	canonical, _ := executioncell.CanonicalJSON(value)
	return canonical
}

// BeginLaunch commits launch-start intent before any external launch call.
func (s *Store) BeginLaunch(ctx context.Context, expected Revision, attempt AttemptRef) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	state, err := s.stateFor(attempt)
	if err != nil || state.record.DispatchState != DispatchClaimed || hasPrestartDenial(state) {
		return ErrAttemptMismatch
	}
	return s.commitLocked(ctx, expected, kindLaunchIntent, launchIntentTransaction{Attempt: attempt})
}

// ObserveLaunch records actual owned process/session correlation. It cannot
// fabricate a launch from a timeout or a mere claim.
func (s *Store) ObserveLaunch(ctx context.Context, expected Revision, attempt AttemptRef, process OwnedProcessCorrelation) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	state, err := s.stateFor(attempt)
	if err != nil || state.record.DispatchState != DispatchUnknown || !validProcess(process) {
		return ErrAttemptMismatch
	}
	return s.commitLocked(ctx, expected, kindObserveLaunch, observeLaunchTransaction{Attempt: attempt, Process: process})
}

// RecordObservation durably advances exact-attempt liveness/stop/denial. A
// stale owner never refreshes a replacement attempt.
func (s *Store) RecordObservation(ctx context.Context, attempt AttemptRef, observation BoundedTypedObservation) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	state, err := s.stateFor(attempt)
	if err != nil || state.terminal != nil || !validObservation(observation) {
		return ErrAttemptMismatch
	}
	if observation.Kind == ObservationPrestartDeny && state.record.DispatchState != DispatchClaimed {
		return ErrAttemptMismatch
	}
	if observation.Kind != ObservationPrestartDeny && state.record.DispatchState == DispatchClaimed {
		return ErrAttemptMismatch
	}
	return s.commitLocked(ctx, s.revision, kindObservation, observationTransaction{Attempt: attempt, Observation: observation})
}

// CommitTerminal keeps the exact request body and its public digest in one
// atomic terminal/event/publication transaction. Exact replay returns the
// original receipt; a changed body or attempt is a conflict.
func (s *Store) CommitTerminal(ctx context.Context, attempt AttemptRef, exactRequestBytes []byte) (DurableReceipt, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed || s.poisoned {
		return DurableReceipt{}, ErrCorruptStore
	}
	state, err := s.stateFor(attempt)
	if err != nil {
		return DurableReceipt{}, err
	}
	if state.terminal != nil {
		if bytes.Equal(state.terminal.Body, exactRequestBytes) {
			return cloneValue(*state.terminal), nil
		}
		return DurableReceipt{}, ErrConflict
	}
	if state.record.DispatchState != DispatchUnknown && state.record.DispatchState != DispatchLaunched &&
		!(state.record.DispatchState == DispatchClaimed && hasPrestartDenial(state)) {
		return DurableReceipt{}, ErrAttemptMismatch
	}
	status, digest, correlation, err := validateTerminalBody(attempt, state.record.Envelope.Source, exactRequestBytes)
	if err != nil {
		return DurableReceipt{}, err
	}
	if state.record.DispatchState == DispatchClaimed && status != "failed" {
		return DurableReceipt{}, ErrAttemptMismatch
	}
	receipt := DurableReceipt{
		Attempt: attempt, Body: bytes.Clone(exactRequestBytes), BodySHA256: digest,
		Status: status, Revision: s.revision + 1,
	}
	expectedPRURL, expectedHeadSHA := "", ""
	if status == "completed" {
		expectedPRURL, expectedHeadSHA = correlation.PullRequestURL, correlation.CommitSHA
	}
	data := terminalTransaction{
		Receipt: receipt,
		Event:   terminalEvent{Type: "session.ended", ScopeID: attempt.ScopeID, SessionID: attempt.SessionID, Status: status, BodySHA256: digest},
		Publication: PublicationOutbox{
			Key: publicationKey(attempt.SessionID, digest, publicationOperation(status)), SessionID: attempt.SessionID,
			ResultSHA256: digest, Operation: publicationOperation(status), State: DispatchPending,
			URL: expectedPRURL, HeadSHA: expectedHeadSHA,
		},
	}
	if err := s.commitLocked(ctx, s.revision, kindTerminal, data); err != nil {
		return DurableReceipt{}, err
	}
	return cloneValue(receipt), nil
}

// CompletePublication records read-back evidence for exactly one pending
// operation. Ambiguous external creates must be resolved by the publisher
// before calling this method.
func (s *Store) CompletePublication(ctx context.Context, key string, evidence PublicationEvidence) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	item, exists := s.publications[key]
	state := s.sessions[sessionKey(s.binding.ScopeID, item.SessionID)]
	if !exists || item.State != DispatchPending || state == nil ||
		!validPublicationEvidence(evidence) || !publicationMatchesSource(evidence, state.record.Envelope.Source, item.Operation) {
		return ErrConflict
	}
	if item.Operation == PublicationGitHubPR && (evidence.URL != item.URL || evidence.HeadSHA != item.HeadSHA) {
		return ErrConflict
	}
	return s.commitLocked(ctx, s.revision, kindPublication, publicationTransaction{Key: key, Evidence: evidence})
}
