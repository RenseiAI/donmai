package attachclient

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/RenseiAI/donmai/attachwire"
	attachwirev2 "github.com/RenseiAI/donmai/attachwire/v2"
	"github.com/coder/websocket"
)

// V2HostConfig configures one generic native WSS v2 candidate. This is a
// deliberately small carrier client for composing daemons; it owns no PTY or
// durable journal and never treats a socket write as durable success.
type V2HostConfig struct {
	AttachURL   string
	TokenSource TokenSource
	HTTPClient  *http.Client
	Logger      *slog.Logger
	DialTimeout time.Duration
	ReadLimit   int64
	Now         func() time.Time

	// DurableHighWater is the carrier journal cursor loaded before this leg is
	// admitted. It seeds gap validation and ordinary contiguous sends; zero is
	// the safe over-replay default.
	DurableHighWater uint64

	// ResumeDisposition is explicit caller-retained evidence for reconnecting the
	// same authenticated carrier epoch after a client/relay process restart.
	// Nil is the normal fresh-candidate posture. A non-nil value never weakens the
	// fresh path: it is validated exactly and suppresses a duplicate Snapshot.
	ResumeDisposition *V2ResumeDisposition

	// Authority callbacks are invoked only after carrier_active. Before that,
	// Input/Resize/Kill are refused locally even if a non-conforming relay sends
	// them. SnapshotRequest is separate because exactly one mandatory resync is
	// permitted while the leg is a candidate.
	OnInput           func(context.Context, attachwire.InputPayload) error
	OnResize          func(context.Context, attachwire.ResizePayload) error
	OnKill            func(context.Context, attachwire.Kill) error
	OnSnapshotRequest func(context.Context, attachwire.SnapshotRequest) error
}

// V2ResumeState is the closed durable carrier-reload posture.
type V2ResumeState string

const (
	// V2ResumeReceiptStored resumes a pre-active candidate whose exact mandatory
	// Snapshot and frozen receipt already exist durably at the relay.
	V2ResumeReceiptStored V2ResumeState = "receipt_stored"
	// V2ResumeActive resumes an already-active equal carrier at journal high-water.
	V2ResumeActive V2ResumeState = "active"
)

// V2ResumeAuthority identifies why an already-retained carrier may reconnect.
// It is explicit so a proof-v1 token can never drift into a fresh admission and
// a changed controller can never equal-active rebind an old carrier.
type V2ResumeAuthority string

const (
	// V2ResumeSameHandoff is exact replay/drain of the original controller
	// handoff. It is the only authority accepted with the frozen proof-v1 claim
	// profile.
	V2ResumeSameHandoff V2ResumeAuthority = "same_handoff"
	// V2ResumeAdoptedCandidateRecovery rehydrates a proof-v2 candidate whose
	// proof and Snapshot receipt were already consumed by adoption. It may cross
	// arbitrary controller generations but reuses the exact original bearer,
	// jti, candidate epoch, staged Snapshot, and cursor.
	V2ResumeAdoptedCandidateRecovery V2ResumeAuthority = "adopted_candidate_recovery"
)

// V2ResumeDisposition carries only the exact non-secret evidence needed to
// reconnect the already-prepared equal carrier inside the current composing
// daemon without generating another mandatory Snapshot. Authority says whether
// this is exact same-handoff replay/drain or server-resolved recovery after the
// original proof/receipt adoption consume. An already-active carrier under a
// changed controller never uses this type: it reserves a strictly higher
// proof-v2 candidate and follows the full mandatory-Snapshot pipeline.
//
// For receipt_stored, AckSeq is the pre-staged daemon/shim cursor while
// CandidateSnapshotSeq and CandidateSnapshot identify the already-journaled
// raw Snapshot. GapFromSeq/GapToSeq/GapReason are either all zero or the exact
// proof-bound controller_unforwarded N+1..K transition immediately before that
// Snapshot.
// The Relay-private request UUID is deliberately absent because the ratified
// host wire never exposes it; Relay binds that correlation internally. PTYEpoch
// and CarrierEpoch must exactly equal the authenticated token claims in both
// states. For active, candidate-specific fields are absent.
type V2ResumeDisposition struct {
	ProofSchemaVersion   V2ProofSchemaVersion
	Authority            V2ResumeAuthority
	State                V2ResumeState
	PTYEpoch             uint64
	CarrierEpoch         uint64
	AckSeq               uint64
	CandidateSnapshotSeq uint64
	CandidateSnapshot    []byte
	GapFromSeq           uint64
	GapToSeq             uint64
	GapReason            attachwirev2.GapReason
}

func (c *V2HostConfig) withDefaults() error {
	if c.AttachURL == "" || !strings.Contains(c.AttachURL, "/v2/") {
		return errors.New("attachclient: v2 AttachURL with /v2/ path is required")
	}
	if c.TokenSource == nil {
		return errors.New("attachclient: v2 TokenSource is required")
	}
	if c.HTTPClient == nil {
		c.HTTPClient = http.DefaultClient
	}
	if c.Logger == nil {
		c.Logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	if c.DialTimeout <= 0 {
		c.DialTimeout = defaultDialTimeout
	}
	if c.ReadLimit <= 0 {
		c.ReadLimit = defaultReadLimitBytes
	}
	if c.Now == nil {
		c.Now = time.Now
	}
	if c.ResumeDisposition != nil {
		resume := cloneV2ResumeDisposition(*c.ResumeDisposition)
		var err error
		resume, err = normalizeV2ResumeDisposition(resume)
		if err != nil {
			return err
		}
		if err := validateV2ResumeDisposition(resume); err != nil {
			return err
		}
		if c.DurableHighWater != 0 && c.DurableHighWater != resume.AckSeq {
			return errors.New("attachclient: v2 resume acknowledged cursor conflicts with DurableHighWater")
		}
		c.DurableHighWater = resume.AckSeq
		c.ResumeDisposition = &resume
	}
	return nil
}

func cloneV2ResumeDisposition(in V2ResumeDisposition) V2ResumeDisposition {
	in.CandidateSnapshot = append([]byte(nil), in.CandidateSnapshot...)
	return in
}

// Format prevents the retained raw Snapshot from reaching logs while keeping
// the bounded non-secret resume comparison fields visible.
func (r V2ResumeDisposition) Format(state fmt.State, _ rune) {
	_, _ = fmt.Fprintf(state,
		"{proofSchema:%s authority:%s state:%s ptyEpoch:%d carrierEpoch:%d ackSeq:%d candidateSnapshotSeq:%d gap:%d..%d/%s <redacted-snapshot>}",
		r.ProofSchemaVersion, r.Authority, r.State, r.PTYEpoch, r.CarrierEpoch, r.AckSeq,
		r.CandidateSnapshotSeq, r.GapFromSeq, r.GapToSeq, r.GapReason)
}

// Validate checks the exact closed resume shape without dialing or exposing
// credential material. Composing recovery seams use it before retaining a
// disposition for later DialV2HostCandidate.
func (r V2ResumeDisposition) Validate() error {
	normalized, err := normalizeV2ResumeDisposition(r)
	if err != nil {
		return err
	}
	return validateV2ResumeDisposition(normalized)
}

func normalizeV2ResumeDisposition(resume V2ResumeDisposition) (V2ResumeDisposition, error) {
	if resume.ProofSchemaVersion == "" && resume.Authority == "" {
		resume.ProofSchemaVersion = V2ProofSchemaV1
		resume.Authority = V2ResumeSameHandoff
		return resume, nil
	}
	if resume.ProofSchemaVersion == "" || resume.Authority == "" {
		return V2ResumeDisposition{}, errors.New("attachclient: v2 resume proof schema and authority must be supplied together")
	}
	return resume, nil
}

func validateV2ResumeDisposition(resume V2ResumeDisposition) error {
	if resume.CarrierEpoch == 0 {
		return errors.New("attachclient: v2 resume carrier epoch is required")
	}
	if resume.ProofSchemaVersion != V2ProofSchemaV1 && resume.ProofSchemaVersion != V2ProofSchemaV2 {
		return errors.New("attachclient: v2 resume proof schema version is required")
	}
	if resume.Authority != V2ResumeSameHandoff && resume.Authority != V2ResumeAdoptedCandidateRecovery {
		return errors.New("attachclient: v2 resume authority is required")
	}
	if resume.ProofSchemaVersion == V2ProofSchemaV1 && resume.Authority != V2ResumeSameHandoff {
		return errors.New("attachclient: retained proof-v1 is exact same-handoff replay/drain only")
	}
	if resume.Authority == V2ResumeAdoptedCandidateRecovery && resume.State != V2ResumeReceiptStored {
		return errors.New("attachclient: adopted-candidate recovery requires receipt-stored state")
	}
	switch resume.State {
	case V2ResumeActive:
		if resume.AckSeq == 0 || resume.CandidateSnapshotSeq != 0 || len(resume.CandidateSnapshot) != 0 ||
			resume.GapFromSeq != 0 || resume.GapToSeq != 0 || resume.GapReason != "" {
			return errors.New("attachclient: active v2 resume disposition is not exact")
		}
		return nil
	case V2ResumeReceiptStored:
		if resume.CandidateSnapshotSeq <= resume.AckSeq || len(resume.CandidateSnapshot) == 0 {
			return errors.New("attachclient: receipt-stored v2 resume disposition is incomplete")
		}
		frame, err := attachwire.DecodeFrame(resume.CandidateSnapshot)
		if err != nil || !bytes.Equal(frame.Encode(), resume.CandidateSnapshot) ||
			frame.Type != attachwire.TypeSnapshot || frame.Seq != resume.CandidateSnapshotSeq {
			return errors.New("attachclient: receipt-stored v2 resume Snapshot is not exact")
		}
		envelope, err := attachwire.DecodeSnapshotEnvelope(frame.Payload)
		if err != nil || envelope.AtSeq != frame.Seq-1 {
			return errors.New("attachclient: receipt-stored v2 resume Snapshot correlation is not exact")
		}
		gapAbsent := resume.GapFromSeq == 0 && resume.GapToSeq == 0 && resume.GapReason == ""
		gapExact := resume.GapFromSeq == resume.AckSeq+1 &&
			resume.GapToSeq == resume.CandidateSnapshotSeq-1 &&
			resume.GapReason == attachwirev2.GapControllerUnforwarded
		if !gapAbsent && !gapExact {
			return errors.New("attachclient: receipt-stored v2 resume gap is not exact")
		}
		return nil
	default:
		return errors.New("attachclient: unknown v2 resume disposition")
	}
}

// V2HostCandidate is one authenticated exact v2 leg. Its methods are safe for
// concurrent activation, inbound control, and durable event callbacks.
type V2HostCandidate struct {
	cfg    V2HostConfig
	claims v2HostClaims
	conn   *websocket.Conn

	writeMu   sync.Mutex
	durableMu sync.Mutex
	mu        sync.Mutex
	closed    bool
	err       error
	// remoteActive is the exact carrier_active evidence. active is the separate
	// composing-daemon local-publication release and gates authority callbacks.
	remoteActive bool
	active       bool
	// One shared activation flight makes concurrent callers observe one wire
	// request and one immutable completion. The first local call is also the
	// publication release; remote evidence alone never opens authority callbacks.
	activationStarted bool
	activationDone    chan struct{}
	activationAck     uint64
	activationErr     error
	localActiveCh     chan struct{}
	localActiveOnce   sync.Once
	ackSeq            uint64
	ackVersion        uint64
	// highestSent is the highest sequence-bearing raw frame written on this leg.
	highestSent         uint64
	gapTo               uint64
	gapReason           attachwirev2.GapReason
	gapPending          bool
	candidateSent       bool
	snapshotRequestSeen bool
	pendingSeq          uint64
	pendingRaw          []byte
	resumeDisposition   *V2ResumeDisposition
	notify              chan struct{}
	closedCh            chan struct{}

	snapshotRequests chan attachwire.SnapshotRequest
	cancel           context.CancelFunc
}

// DialV2HostCandidate authenticates and subscribes one non-active candidate.
// It requires an exact v2 token and exact subprotocol echo; it never falls back
// to v1 or the degraded carrier.
func DialV2HostCandidate(ctx context.Context, cfg V2HostConfig) (*V2HostCandidate, error) {
	if err := cfg.withDefaults(); err != nil {
		return nil, err
	}
	token, err := cfg.TokenSource(ctx)
	if err != nil {
		return nil, fmt.Errorf("attachclient: v2 token source: %w", err)
	}
	claims, err := parseV2HostClaims(token, cfg.Now())
	if err != nil {
		return nil, err
	}
	if err := validateV2ProofDisposition(claims, cfg); err != nil {
		return nil, err
	}
	if cfg.ResumeDisposition != nil &&
		(cfg.ResumeDisposition.PTYEpoch != claims.Epoch || cfg.ResumeDisposition.CarrierEpoch != claims.CarrierEpoch) {
		return nil, errors.New("attachclient: v2 resume disposition does not match the authenticated carrier")
	}
	dialCtx, cancelDial := context.WithTimeout(ctx, cfg.DialTimeout)
	conn, _, err := websocket.Dial(dialCtx, cfg.AttachURL, &websocket.DialOptions{
		HTTPClient:   cfg.HTTPClient,
		HTTPHeader:   http.Header{"Authorization": {"Bearer " + token}},
		Subprotocols: []string{attachwirev2.SubprotocolVersion},
	})
	cancelDial()
	if err != nil {
		return nil, fmt.Errorf("attachclient: v2 wss dial: %w", err)
	}
	if conn.Subprotocol() != attachwirev2.SubprotocolVersion {
		got := conn.Subprotocol()
		_ = conn.Close(websocket.StatusProtocolError, "v2 subprotocol not negotiated")
		return nil, fmt.Errorf("attachclient: v2 subprotocol %q not echoed (got %q)", attachwirev2.SubprotocolVersion, got)
	}
	conn.SetReadLimit(cfg.ReadLimit)
	legCtx, cancel := context.WithCancel(context.Background())
	candidate := &V2HostCandidate{
		cfg: cfg, claims: claims, conn: conn, notify: make(chan struct{}),
		closedCh: make(chan struct{}), ackSeq: cfg.DurableHighWater, highestSent: cfg.DurableHighWater,
		snapshotRequests: make(chan attachwire.SnapshotRequest, 1), localActiveCh: make(chan struct{}), cancel: cancel,
	}
	if cfg.ResumeDisposition != nil {
		resume := cloneV2ResumeDisposition(*cfg.ResumeDisposition)
		candidate.resumeDisposition = &resume
		switch resume.State {
		case V2ResumeReceiptStored:
			candidate.candidateSent = true
			candidate.snapshotRequestSeen = true
			candidate.highestSent = resume.CandidateSnapshotSeq
			candidate.pendingSeq = resume.CandidateSnapshotSeq
			candidate.pendingRaw = append([]byte(nil), resume.CandidateSnapshot...)
		case V2ResumeActive:
			candidate.highestSent = resume.AckSeq
		}
	}
	subscribe, err := attachwirev2.BuildControlFrame(attachwire.Subscribe{
		SessionID: claims.SessionID, AsRole: attachwire.RoleHost,
		Epoch: int64Pointer(claims.Epoch), ResumeFrom: nil,
	})
	if err != nil {
		_ = candidate.Close()
		return nil, err
	}
	if err := candidate.writeRaw(ctx, subscribe.Encode()); err != nil {
		_ = candidate.Close()
		return nil, fmt.Errorf("attachclient: v2 subscribe: %w", err)
	}
	go candidate.readLoop(legCtx)
	return candidate, nil
}

func validateV2ProofDisposition(claims v2HostClaims, cfg V2HostConfig) error {
	if cfg.ResumeDisposition == nil {
		if claims.ProofSchemaVersion != V2ProofSchemaV2 {
			return errors.New("attachclient: fresh v2 candidate requires proof schema v2")
		}
		if cfg.DurableHighWater != claims.CarrierBoundary {
			return errors.New("attachclient: v2 durable high-water does not match signed carrier boundary")
		}
		return nil
	}
	resume := *cfg.ResumeDisposition
	if resume.ProofSchemaVersion != claims.ProofSchemaVersion {
		return errors.New("attachclient: v2 resume proof schema does not match the original bearer")
	}
	switch resume.State {
	case V2ResumeReceiptStored:
		if resume.AckSeq != claims.CarrierBoundary ||
			resume.CandidateSnapshotSeq != claims.ResolvedBoundary+1 {
			return errors.New("attachclient: receipt-stored resume does not match signed proof boundaries")
		}
		if claims.ResolvedBoundary == claims.CarrierBoundary {
			if resume.GapFromSeq != 0 || resume.GapToSeq != 0 || resume.GapReason != "" {
				return errors.New("attachclient: receipt-stored resume invented a proof gap")
			}
		} else if resume.GapFromSeq != claims.CarrierBoundary+1 ||
			resume.GapToSeq != claims.ResolvedBoundary ||
			resume.GapReason != attachwirev2.GapControllerUnforwarded {
			return errors.New("attachclient: receipt-stored resume proof gap does not match signed boundaries")
		}
	case V2ResumeActive:
		if resume.AckSeq < claims.ResolvedBoundary+1 {
			return errors.New("attachclient: active resume regresses the activated proof transition")
		}
	}
	return nil
}

func int64Pointer(value uint64) *int64 {
	if value > uint64(^uint64(0)>>1) {
		return nil
	}
	converted := int64(value) //nolint:gosec // range checked above
	return &converted
}

// WaitMandatorySnapshotRequest returns the one pre-active resync request. A
// second request before activation is a terminal protocol error.
func (c *V2HostCandidate) WaitMandatorySnapshotRequest(ctx context.Context) (attachwire.SnapshotRequest, error) {
	c.mu.Lock()
	resuming := c.resumeDisposition != nil
	c.mu.Unlock()
	if resuming {
		return attachwire.SnapshotRequest{}, errors.New("attachclient: resumed v2 carrier must not request another mandatory Snapshot")
	}
	select {
	case request := <-c.snapshotRequests:
		if request.Reason != attachwire.ReasonResync {
			return attachwire.SnapshotRequest{}, errors.New("attachclient: v2 candidate snapshot request is not resync")
		}
		return request, nil
	case <-ctx.Done():
		return attachwire.SnapshotRequest{}, ctx.Err()
	case <-c.done():
		return attachwire.SnapshotRequest{}, c.terminalError()
	}
}

// SendCandidateSnapshot writes the exact mandatory sequence-bearing Snapshot
// while the leg is still non-active. It deliberately does not wait for host_ack:
// the strict stored receipt drives adoption publication and carrier_active later
// resolves this pending cursor.
func (c *V2HostCandidate) SendCandidateSnapshot(ctx context.Context, raw []byte) error {
	c.durableMu.Lock()
	defer c.durableMu.Unlock()
	frame, err := attachwire.DecodeFrame(raw)
	if err != nil || frame.Type != attachwire.TypeSnapshot || frame.Seq == 0 {
		return errors.New("attachclient: v2 candidate requires an exact sequence-bearing Snapshot frame")
	}
	envelope, err := attachwire.DecodeSnapshotEnvelope(frame.Payload)
	if err != nil || frame.Seq != c.claims.ResolvedBoundary+1 || envelope.AtSeq != c.claims.ResolvedBoundary {
		return errors.New("attachclient: v2 candidate Snapshot does not match signed resolved boundary")
	}
	c.mu.Lock()
	if c.closed || c.resumeDisposition != nil || c.active || c.candidateSent || !c.snapshotRequestSeen {
		c.mu.Unlock()
		return errors.New("attachclient: v2 candidate Snapshot is late, duplicate, or unavailable")
	}
	if c.claims.ResolvedBoundary > c.claims.CarrierBoundary &&
		(!c.gapPending || c.gapReason != attachwirev2.GapControllerUnforwarded) {
		c.mu.Unlock()
		return errors.New("attachclient: v2 candidate omitted the proof-bound controller gap")
	}
	if c.claims.ResolvedBoundary == c.claims.CarrierBoundary && c.gapPending {
		c.mu.Unlock()
		return errors.New("attachclient: v2 candidate invented a proof-bound gap")
	}
	if c.gapPending {
		if frame.Seq != c.gapTo+1 {
			c.mu.Unlock()
			return errors.New("attachclient: v2 candidate gap is not followed by its authoritative Snapshot")
		}
	} else if frame.Seq != c.highestSent+1 {
		c.mu.Unlock()
		return errors.New("attachclient: v2 candidate Snapshot is not contiguous with the durable cursor")
	}
	if err := c.writeRaw(ctx, raw); err != nil {
		c.mu.Unlock()
		return fmt.Errorf("attachclient: write v2 candidate Snapshot: %w", err)
	}
	c.highestSent = frame.Seq
	c.candidateSent = true
	c.gapPending = false
	c.gapReason = ""
	c.pendingSeq = frame.Seq
	c.pendingRaw = append([]byte(nil), raw...)
	c.mu.Unlock()
	return nil
}

// DeclareHostGap sends the exact replay-gap disposition. The following durable
// send must be the authoritative Snapshot at toSeq+1.
func (c *V2HostCandidate) DeclareHostGap(ctx context.Context, fromSeq, toSeq uint64) error {
	return c.DeclareHostGapWithReason(ctx, fromSeq, toSeq, attachwirev2.GapRingEvicted)
}

// DeclareHostGapWithReason sends one exact gap from the closed v2 reason set.
// controller_unforwarded is accepted only for the signed proof-bound N+1..K
// transition; ordinary callers retain DeclareHostGap's ring_evicted default.
func (c *V2HostCandidate) DeclareHostGapWithReason(
	ctx context.Context,
	fromSeq, toSeq uint64,
	reason attachwirev2.GapReason,
) error {
	c.durableMu.Lock()
	defer c.durableMu.Unlock()
	if fromSeq == 0 || toSeq < fromSeq ||
		(reason != attachwirev2.GapRingEvicted && reason != attachwirev2.GapControllerUnforwarded) {
		return errors.New("attachclient: invalid v2 host gap")
	}
	c.mu.Lock()
	if c.closed || c.resumeDisposition != nil || c.pendingSeq != 0 || c.gapPending || fromSeq != c.ackSeq+1 {
		c.mu.Unlock()
		return errors.New("attachclient: v2 host gap does not begin at the contiguous durable cursor")
	}
	if !c.active {
		if reason != attachwirev2.GapControllerUnforwarded || !c.snapshotRequestSeen ||
			fromSeq != c.claims.CarrierBoundary+1 || toSeq != c.claims.ResolvedBoundary ||
			c.claims.ResolvedBoundary <= c.claims.CarrierBoundary {
			c.mu.Unlock()
			return errors.New("attachclient: pre-active gap does not match signed controller_unforwarded proof boundaries")
		}
	} else if reason == attachwirev2.GapControllerUnforwarded {
		c.mu.Unlock()
		return errors.New("attachclient: controller_unforwarded gap is not legal after activation")
	}
	control, err := attachwirev2.BuildControlFrame(attachwirev2.HostGap{
		FromSeq: attachwirev2.DecimalUint64(fromSeq), ToSeq: attachwirev2.DecimalUint64(toSeq),
		Reason: reason,
	})
	if err != nil {
		c.mu.Unlock()
		return err
	}
	if err := c.writeRaw(ctx, control.Encode()); err != nil {
		c.mu.Unlock()
		return err
	}
	c.gapPending = true
	c.gapTo = toSeq
	c.gapReason = reason
	c.mu.Unlock()
	return nil
}

// Activate sends carrier_activate after Donmai's local publication and waits
// for the exact carrier_active acknowledgement. The returned cursor also
// resolves the pre-active Snapshot when it covers that sequence.
func (c *V2HostCandidate) Activate(ctx context.Context) (uint64, error) {
	c.mu.Lock()
	resumeActive := c.resumeDisposition != nil && c.resumeDisposition.State == V2ResumeActive
	if c.active {
		ack := c.ackSeq
		c.mu.Unlock()
		return ack, nil
	}
	if c.closed || (!resumeActive && !c.candidateSent) {
		c.mu.Unlock()
		return 0, errors.New("attachclient: carrier_activate requires the mandatory candidate Snapshot")
	}
	first := !c.activationStarted
	if first {
		c.activationStarted = true
		c.activationDone = make(chan struct{})
		if c.remoteActive {
			c.completeActivationLocked(nil)
		}
	}
	done := c.activationDone
	c.mu.Unlock()
	if first && !resumeActive {
		control, err := attachwirev2.BuildControlFrame(attachwirev2.CarrierActivate{
			PTYEpoch:     attachwirev2.DecimalUint64(c.claims.Epoch),
			CarrierEpoch: attachwirev2.DecimalUint64(c.claims.CarrierEpoch),
		})
		if err != nil {
			c.completeActivation(err)
			return 0, err
		}
		if err := c.writeRaw(ctx, control.Encode()); err != nil {
			activationErr := fmt.Errorf("attachclient: carrier_activate: %w", err)
			c.completeActivation(activationErr)
			return 0, activationErr
		}
	}
	select {
	case <-done:
		c.mu.Lock()
		ack, err := c.activationAck, c.activationErr
		c.mu.Unlock()
		return ack, err
	case <-ctx.Done():
		return 0, ctx.Err()
	case <-c.done():
		return 0, c.terminalError()
	}
}

func (c *V2HostCandidate) completeActivation(err error) {
	c.mu.Lock()
	c.completeActivationLocked(err)
	c.mu.Unlock()
}

func (c *V2HostCandidate) completeActivationLocked(err error) {
	if !c.activationStarted || c.activationErr != nil || c.active {
		return
	}
	if err != nil {
		c.activationErr = err
		close(c.activationDone)
		return
	}
	if !c.remoteActive {
		return
	}
	c.active = true
	c.activationAck = c.ackSeq
	c.localActiveOnce.Do(func() { close(c.localActiveCh) })
	close(c.activationDone)
}

// SendRawFrameDurable writes exact source bytes and returns nil only after the
// current exact leg reports a contiguous host_ack covering the sequence. It is
// the generic client method a composing OnSessionEventDurable callback targets.
func (c *V2HostCandidate) SendRawFrameDurable(ctx context.Context, raw []byte) error {
	c.durableMu.Lock()
	defer c.durableMu.Unlock()
	frame, err := attachwire.DecodeFrame(raw)
	if err != nil || frame.Seq == 0 || frame.Type == attachwire.TypeInput || frame.Type == attachwire.TypeControl {
		return errors.New("attachclient: v2 durable send requires a sequence-bearing host frame")
	}
	c.mu.Lock()
	if c.closed || !c.active {
		c.mu.Unlock()
		return errors.New("attachclient: v2 carrier is not active")
	}
	retryPending := c.pendingSeq != 0
	switch {
	case retryPending:
		if frame.Seq != c.pendingSeq || !bytes.Equal(raw, c.pendingRaw) {
			c.mu.Unlock()
			return errors.New("attachclient: v2 pending host frame replay changed sequence or raw bytes")
		}
	case c.gapPending:
		if frame.Type != attachwire.TypeSnapshot || frame.Seq != c.gapTo+1 {
			c.mu.Unlock()
			return errors.New("attachclient: v2 host gap must be followed by its exact authoritative Snapshot")
		}
	case frame.Seq > c.ackSeq && frame.Seq != c.highestSent+1:
		c.mu.Unlock()
		return errors.New("attachclient: v2 host frame is not contiguous with the sent stream")
	}
	ackVersion := c.ackVersion
	if err := c.writeRaw(ctx, raw); err != nil {
		c.mu.Unlock()
		return fmt.Errorf("attachclient: write v2 durable host frame: %w", err)
	}
	if !retryPending {
		if frame.Seq > c.highestSent {
			c.highestSent = frame.Seq
		}
		c.pendingSeq = frame.Seq
		c.pendingRaw = append([]byte(nil), raw...)
		c.gapPending = false
		c.gapReason = ""
	}
	c.mu.Unlock()
	return c.waitForAck(ctx, frame.Seq, ackVersion)
}

// OnSessionEventDurable is a named alias for SendRawFrameDurable so composing
// adapters can expose its durable boundary without restating the semantics.
func (c *V2HostCandidate) OnSessionEventDurable(ctx context.Context, raw []byte) error {
	return c.SendRawFrameDurable(ctx, raw)
}

func (c *V2HostCandidate) waitForAck(ctx context.Context, sequence, afterVersion uint64) error {
	for {
		c.mu.Lock()
		if c.ackSeq >= sequence && c.ackVersion > afterVersion {
			c.mu.Unlock()
			return nil
		}
		if c.closed {
			err := c.err
			c.mu.Unlock()
			return terminalV2Error(err)
		}
		notify := c.notify
		c.mu.Unlock()
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-notify:
		}
	}
}

func (c *V2HostCandidate) readLoop(ctx context.Context) {
	for {
		kind, raw, err := c.conn.Read(ctx)
		if err != nil {
			c.fail(err)
			return
		}
		if kind != websocket.MessageBinary {
			c.fail(errors.New("attachclient: v2 carrier received a non-binary message"))
			return
		}
		frame, err := attachwire.DecodeFrame(raw)
		if err != nil {
			c.fail(fmt.Errorf("attachclient: v2 inbound frame: %w", err))
			return
		}
		if err := c.handleV2Inbound(ctx, frame); err != nil {
			c.fail(err)
			return
		}
	}
}

func (c *V2HostCandidate) handleV2Inbound(ctx context.Context, frame attachwire.Frame) error {
	if frame.Type == attachwire.TypeControl {
		payload, err := attachwire.DecodeControlPayload(frame.Payload)
		if err != nil {
			return err
		}
		message, err := attachwirev2.DecodeControl(payload)
		if err != nil {
			return err
		}
		switch typed := message.(type) {
		case attachwire.SnapshotRequest:
			c.mu.Lock()
			active := c.active
			resumeActive := c.resumeDisposition != nil && c.resumeDisposition.State == V2ResumeActive
			waitForPublication := !active && c.remoteActive && (c.activationStarted || resumeActive)
			c.mu.Unlock()
			if waitForPublication {
				if err := c.waitForLocalAuthority(ctx); err != nil {
					return err
				}
				active = true
			}
			c.mu.Lock()
			if !active {
				if c.resumeDisposition != nil {
					c.mu.Unlock()
					return errors.New("attachclient: resumed v2 carrier received a duplicate mandatory Snapshot request")
				}
				if c.snapshotRequestSeen {
					c.mu.Unlock()
					return errors.New("attachclient: v2 candidate received more than one mandatory Snapshot request")
				}
				c.snapshotRequestSeen = true
			}
			c.mu.Unlock()
			if !active {
				select {
				case c.snapshotRequests <- typed:
					return nil
				default:
					return errors.New("attachclient: v2 candidate Snapshot request queue is unavailable")
				}
			}
			if c.cfg.OnSnapshotRequest != nil {
				return c.cfg.OnSnapshotRequest(ctx, typed)
			}
			return nil
		case attachwirev2.CarrierActive:
			return c.acceptCarrierActive(typed)
		case attachwirev2.HostAck:
			return c.acceptHostAck(typed)
		case attachwire.Kill:
			if err := c.waitForLocalAuthority(ctx); err != nil {
				return err
			}
			if c.cfg.OnKill != nil {
				return c.cfg.OnKill(ctx, typed)
			}
			return nil
		case attachwire.ControlError:
			return &RelayStopError{Code: typed.Code, Message: typed.Message}
		default:
			return nil
		}
	}
	if err := c.waitForLocalAuthority(ctx); err != nil {
		return err
	}
	switch frame.Type {
	case attachwire.TypeInput:
		input, err := attachwire.DecodeInput(frame.Payload)
		if err != nil {
			return err
		}
		if c.cfg.OnInput != nil {
			return c.cfg.OnInput(ctx, input)
		}
	case attachwire.TypeResize:
		resize, err := attachwire.DecodeResize(frame.Payload)
		if err != nil {
			return err
		}
		if c.cfg.OnResize != nil {
			return c.cfg.OnResize(ctx, resize)
		}
	}
	return nil
}

func (c *V2HostCandidate) acceptCarrierActive(message attachwirev2.CarrierActive) error {
	if uint64(message.PTYEpoch) != c.claims.Epoch || uint64(message.CarrierEpoch) != c.claims.CarrierEpoch {
		return attachwirev2.ErrControlMismatch
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	ack := uint64(message.AckSeq)
	expectedAck := c.highestSent
	canActivate := c.candidateSent
	allowBeforeLocalRequest := false
	if c.resumeDisposition != nil {
		switch c.resumeDisposition.State {
		case V2ResumeActive:
			canActivate = true
			allowBeforeLocalRequest = true
			expectedAck = c.resumeDisposition.AckSeq
		case V2ResumeReceiptStored:
			expectedAck = c.resumeDisposition.CandidateSnapshotSeq
		}
	}
	if !canActivate || (!allowBeforeLocalRequest && !c.activationStarted) ||
		ack != expectedAck || (c.remoteActive && ack != c.ackSeq) {
		return errors.New("attachclient: carrier_active cursor is outside the exact sent stream")
	}
	c.remoteActive = true
	c.ackSeq = ack
	c.ackVersion++
	if c.pendingSeq != 0 && ack >= c.pendingSeq {
		c.pendingSeq = 0
		c.pendingRaw = nil
	}
	c.completeActivationLocked(nil)
	c.signalLocked()
	return nil
}

func (c *V2HostCandidate) acceptHostAck(message attachwirev2.HostAck) error {
	if uint64(message.PTYEpoch) != c.claims.Epoch || uint64(message.CarrierEpoch) != c.claims.CarrierEpoch {
		return attachwirev2.ErrControlMismatch
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	ack := uint64(message.AckSeq)
	if !c.active || ack < c.ackSeq || ack > c.highestSent {
		return errors.New("attachclient: host_ack is stale, early, or beyond the exact sent stream")
	}
	c.ackSeq = ack
	c.ackVersion++
	if c.pendingSeq != 0 && ack >= c.pendingSeq {
		c.pendingSeq = 0
		c.pendingRaw = nil
	}
	c.signalLocked()
	return nil
}

func (c *V2HostCandidate) waitForLocalAuthority(ctx context.Context) error {
	c.mu.Lock()
	if c.active {
		c.mu.Unlock()
		return nil
	}
	resumeActive := c.resumeDisposition != nil && c.resumeDisposition.State == V2ResumeActive
	allowed := c.remoteActive && (c.activationStarted || resumeActive)
	localActive := c.localActiveCh
	closed := c.closedCh
	c.mu.Unlock()
	if !allowed || localActive == nil {
		return errors.New("attachclient: v2 candidate received authority before local publication")
	}
	select {
	case <-localActive:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	case <-closed:
		return c.terminalError()
	}
}

func (c *V2HostCandidate) writeRaw(ctx context.Context, raw []byte) error {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	return c.conn.Write(ctx, websocket.MessageBinary, raw)
}

func (c *V2HostCandidate) signalLocked() {
	close(c.notify)
	c.notify = make(chan struct{})
}

func (c *V2HostCandidate) fail(err error) {
	c.mu.Lock()
	if !c.closed {
		c.closed = true
		c.err = err
		c.completeActivationLocked(terminalV2Error(err))
		close(c.closedCh)
		c.signalLocked()
	}
	c.mu.Unlock()
	if c.cancel != nil {
		c.cancel()
	}
}

func (c *V2HostCandidate) done() <-chan struct{} {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.closedCh
}

func (c *V2HostCandidate) terminalError() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return terminalV2Error(c.err)
}

func terminalV2Error(err error) error {
	if err == nil {
		return errors.New("attachclient: v2 carrier closed")
	}
	return err
}

// Close releases the candidate connection. It does not imply that a frame was
// durable or that the session ended.
func (c *V2HostCandidate) Close() error {
	if c == nil {
		return nil
	}
	c.fail(errors.New("attachclient: v2 carrier closed"))
	return c.conn.Close(websocket.StatusNormalClosure, "host carrier closed")
}
