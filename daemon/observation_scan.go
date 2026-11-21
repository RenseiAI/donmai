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

	// Acknowledgements of deferred observations.scan mutations, drained into
	// the heartbeat before it builds its observations report.
	ackMu       sync.Mutex
	ackApplied  []string
	ackFailures []HeartbeatMutationFailure
}

// observationScanDeferAttempts bounds how often a deferred scan retries
// after losing the slot to another scan.
const observationScanDeferAttempts = 5

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
// not acknowledged in this beat: a scan was already in flight or ran inside
// the rate-limit window, so a fresh scan runs later and acknowledges then.
var errObservationScanDeferred = errors.New("observations.scan deferred until a fresh scan completes")

// applyObservationsScan handles the observations.scan host mutation. The
// mutation is acknowledged only in the heartbeat that carries the report of
// a scan run for it. A scan that completes here is acknowledged by the next
// beat, whose report already reflects it. A scan already in flight, or one
// that ran inside the rate-limit window, never answers the request: the scan
// is deferred, and its acknowledgement rides the first beat built after the
// deferred scan completes.
func (d *Daemon) applyObservationsScan(m PendingMutation) error {
	var params struct {
		Kinds []string `json:"kinds"`
	}
	if len(m.Params) > 0 && string(m.Params) != "null" {
		if err := json.Unmarshal(m.Params, &params); err != nil {
			return fmt.Errorf("%s decode params: %w", observationsScanOp, err)
		}
	}
	_, err := d.ScanObservations(context.Background(), params.Kinds)
	var limited *observationScanLimitedError
	if errors.As(err, &limited) {
		go d.runDeferredObservationsScan(m.ID, params.Kinds)
		return errObservationScanDeferred
	}
	if err != nil {
		return fmt.Errorf("%s: %w", observationsScanOp, err)
	}
	return nil
}

// runDeferredObservationsScan waits for the scan slot, runs a fresh scan and
// queues the mutation's acknowledgement, or its failure, for the next beat.
func (d *Daemon) runDeferredObservationsScan(id string, kinds []string) {
	s := &d.observationScan
	var err error
	for attempt := 0; attempt < observationScanDeferAttempts; attempt++ {
		time.Sleep(s.waitForSlot())
		_, err = d.ScanObservations(context.Background(), kinds)
		var limited *observationScanLimitedError
		if !errors.As(err, &limited) {
			break
		}
	}
	s.ackMu.Lock()
	defer s.ackMu.Unlock()
	if err != nil {
		s.ackFailures = append(s.ackFailures, HeartbeatMutationFailure{ID: id, Error: fmt.Sprintf("%s: %v", observationsScanOp, err)})
		return
	}
	s.ackApplied = append(s.ackApplied, id)
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

// drainDeferredScanAcks hands the heartbeat the acknowledgements of deferred
// scans that have completed. The heartbeat drains before it builds its
// observations report, so an acknowledgement never rides a report that
// predates its scan.
func (d *Daemon) drainDeferredScanAcks() ([]string, []HeartbeatMutationFailure) {
	s := &d.observationScan
	s.ackMu.Lock()
	defer s.ackMu.Unlock()
	applied, failures := s.ackApplied, s.ackFailures
	s.ackApplied, s.ackFailures = nil, nil
	return applied, failures
}

func (s *Server) handleObservationScan(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Kinds []string `json:"kinds"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&body); err != nil && !errors.Is(err, io.EOF) {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid scan request: " + err.Error()})
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
