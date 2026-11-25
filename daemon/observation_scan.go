package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/RenseiAI/donmai/observation"
)

// observationsScanOp is the host mutation that requests a full rescan.
const observationsScanOp = "observations.scan"

// Scan bounds: one scan in flight, at most one a minute, and a timeout
// per kind after which the kind reports readiness unknown.
const (
	observationScanMinInterval = time.Minute
	observationScanKindTimeout = quotaProbeTimeout + 5*time.Second
)

// Typed reasons a scan is refused.
const (
	observationScanInFlight    = "scan_in_flight"
	observationScanRateLimited = "scan_rate_limited"
)

// observationKinds are the kinds this daemon scans.
var observationKinds = []string{observation.KindSubscriptionLogin}

// observationScanLimitedError refuses a scan the rate limit forbids.
type observationScanLimitedError struct {
	reason     string
	retryAfter time.Duration
}

func (e *observationScanLimitedError) Error() string { return "observation scan refused: " + e.reason }

// ObservationMark is the host-local view of one observed item. New marks
// an item absent from the previous completed scan of its kind.
type ObservationMark struct {
	observation.Ref
	New     bool `json:"new"`
	Enabled bool `json:"enabled"`
}

// ObservationScanResult is the body of the observations read and scan
// endpoints: the encoded report plus one mark per item.
type ObservationScanResult struct {
	Report *observation.Report `json:"report"`
	Marks  []ObservationMark   `json:"marks"`
}

// observationScanner tracks scans for the rate limit and the new marks.
type observationScanner struct {
	mu          sync.Mutex
	inFlight    bool
	lastAt      time.Time
	timeout     time.Duration // zero uses observationScanKindTimeout
	minInterval time.Duration // zero uses observationScanMinInterval
	probes      map[string]quotaProbeFunc
	previous    map[observation.Ref]bool
	latest      map[observation.Ref]bool

	// The observations.scan mutation queue: requests run in arrival order,
	// one at a time, each as a fresh scan. queued holds every key that is
	// queued or running, and done holds every completed outcome for the
	// last observationScanRecentTTL, so a redelivered id starts no second
	// scan. Both are scoped by registration: an embedder's scan completes
	// only on its own registration's drain, never on the primary's, and a
	// redelivery on one registration never observes another's outcome.
	queue   []observationScanRequest
	queued  map[observationScanKey]bool
	done    map[observationScanKey]observationScanOutcome
	working bool

	// stopCh is closed when the daemon stops. The worker watches it to
	// cancel the slot wait and the probes; on cancel every pending request
	// fails closed and the worker exits. stopped refuses new requests
	// fail-closed once cancellation has fired.
	stopCh  chan struct{}
	stopped bool

	// maxQueue bounds the queue; zero selects observationScanMaxQueue.
	maxQueue int

	// Completed scans wait here as one batch each: the request id the scan
	// answered plus that scan's exact report. Batches drain oldest-first
	// per registration, one per heartbeat, so every acknowledgement rides
	// with the report of its own scan on its own registration's beat.
	ackMu      sync.Mutex
	ackBatches []observationScanAckBatch
}

// observationScanKey scopes a scan request to its registration: the same
// mutation id on two registrations is two independent requests.
type observationScanKey struct {
	registration string
	id           string
}

// observationScanOutcome is one completed request: whether the platform
// should see it applied, and the exact failure text otherwise. Validation
// failures replay verbatim inside the dedupe window. Scan outcomes (a
// success or a scan-execution failure) absorb redelivery as deferred: the
// acknowledgement rides its own batch's drain with its own report, never
// as a bare replay that could merge with another scan's report.
type observationScanOutcome struct {
	applied    bool
	errText    string
	at         time.Time
	validation bool
}

// observationScanAckBatch is one completed scan's heartbeat payload: the id
// it answered and that scan's exact report (nil when the scan failed).
// registration is the lane that queued it; only that lane's drain returns it.
type observationScanAckBatch struct {
	applied      []string
	failures     []HeartbeatMutationFailure
	report       *observation.Report
	registration string
}

// errObservationScanCancelled aborts queued and running scans when the
// daemon stops. Callers wrap it with the operation, so the heartbeat
// failure matches the replayed outcome text exactly.
var errObservationScanCancelled = errors.New("cancelled: daemon stopping")

// observationScanMaxQueue caps queued scan requests: each one costs at
// least one rate-limited scan slot, so a deeper queue is backlog rather
// than buffering. Overflow fails typed so the platform can retry it later.
const observationScanMaxQueue = 32

// observationScanMaxRetainedOutcomes bounds completed outcomes: validation
// failures cost no scan, so without a cap a burst of distinct invalid ids
// grows the map until its TTL. The oldest outcome by completion time goes
// first, mirroring the host upgrade ledger bound.
const observationScanMaxRetainedOutcomes = 128

// observationScanRequest is one queued observations.scan mutation.
type observationScanRequest struct {
	id           string
	kinds        []string
	registration string
}

// A queued request waits at most observationScanQueueDeadline for its slot
// (a running scan plus the rate window), then fails. Completed outcomes are
// remembered for observationScanRecentTTL to absorb redelivery.
const (
	observationScanQueueDeadline = 10 * time.Minute
	observationScanRecentTTL     = 10 * time.Minute
)

func (s *observationScanner) interval() time.Duration {
	if s.minInterval > 0 {
		return s.minInterval
	}
	return observationScanMinInterval
}

// ScanObservations runs a full scan of kinds (all kinds when empty) now:
// it probes every quota harness, then reports what the probes found.
func (d *Daemon) ScanObservations(ctx context.Context, kinds []string) (*ObservationScanResult, error) {
	if err := checkObservationKinds(kinds); err != nil {
		return nil, err
	}
	s := &d.observationScan
	now := time.Now()
	s.mu.Lock()
	switch {
	case s.inFlight:
		s.mu.Unlock()
		return nil, &observationScanLimitedError{reason: observationScanInFlight}
	case !s.lastAt.IsZero() && now.Sub(s.lastAt) < s.interval():
		retry := s.interval() - now.Sub(s.lastAt)
		s.mu.Unlock()
		return nil, &observationScanLimitedError{reason: observationScanRateLimited, retryAfter: retry}
	}
	s.inFlight, s.lastAt = true, now
	probes, timeout := s.probes, s.timeout
	s.mu.Unlock()
	if probes == nil {
		probes = d.quotaProbes()
	}
	if timeout == 0 {
		timeout = observationScanKindTimeout
	}
	timedOut := runObservationProbes(ctx, d.quota, probes, timeout)
	report, err := d.observationReport(time.Now(), timedOut)
	s.mu.Lock()
	defer s.mu.Unlock()
	s.inFlight = false
	if err != nil {
		return nil, err
	}
	s.previous, s.latest = s.latest, map[observation.Ref]bool{}
	for _, it := range report.Items {
		s.latest[it.Ref()] = true
	}
	return d.markObservations(report), nil
}

// ObservationsNow returns the current report and marks without scanning.
func (d *Daemon) ObservationsNow() (*ObservationScanResult, error) {
	report, err := d.observationReport(time.Now(), false)
	if err != nil {
		return nil, err
	}
	d.observationScan.mu.Lock()
	defer d.observationScan.mu.Unlock()
	return d.markObservations(report), nil
}

// observationReport is a full report over every quota slot; it is empty,
// not nil, when no slot holds quota state.
func (d *Daemon) observationReport(now time.Time, timedOut bool) (*observation.Report, error) {
	slots := append([]quotaSlot{{accounts: d.quota.snapshot()}}, d.extraQuotaSlots()...)
	report, err := subscriptionLoginReport(slots, now, timedOut)
	if err == nil && report == nil {
		report, err = observation.NewReport(now, true, observationKinds, nil)
	}
	if err != nil {
		return nil, fmt.Errorf("observation report: %w", err)
	}
	return report, nil
}

// markObservations marks each item; the caller holds observationScan.mu.
func (d *Daemon) markObservations(report *observation.Report) *ObservationScanResult {
	out := &ObservationScanResult{Report: report, Marks: []ObservationMark{}}
	for _, it := range report.Items {
		ref := it.Ref()
		out.Marks = append(out.Marks, ObservationMark{Ref: ref, New: !d.observationScan.previous[ref], Enabled: d.observationEnabled(it)})
	}
	return out
}

// observationEnabled is the local enabled flag: false only when
// daemon.yaml disables the item's (kind, slot, harness).
func (d *Daemon) observationEnabled(it observation.Item) bool {
	var probe struct {
		Attributes struct {
			Harness string `json:"harness"`
		} `json:"attributes"`
	}
	raw, err := json.Marshal(it)
	if err != nil || json.Unmarshal(raw, &probe) != nil {
		return true
	}
	ref := it.Ref()
	local, ok := d.localObservationItem(observationSlotKey{ref.Kind, ref.Slot, probe.Attributes.Harness})
	return !ok || !local.Disabled
}

// runObservationProbes runs every probe at once and reports whether the
// scan overran its timeout. A late probe still feeds the cache.
func runObservationProbes(ctx context.Context, q *quotaState, probes map[string]quotaProbeFunc, timeout time.Duration) bool {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	var wg sync.WaitGroup
	for _, probe := range probes {
		wg.Add(1)
		go func(p quotaProbeFunc) {
			defer wg.Done()
			p(ctx, q)
		}(probe)
	}
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
		return false
	case <-ctx.Done():
		return true
	}
}

func checkObservationKinds(kinds []string) error {
	for _, k := range kinds {
		if k != observation.KindSubscriptionLogin {
			return fmt.Errorf("unknown observation kind %q", k)
		}
	}
	return nil
}

// errObservationScanDeferred reports that an observations.scan mutation is
// not acknowledged in this beat: it is queued, and its acknowledgement rides
// the first beat built after its scan completes.
var errObservationScanDeferred = errors.New("observations.scan queued until its scan completes")

// applyObservationsScan queues the observations.scan host mutation. Every
// request is answered by a fresh scan run for it, in arrival order: a scan
// already in flight, or one that ran inside the rate-limit window, never
// answers it. The acknowledgement is sent with the exact report of that
// scan, an empty full report included, on the first beat built after its
// batch drains on its own registration; batches drain oldest-first per
// registration, one per beat, so a later scan's report never answers an
// earlier request and one registration never acknowledges another's scan.
// A redelivered id that is queued, running, or awaiting its own
// registration's handoff is absorbed, and a redelivered validation failure
// replays its exact text inside the dedupe window. A redelivered completed
// scan absorbs too: its acknowledgement already rides its own batch's drain
// with its own report, so a bare replay must never merge with another
// scan's report on a later beat.
func (d *Daemon) applyObservationsScan(m PendingMutation, registration string) error {
	if strings.TrimSpace(m.ID) == "" {
		return fmt.Errorf("%s requires an id", observationsScanOp)
	}
	s := &d.observationScan
	key := observationScanKey{registration: registration, id: m.ID}
	// Snapshot the undrained batches before taking mu: drain takes ackMu
	// before mu, so nesting the check inside mu would invert the order.
	// A redelivery answered here is always safe to defer: the pending
	// batch will acknowledge it on its own registration's handoff, or the
	// handoff will have refreshed its outcome by the next redelivery.
	undrained := s.hasUndrainedBatch(m.ID, registration)
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now()
	for k, outcome := range s.done {
		if now.Sub(outcome.at) > observationScanRecentTTL {
			delete(s.done, k)
		}
	}
	// The dedupe check runs before the stop refusal, so stop refuses only
	// new ids: a redelivered id that is queued, undrained, or done already
	// has its one outcome decided, and a fresh cancellation would report it
	// a second, contradicting way in the same beat. It also runs before
	// parameter validation, so a redelivered id never re-reports a
	// validation failure differently. An id with a
	// completed but undrained batch is absorbed the same way: its
	// acknowledgement is already decided and awaits only its own
	// registration's heartbeat handoff, so it must not start a second scan.
	if s.queued[key] {
		return errObservationScanDeferred
	}
	if undrained {
		return errObservationScanDeferred
	}
	if outcome, ok := s.done[key]; ok {
		if outcome.validation {
			return errors.New(outcome.errText)
		}
		return errObservationScanDeferred
	}
	if s.stopped {
		return fmt.Errorf("%s: %v", observationsScanOp, errObservationScanCancelled)
	}
	var params struct {
		Kinds []string `json:"kinds"`
	}
	if len(m.Params) > 0 && string(m.Params) != "null" {
		if err := json.Unmarshal(m.Params, &params); err != nil {
			err = fmt.Errorf("%s decode params: %w", observationsScanOp, err)
			s.recordScanOutcomeLocked(key, err, now, true)
			return err
		}
	}
	if err := checkObservationKinds(params.Kinds); err != nil {
		err = fmt.Errorf("%s: %w", observationsScanOp, err)
		s.recordScanOutcomeLocked(key, err, now, true)
		return err
	}
	if limit := s.maxQueueOrDefault(); len(s.queue) >= limit {
		return fmt.Errorf("%s queue full (%d queued)", observationsScanOp, len(s.queue))
	}
	if s.queued == nil {
		s.queued = map[observationScanKey]bool{}
	}
	s.queued[key] = true
	s.queue = append(s.queue, observationScanRequest{id: m.ID, kinds: params.Kinds, registration: registration})
	if s.stopCh == nil {
		s.stopCh = make(chan struct{})
	}
	if !s.working {
		s.working = true
		go d.runObservationScanQueue()
	}
	return errObservationScanDeferred
}

// hasUndrainedBatch reports whether a completed scan's batch for id on
// registration still awaits its own registration's heartbeat handoff. The
// caller must not hold observationScan.mu.
func (s *observationScanner) hasUndrainedBatch(id, registration string) bool {
	s.ackMu.Lock()
	defer s.ackMu.Unlock()
	for _, batch := range s.ackBatches {
		if batch.registration != registration {
			continue
		}
		for _, appliedID := range batch.applied {
			if appliedID == id {
				return true
			}
		}
		for _, failure := range batch.failures {
			if failure.ID == id {
				return true
			}
		}
	}
	return false
}

// recordScanOutcomeLocked remembers a terminal outcome. Validation
// failures replay verbatim on redelivery; scan outcomes (success or
// execution failure) absorb redelivery as deferred so the acknowledgement
// rides its own batch's drain with its own report. The caller holds
// observationScan.mu.
func (s *observationScanner) recordScanOutcomeLocked(key observationScanKey, err error, at time.Time, validation bool) {
	if s.done == nil {
		s.done = map[observationScanKey]observationScanOutcome{}
	}
	outcome := observationScanOutcome{at: at, validation: validation}
	if err == nil {
		outcome.applied = true
	} else {
		outcome.errText = err.Error()
	}
	s.done[key] = outcome
	for len(s.done) > observationScanMaxRetainedOutcomes {
		var oldest observationScanKey
		var oldestAt time.Time
		first := true
		found := false
		for k, o := range s.done {
			older := first || o.at.Before(oldestAt) || (o.at.Equal(oldestAt) && (k.registration < oldest.registration || (k.registration == oldest.registration && k.id < oldest.id)))
			if older {
				oldest, oldestAt, first = k, o.at, false
				found = true
			}
		}
		if !found {
			break
		}
		delete(s.done, oldest)
	}
}

func (s *observationScanner) maxQueueOrDefault() int {
	if s.maxQueue > 0 {
		return s.maxQueue
	}
	return observationScanMaxQueue
}

// cancelObservationScans fails the scan queue closed for daemon stop: new
// requests refuse at once, the worker and the slot wait observe the cancel
// context, every still-queued request is reported as a failure if a beat
// still goes out, and the worker exits.
func (d *Daemon) cancelObservationScans() {
	s := &d.observationScan
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.stopped {
		return
	}
	s.stopped = true
	if s.stopCh != nil {
		close(s.stopCh)
	}
}

// runObservationScanQueue runs queued requests in order until the queue is
// empty. Each request waits for the scan slot (a running scan, then the rate
// window) and runs its own scan; the per-kind probe timeout applies once that
// scan starts. On daemon stop the slot wait and the probes observe the
// scanner's cancel context, every still-queued request fails closed, and the
// worker exits without starting another scan.
func (d *Daemon) runObservationScanQueue() {
	s := &d.observationScan
	s.mu.Lock()
	stopCh := s.stopCh
	s.mu.Unlock()
	// The slot wait and the probes observe ctx instead of sleeping: the
	// watcher below cancels it when the daemon stops, and the deferred
	// cancel releases the watcher when the queue drains.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if stopCh != nil {
		watchDone := make(chan struct{})
		defer close(watchDone)
		go func() {
			select {
			case <-stopCh:
				cancel()
			case <-watchDone:
			}
		}()
	}
	for {
		s.mu.Lock()
		if len(s.queue) == 0 {
			s.working = false
			s.mu.Unlock()
			return
		}
		if ctx.Err() != nil {
			cancelErr := fmt.Errorf("%s: %v", observationsScanOp, errObservationScanCancelled)
			pending := s.queue
			s.queue = nil
			now := time.Now()
			for _, req := range pending {
				delete(s.queued, observationScanKey{registration: req.registration, id: req.id})
				s.recordScanOutcomeLocked(observationScanKey{registration: req.registration, id: req.id}, cancelErr, now, false)
			}
			s.working = false
			s.mu.Unlock()
			// One batch per request preserves registration isolation: each
			// lane drains only its own failures with no cross-lane merge.
			s.ackMu.Lock()
			for _, req := range pending {
				s.ackBatches = append(s.ackBatches, observationScanAckBatch{
					failures:     []HeartbeatMutationFailure{{ID: req.id, Error: cancelErr.Error()}},
					registration: req.registration,
				})
			}
			s.ackMu.Unlock()
			return
		}
		req := s.queue[0]
		s.mu.Unlock()
		result, scanErr := d.scanWhenSlotOpens(ctx, req.kinds, time.Now().Add(observationScanQueueDeadline))
		batch := observationScanAckBatch{registration: req.registration}
		var batchErr error
		if scanErr != nil {
			batchErr = fmt.Errorf("%s: %v", observationsScanOp, scanErr)
			batch.failures = append(batch.failures, HeartbeatMutationFailure{ID: req.id, Error: batchErr.Error()})
		} else {
			batch.applied = append(batch.applied, req.id)
			batch.report = result.Report
		}
		s.mu.Lock()
		s.queue = s.queue[1:]
		delete(s.queued, observationScanKey{registration: req.registration, id: req.id})
		s.recordScanOutcomeLocked(observationScanKey{registration: req.registration, id: req.id}, batchErr, time.Now(), false)
		s.mu.Unlock()
		s.ackMu.Lock()
		s.ackBatches = append(s.ackBatches, batch)
		s.ackMu.Unlock()
	}
}

// scanWhenSlotOpens waits for the scan slot and runs one scan, retrying when
// another caller takes the slot first, until deadline. The wait and the scan
// observe ctx: on daemon stop the wait aborts instead of sleeping out the
// rate window, and an already-running scan's probes abort with it.
func (d *Daemon) scanWhenSlotOpens(ctx context.Context, kinds []string, deadline time.Time) (*ObservationScanResult, error) {
	s := &d.observationScan
	for {
		wait := s.waitForSlot()
		if time.Now().Add(wait).After(deadline) {
			return nil, fmt.Errorf("observation scan slot not available within %s", observationScanQueueDeadline)
		}
		if wait > 0 {
			timer := time.NewTimer(wait)
			select {
			case <-ctx.Done():
				timer.Stop()
				return nil, errObservationScanCancelled
			case <-timer.C:
			}
		}
		if ctx.Err() != nil {
			return nil, errObservationScanCancelled
		}
		result, err := d.ScanObservations(ctx, kinds)
		var limited *observationScanLimitedError
		if errors.As(err, &limited) {
			continue
		}
		if ctx.Err() != nil {
			return nil, errObservationScanCancelled
		}
		return result, err
	}
}

// waitForSlot is how long until a new scan may start.
func (s *observationScanner) waitForSlot() time.Duration {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.inFlight {
		return 50 * time.Millisecond
	}
	if s.lastAt.IsZero() {
		return 0
	}
	if wait := s.interval() - time.Since(s.lastAt); wait > 0 {
		return wait
	}
	return 0
}

// drainDeferredScanAcks hands the primary heartbeat the oldest completed
// scan's batch for its own registration: its request id and that scan's
// exact report (nil when the scan failed). Embedder lanes drain only their
// own registration via DrainDeferredScanAcksFor, so one registration never
// acknowledges another's scan.
func (d *Daemon) drainDeferredScanAcks() ([]string, []HeartbeatMutationFailure, *observation.Report) {
	return d.drainDeferredScanAcksFor(primaryObservationRegistration)
}

// DrainDeferredScanAcksFor hands the named registration's heartbeat its
// oldest completed scan batch. Embedders wire it as their heartbeat lane's
// DrainDeferredAcks alongside ApplyObservationMutations as OnPendingMutations,
// so each registration's scan completes only on its own beat with its own
// report. One batch drains per call, so every acknowledgement rides with
// the report of its own scan and later batches wait for later beats. The
// dedupe window refreshes at this handoff: the 10 minutes run from the
// heartbeat taking the acknowledgement, not from the scan completing.
func (d *Daemon) DrainDeferredScanAcksFor(registration string) ([]string, []HeartbeatMutationFailure, *observation.Report) {
	return d.drainDeferredScanAcksFor(registration)
}

func (d *Daemon) drainDeferredScanAcksFor(registration string) ([]string, []HeartbeatMutationFailure, *observation.Report) {
	s := &d.observationScan
	s.ackMu.Lock()
	idx := -1
	for i, b := range s.ackBatches {
		if b.registration == registration {
			idx = i
			break
		}
	}
	if idx < 0 {
		s.ackMu.Unlock()
		return nil, nil, nil
	}
	batch := s.ackBatches[idx]
	s.ackBatches = append(s.ackBatches[:idx], s.ackBatches[idx+1:]...)
	s.ackMu.Unlock()
	now := time.Now()
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, id := range batch.applied {
		key := observationScanKey{registration: registration, id: id}
		if outcome, ok := s.done[key]; ok {
			outcome.at = now
			s.done[key] = outcome
		} else {
			// The outcome was TTL-pruned while the batch awaited its
			// handoff: re-record it so a later redelivery absorbs instead
			// of starting a second scan for an already-answered id.
			s.recordScanOutcomeLocked(key, nil, now, false)
		}
	}
	for _, failure := range batch.failures {
		key := observationScanKey{registration: registration, id: failure.ID}
		if outcome, ok := s.done[key]; ok {
			outcome.at = now
			s.done[key] = outcome
		} else {
			s.recordScanOutcomeLocked(key, errors.New(failure.Error), now, false)
		}
	}
	return batch.applied, batch.failures, batch.report
}

func (s *Server) handleObservations(w http.ResponseWriter, _ *http.Request) {
	result, err := s.daemon.ObservationsNow()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func (s *Server) handleObservationScan(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Kinds []string `json:"kinds"`
	}
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096))
	if err := dec.Decode(&body); err != nil && !errors.Is(err, io.EOF) {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid scan request: " + err.Error()})
		return
	}
	// One optional JSON object, then end of input: trailing data is refused
	// before any scan starts, so a malformed request never takes the slot.
	if err := dec.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid scan request: trailing data after the JSON object"})
		return
	}
	result, err := s.daemon.ScanObservations(r.Context(), body.Kinds)
	var limited *observationScanLimitedError
	switch {
	case errors.As(err, &limited):
		writeJSON(w, http.StatusTooManyRequests, map[string]any{
			"error": err.Error(), "reason": limited.reason,
			"retryAfterSeconds": int(math.Ceil(limited.retryAfter.Seconds())),
		})
	case err != nil:
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
	default:
		writeJSON(w, http.StatusOK, result)
	}
}
