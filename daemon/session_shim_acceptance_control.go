package daemon

import (
	"bytes"
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/RenseiAI/donmai/sessionshim"
	"github.com/RenseiAI/donmai/shimwire"
)

const (
	sessionShimAcceptanceRoute   = "/api/daemon/session-shim/acceptance/"
	maxSessionShimAcceptanceBody = 4 << 10
	acceptanceGapResizeCycles    = 4096
	// acceptanceRingBytes is the shim-owned output ring budget for a session
	// launched while the acceptance-control seam is armed.
	//
	// The seam exists to drive the ring past eviction so the product's real
	// recovery path — one declared Gap, its exact recovery Snapshot, a continued
	// sequence — is observable. Its own volume does not come close: 4096 resize
	// cycles plus 128 redraws produce about 50 KB of frames, which is 0.6% of the
	// 8 MiB production budget, so nothing was ever evicted and the lane proved
	// nothing while appearing to pass. Sizing the ring to the seam is the cheap
	// half of that fix; sizing the seam to an 8 MiB ring would need roughly
	// 700,000 cycles, every one of which also has to cross the daemon's consumer
	// and the composing carrier.
	//
	// The size is sourced from the seam, not chosen: this burst puts at least
	// acceptanceGapResizeCycles applied-Resize frames into the ring, and an
	// applied-Resize payload at these geometries is four bytes, so the seam is
	// guaranteed to produce at least 16 KiB of ring-accounted payload. A 4 KiB
	// budget therefore evicts by a factor of four with nothing left to chance,
	// and still retains about a thousand frames of replay window.
	// TestAcceptanceRingIsSmallerThanTheSeamGuarantees fails if the two ever
	// drift apart.
	//
	// This value is NEVER a default. It reaches a shim only through
	// acceptanceLaunchRingBytes, which is gated on the same private token file
	// that makes the acceptance route exist at all.
	acceptanceRingBytes = 4 << 10
)

// acceptanceLaunchRingBytes reports the ring override a newly launched shim
// should carry, or 0 for every ordinary launch.
//
// The gate is deliberately the same one the control route uses: without a
// configured, private, well-permissioned token file there is no acceptance seam
// and no override, and a production daemon is byte-for-byte unchanged.
func acceptanceLaunchRingBytes() int {
	if !sessionShimAcceptanceTokenConfigured() {
		return 0
	}
	return acceptanceRingBytes
}

var errSessionShimAcceptanceFenceRefused = errors.New("restart_fence_refused")

type acceptanceRefusalState uint8

const (
	acceptanceRefusalArmed acceptanceRefusalState = iota + 1
	acceptanceRefusalObserved
)

type sessionShimAcceptanceRequest struct {
	OrgID        string `json:"orgId"`
	SessionID    string `json:"sessionId"`
	ShimID       string `json:"shimId,omitempty"`
	ProcessEpoch uint64 `json:"processEpoch,omitempty"`
}

// handleSessionShimAcceptanceControl is a dormant installed-artifact test
// seam. The route is indistinguishable from an absent route unless a private
// token file is explicitly configured, and every mutating action is bound to
// an exact lifecycle already owned by this daemon. Its responses are never an
// evidence source; callers must re-observe status, heartbeat, wire, and process
// authority independently.
func (s *Server) handleSessionShimAcceptanceControl(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost || !sessionShimAcceptanceAuthorized(r) {
		http.NotFound(w, r)
		return
	}
	action := strings.TrimPrefix(r.URL.Path, sessionShimAcceptanceRoute)
	if action == "check" {
		if r.URL.Path != sessionShimAcceptanceRoute+"check" {
			http.NotFound(w, r)
			return
		}
		w.WriteHeader(http.StatusNoContent)
		return
	}
	var request sessionShimAcceptanceRequest
	if err := decodeSessionShimAcceptanceRequest(r.Body, &request); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid acceptance control request"})
		return
	}
	if err := request.validate(); err != nil {
		http.NotFound(w, r)
		return
	}

	var err error
	switch action {
	case "force-gap":
		err = s.daemon.forceSessionShimAcceptanceGap(request.identity())
	case "quarantine-arm":
		err = s.daemon.armSessionShimAcceptanceQuarantine(request.identity())
	case "quarantine-clear":
		err = s.daemon.clearSessionShimAcceptanceQuarantine(request.incarnation())
	case "fence-refuse-arm":
		err = s.daemon.armSessionShimAcceptanceFenceRefusal(request.identity())
	case "fence-refuse-clear":
		err = s.daemon.clearSessionShimAcceptanceFenceRefusal(request.identity())
	case "cleanup":
		err = s.daemon.cleanupSessionShimAcceptanceControl(request)
	default:
		http.NotFound(w, r)
		return
	}
	if err != nil {
		// The seam is deliberately non-disclosing. The independent fixture owns
		// detailed diagnostics and evidence; this endpoint only acknowledges that
		// its exact mutation was accepted.
		http.NotFound(w, r)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// sessionShimAcceptanceTokenConfigured reports whether a private acceptance
// token file is present and safely permissioned. It reads no request and grants
// nothing on its own.
func sessionShimAcceptanceTokenConfigured() bool {
	_, ok := sessionShimAcceptanceToken()
	return ok
}

func sessionShimAcceptanceToken() ([]byte, bool) {
	path := strings.TrimSpace(os.Getenv(sessionShimAcceptanceTokenPathEnvironment()))
	if path == "" || !filepath.IsAbs(path) {
		return nil, false
	}
	root, err := os.OpenRoot(filepath.Dir(path))
	if err != nil {
		return nil, false
	}
	defer func() { _ = root.Close() }()
	name := filepath.Base(path)
	info, err := root.Stat(name)
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0o077 != 0 || info.Size() <= 0 || info.Size() > 512 {
		return nil, false
	}
	want, err := root.ReadFile(name)
	if err != nil {
		return nil, false
	}
	want = bytes.TrimSpace(want)
	if len(want) < 32 || len(want) > 256 {
		return nil, false
	}
	return want, true
}

func sessionShimAcceptanceAuthorized(r *http.Request) bool {
	want, ok := sessionShimAcceptanceToken()
	if !ok {
		return false
	}
	got := strings.TrimSpace(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "))
	return len(got) == len(want) && subtle.ConstantTimeCompare([]byte(got), want) == 1
}

func sessionShimAcceptanceTokenPathEnvironment() string {
	return strings.Join([]string{"DONMAI", "SESSION", "SHIM", "ACCEPTANCE", "TOKEN", "FILE"}, "_")
}

func decodeSessionShimAcceptanceRequest(body io.Reader, out *sessionShimAcceptanceRequest) error {
	dec := json.NewDecoder(io.LimitReader(body, maxSessionShimAcceptanceBody+1))
	dec.DisallowUnknownFields()
	if err := dec.Decode(out); err != nil {
		return err
	}
	var extra any
	if err := dec.Decode(&extra); !errors.Is(err, io.EOF) {
		return errors.New("trailing request data")
	}
	return nil
}

func (r sessionShimAcceptanceRequest) validate() error {
	if err := r.identity().Validate(); err != nil {
		return err
	}
	if (r.ShimID == "") != (r.ProcessEpoch == 0) {
		return errors.New("partial shim incarnation")
	}
	return nil
}

func (r sessionShimAcceptanceRequest) identity() sessionshim.Identity {
	return sessionshim.Identity{OrgID: r.OrgID, SessionID: r.SessionID}
}

func (r sessionShimAcceptanceRequest) incarnation() shimIncarnation {
	return shimIncarnation{identity: r.identity(), shimID: r.ShimID, processEpoch: r.ProcessEpoch}
}

func (d *Daemon) forceSessionShimAcceptanceGap(id sessionshim.Identity) error {
	if _, err := d.adoptedShimEntry(id.OrgID, id.SessionID); err != nil {
		return err
	}
	// Alternate real PTY geometry and attributed input. A terminal application
	// redraws through the shim-owned PTY/ring path; the independent viewer later
	// proves whether this volume actually evicted its requested resume point.
	for i := 0; i < acceptanceGapResizeCycles; i++ {
		cols := uint32(99 + (i & 1))
		rows := uint32(29 + ((i >> 1) & 1))
		if err := d.ResizeAdoptedSessionShim(id.OrgID, id.SessionID, cols, rows, 0, 0); err != nil {
			return fmt.Errorf("acceptance resize %d: %w", i, err)
		}
		if i%32 == 0 {
			if err := d.WriteAdoptedSessionShimInput(id.OrgID, id.SessionID, []byte{0x0c}); err != nil {
				return fmt.Errorf("acceptance redraw %d: %w", i, err)
			}
		}
	}
	return nil
}

func (d *Daemon) armSessionShimAcceptanceQuarantine(id sessionshim.Identity) error {
	originalShimID, originalProcessEpoch, err := d.sessionShimAcceptanceAdoptedCorrelation(id)
	if err != nil {
		return err
	}
	registry, err := d.sessionShimRegistry()
	if err != nil {
		return err
	}
	entries, err := registry.Scan()
	if err != nil {
		return err
	}
	var candidate *sessionshim.Record
	for _, scanned := range entries {
		if scanned.Err != nil || scanned.Record.Identity() != id {
			continue
		}
		if scanned.Record.ShimID == originalShimID && scanned.Record.ProcessEpoch == originalProcessEpoch {
			continue
		}
		if candidate != nil {
			return errors.New("multiple unexpected shim correlations")
		}
		recordCopy := scanned.Record
		candidate = &recordCopy
	}
	if candidate == nil {
		return errors.New("no unexpected shim correlation")
	}
	alive, err := (sessionshim.ProcessIdentity{PID: candidate.PID, StartedAt: candidate.ProcessStartedAt}).Alive()
	if err != nil || !alive {
		return errors.New("unexpected shim process is not live")
	}
	if _, err := shimwire.Negotiate(candidate.ProtocolMin, candidate.ProtocolMax, shimwire.ProtocolMin, shimwire.ProtocolMax); err == nil {
		return errors.New("unexpected shim is protocol-compatible")
	}
	q := sessionshim.NewQuarantinedSession(*candidate, sessionshim.QuarantineProtocolMismatch, "acceptance fixture protocol range has no overlap", time.Now())
	incarnation := shimIncarnation{identity: id, shimID: candidate.ShimID, processEpoch: candidate.ProcessEpoch}
	d.shims.mu.Lock()
	if _, exists := d.shims.acceptanceQuarantine[incarnation]; exists {
		d.shims.mu.Unlock()
		return nil
	}
	d.upsertShimQuarantineLocked(q)
	d.shims.acceptanceQuarantine[incarnation] = sessionshim.ProcessIdentity{PID: candidate.PID, StartedAt: candidate.ProcessStartedAt}
	d.shims.mu.Unlock()
	// Simulating a quarantine means simulating all of it. A real quarantine is
	// durably published; one that is not leaves the host arguing with the
	// platform about a session the platform has never heard of.
	d.publishSessionShimProjection(context.Background(), id.OrgID)
	return nil
}

func (d *Daemon) clearSessionShimAcceptanceQuarantine(incarnation shimIncarnation) error {
	if incarnation.shimID == "" || incarnation.processEpoch == 0 {
		return errors.New("exact shim incarnation is required")
	}
	d.shims.mu.Lock()
	process, exists := d.shims.acceptanceQuarantine[incarnation]
	d.shims.mu.Unlock()
	if !exists {
		return nil
	}
	alive, err := process.Alive()
	if err != nil || alive {
		return errors.New("mutator-owned shim process remains live")
	}
	registry, err := d.sessionShimRegistry()
	if err != nil {
		return err
	}
	present, err := registry.HasIncarnation(incarnation.identity, incarnation.shimID, incarnation.processEpoch)
	if err != nil || present {
		return errors.New("mutator-owned shim record remains live")
	}
	// The helper reaps its own harness process GROUP, verifies the exact
	// recorded incarnation is gone, and durably publishes a real tombstone
	// before it exits — so this lineage leaves the way every other
	// quarantined-then-terminal lineage leaves: through the production
	// reconcile, which reports shim_terminal_tombstone evidence for the exact
	// incarnation, drops the quarantine, and republishes the complete batch.
	//
	// Nothing is staged as abandoned here, and nothing is manufactured. An
	// abandoned disposition closes what the daemon owes the composer and never
	// what the session owes the fence (§D10), so a lineage cleared that way
	// holds the release predicate forever — the session it belongs to can
	// never terminalize afterwards. Driving the reconcile is also why this
	// waits rather than returning: an acceptance clear whose caller then
	// observes an unreconciled projection would be reporting the seam's own
	// latency as product behaviour. The bound is the terminal path's own
	// settle window — the mutator already proved the record is withdrawn, and
	// PutTombstone publishes the proof BEFORE it withdraws the record, so the
	// tombstone is on disk by the time this runs.
	// ONE clock. The wait sleeps in real time, so the deadline is real time
	// too; mixing an injectable now() with a real Sleep makes the bound mean
	// different things in a test and on a host.
	deadline := time.Now().Add(tombstoneSettleWindow)
	for {
		d.reconcileQuarantinedTombstones()
		quarantined, tombstoned := d.sessionShimLineageDisposition(incarnation)
		if !quarantined && tombstoned {
			break
		}
		if !time.Now().Before(deadline) {
			return errors.New("acceptance clear: the quarantined lineage did not reconcile through its terminal tombstone")
		}
		time.Sleep(acceptanceClearPollInterval)
	}
	// The quarantine set changed, so the projection has to be republished from
	// HERE. The platform compares each beat's quarantine set against the
	// snapshot the last batch commit stored and demotes the host to `draining`
	// when the two disagree. Publishing from inside the reconcile instead would
	// put a blocking durable commit on every occupancy and heartbeat surface
	// that calls it, including the middle of a beat's own projection build.
	if err := d.republishSessionShimProjection(context.Background(), incarnation.identity.OrgID); err != nil {
		return err
	}
	d.shims.mu.Lock()
	delete(d.shims.acceptanceQuarantine, incarnation)
	d.shims.mu.Unlock()
	return nil
}

// acceptanceClearPollInterval paces the wait for the production reconcile. It
// is derived from the settle window rather than picked: a 25ms poll spent the
// whole window re-driving a reconcile whose own commit is rate-limited anyway.
const acceptanceClearPollInterval = tombstoneSettleWindow / 20

// sessionShimLineageDisposition reports whether one exact incarnation is still
// projected quarantined, and whether this daemon retains a terminal tombstone
// for it.
//
// Both halves are needed because the reconcile runs from every occupancy and
// heartbeat surface: by the time an acceptance clear arrives the lineage may
// already have left through its tombstone, and the tombstone itself is disposed
// once the durable handoff succeeds. "Gone" alone would let a lineage that
// vanished some other way pass as a reconciled one.
func (d *Daemon) sessionShimLineageDisposition(incarnation shimIncarnation) (quarantined, tombstoned bool) {
	d.shims.mu.RLock()
	defer d.shims.mu.RUnlock()
	for _, q := range d.shims.quarantined {
		if q.Identity() == incarnation.identity && q.ShimID == incarnation.shimID && q.ProcessEpoch == incarnation.processEpoch {
			quarantined = true
			break
		}
	}
	for _, t := range d.shims.tombstoned {
		if t.Identity() == incarnation.identity && t.ShimID == incarnation.shimID && t.ProcessEpoch == incarnation.processEpoch {
			tombstoned = true
			break
		}
	}
	return quarantined, tombstoned
}

func (d *Daemon) armSessionShimAcceptanceFenceRefusal(id sessionshim.Identity) error {
	if _, _, err := d.sessionShimAcceptanceAdoptedCorrelation(id); err != nil {
		return err
	}
	d.shims.mu.Lock()
	defer d.shims.mu.Unlock()
	d.shims.acceptanceRefusals[id] = acceptanceRefusalArmed
	return nil
}

func (d *Daemon) sessionShimAcceptanceAdoptedCorrelation(id sessionshim.Identity) (string, uint64, error) {
	if d.shims == nil {
		return "", 0, errors.New("session shim adoption is not configured")
	}
	d.shims.mu.RLock()
	entry, ok := d.shims.adopted[id]
	d.shims.mu.RUnlock()
	if !ok {
		return "", 0, fmt.Errorf("session shim: %s is not adopted by this daemon", id)
	}
	if entry.controller != nil {
		hello := entry.controller.Hello()
		return hello.ShimID, hello.ProcessEpoch, nil
	}
	if entry.shimID == "" {
		return "", 0, errors.New("adopted session has no shim correlation")
	}
	return entry.shimID, 0, nil
}

func (d *Daemon) clearSessionShimAcceptanceFenceRefusal(id sessionshim.Identity) error {
	d.shims.mu.Lock()
	defer d.shims.mu.Unlock()
	state, exists := d.shims.acceptanceRefusals[id]
	if !exists {
		return nil
	}
	if state != acceptanceRefusalObserved {
		return errors.New("fence refusal was not observed")
	}
	delete(d.shims.acceptanceRefusals, id)
	return nil
}

func (d *Daemon) consumeSessionShimAcceptanceFenceRefusal(preparation *restartPreparation) error {
	if preparation == nil {
		return nil
	}
	d.shims.mu.Lock()
	defer d.shims.mu.Unlock()
	for _, orgID := range preparation.scopeIDs {
		for _, covered := range preparation.covered[orgID] {
			id := sessionshim.Identity{OrgID: covered.OrgID, SessionID: covered.SessionID}
			if d.shims.acceptanceRefusals[id] == acceptanceRefusalArmed {
				d.shims.acceptanceRefusals[id] = acceptanceRefusalObserved
				return errSessionShimAcceptanceFenceRefused
			}
		}
	}
	return nil
}

func (d *Daemon) cleanupSessionShimAcceptanceControl(request sessionShimAcceptanceRequest) error {
	if request.OrgID != "" || request.SessionID != "" {
		id := request.identity()
		d.shims.mu.Lock()
		delete(d.shims.acceptanceRefusals, id)
		d.shims.mu.Unlock()
		if request.ShimID != "" {
			return d.clearSessionShimAcceptanceQuarantine(request.incarnation())
		}
		return nil
	}
	// Empty cleanup is intentionally conservative: it clears only one-shot
	// refusal state. Quarantine removal still requires an exact incarnation and
	// positive process/record absence proof.
	d.shims.mu.Lock()
	clear(d.shims.acceptanceRefusals)
	d.shims.mu.Unlock()
	return nil
}
