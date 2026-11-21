package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
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
	// one at a time, each as a fresh scan. queued holds every id that is
	// queued, running or awaiting its acknowledgement, and recent holds ids
	// acknowledged in the last observationScanRecentTTL, so a redelivered id
	// never starts a second scan.
	queue   []observationScanRequest
	queued  map[string]bool
	recent  map[string]time.Time
	working bool

	// Completed requests' acknowledgements and the exact report of the scan
	// that answered them, drained into the heartbeat before it builds its
	// observations report.
	ackMu       sync.Mutex
	ackApplied  []string
	ackFailures []HeartbeatMutationFailure
	ackReport   *observation.Report
}

// observationScanRequest is one queued observations.scan mutation.
type observationScanRequest struct {
	id    string
	kinds []string
}

// A queued request waits at most observationScanQueueDeadline for its slot
// (a running scan plus the rate window), then fails. Acknowledged ids are
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
		out.Marks = append(out.Marks, ObservationMark{Ref: ref, New: !d.observationScan.previous[ref], Enabled: d.observationEnabled(ref)})
	}
	return out
}

// observationEnabled is the seam for local enablement: every item is
// enabled until local configuration can disable one.
func (d *Daemon) observationEnabled(observation.Ref) bool { return true }

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
// answers it. The acknowledgement is sent with the exact report of that scan,
// an empty full report included, on the first beat built after it completes.
// A redelivered id that is queued, running, awaiting acknowledgement or
// recently acknowledged starts no second scan.
func (d *Daemon) applyObservationsScan(m PendingMutation) error {
	var params struct {
		Kinds []string `json:"kinds"`
	}
	if len(m.Params) > 0 && string(m.Params) != "null" {
		if err := json.Unmarshal(m.Params, &params); err != nil {
			return fmt.Errorf("%s decode params: %w", observationsScanOp, err)
		}
	}
	if err := checkObservationKinds(params.Kinds); err != nil {
		return fmt.Errorf("%s: %w", observationsScanOp, err)
	}
	s := &d.observationScan
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now()
	for id, at := range s.recent {
		if now.Sub(at) > observationScanRecentTTL {
			delete(s.recent, id)
		}
	}
	if s.queued[m.ID] {
		return errObservationScanDeferred
	}
	if _, ok := s.recent[m.ID]; ok {
		return errObservationScanDeferred
	}
	if s.queued == nil {
		s.queued = map[string]bool{}
	}
	s.queued[m.ID] = true
	s.queue = append(s.queue, observationScanRequest{id: m.ID, kinds: params.Kinds})
	if !s.working {
		s.working = true
		go d.runObservationScanQueue()
	}
	return errObservationScanDeferred
}

// runObservationScanQueue runs queued requests in order until the queue is
// empty. Each request waits for the scan slot (a running scan, then the rate
// window) and runs its own scan; the per-kind probe timeout applies once that
// scan starts.
func (d *Daemon) runObservationScanQueue() {
	s := &d.observationScan
	for {
		s.mu.Lock()
		if len(s.queue) == 0 {
			s.working = false
			s.mu.Unlock()
			return
		}
		req := s.queue[0]
		s.mu.Unlock()
		result, err := d.scanWhenSlotOpens(req.kinds, time.Now().Add(observationScanQueueDeadline))
		s.ackMu.Lock()
		if err != nil {
			s.ackFailures = append(s.ackFailures, HeartbeatMutationFailure{ID: req.id, Error: fmt.Sprintf("%s: %v", observationsScanOp, err)})
		} else {
			s.ackApplied = append(s.ackApplied, req.id)
			s.ackReport = result.Report
		}
		s.ackMu.Unlock()
		s.mu.Lock()
		s.queue = s.queue[1:]
		delete(s.queued, req.id)
		if s.recent == nil {
			s.recent = map[string]time.Time{}
		}
		s.recent[req.id] = time.Now()
		s.mu.Unlock()
	}
}

// scanWhenSlotOpens waits for the scan slot and runs one scan, retrying when
// another caller takes the slot first, until deadline.
func (d *Daemon) scanWhenSlotOpens(kinds []string, deadline time.Time) (*ObservationScanResult, error) {
	s := &d.observationScan
	for {
		wait := s.waitForSlot()
		if time.Now().Add(wait).After(deadline) {
			return nil, fmt.Errorf("observation scan slot not available within %s", observationScanQueueDeadline)
		}
		time.Sleep(wait)
		result, err := d.ScanObservations(context.Background(), kinds)
		var limited *observationScanLimitedError
		if errors.As(err, &limited) {
			continue
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

// drainDeferredScanAcks hands the heartbeat the acknowledgements of completed
// scan requests and the exact report of the latest such scan (nil when none
// completed). The heartbeat drains before it builds its observations report
// and sends that report, an empty full report included, with the
// acknowledgements.
func (d *Daemon) drainDeferredScanAcks() ([]string, []HeartbeatMutationFailure, *observation.Report) {
	s := &d.observationScan
	s.ackMu.Lock()
	defer s.ackMu.Unlock()
	applied, failures, report := s.ackApplied, s.ackFailures, s.ackReport
	s.ackApplied, s.ackFailures, s.ackReport = nil, nil, nil
	return applied, failures, report
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
