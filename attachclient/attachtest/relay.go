package attachtest

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/RenseiAI/donmai/attachwire"
	"github.com/coder/websocket"
)

// Config configures a StubRelay.
type Config struct {
	// Addr is the loopback listen address. Empty → 127.0.0.1:0 (ephemeral port).
	Addr string
	// RoomID names the single room; it must match the AttachURL path segment.
	RoomID string
	// RingSize bounds the seq-keyed ring (frames). Default 256.
	RingSize int
	// RefuseWSS makes the WSS upgrade endpoint return 404 (proxy-stripped Upgrade
	// simulation) so the client falls back to the degraded lane (§ 14). Toggle at
	// runtime with SetRefuseWSS to exercise upgrade-back.
	RefuseWSS bool
	// DropHostPOSTOnce returns 503 to the FIRST host POST that is newly applied,
	// simulating a lost 200 so the client retries the same batchId (§ 14
	// idempotency). The batch is still applied; the retry is a de-duplicated
	// no-op returning the same ack.
	DropHostPOSTOnce bool
	// Logger is optional (nil → discard).
	Logger *slog.Logger
}

// StubRelay is a deliberately-dumb, single-room, mechanism-only relay for
// exercising the attach client end to end. It is TEST INFRASTRUCTURE: it carries
// the wire mechanism (epoch CAS, ring/resume, degraded batch/ack, snapshot
// round-trip) but NONE of the platform's relay policy (arbitration, quotas,
// multi-tenant, backpressure). See doc.go.
type StubRelay struct {
	cfg    Config
	log    *slog.Logger
	room   *room
	server *http.Server
	ln     net.Listener

	refuseWSS   atomic.Bool
	droppedPOST atomic.Bool

	mu      sync.Mutex
	started bool
}

// New builds a StubRelay. Call Start to bind and serve.
func New(cfg Config) *StubRelay {
	if cfg.RoomID == "" {
		cfg.RoomID = "room-1"
	}
	if cfg.RingSize <= 0 {
		cfg.RingSize = 256
	}
	log := cfg.Logger
	if log == nil {
		log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	s := &StubRelay{cfg: cfg, log: log, room: newRoom(cfg.RingSize)}
	s.refuseWSS.Store(cfg.RefuseWSS)
	return s
}

// Start binds the loopback listener and serves in the background.
func (s *StubRelay) Start() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.started {
		return fmt.Errorf("attachtest: relay already started")
	}
	addr := s.cfg.Addr
	if addr == "" {
		addr = "127.0.0.1:0"
	}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("attachtest: listen %q: %w", addr, err)
	}
	s.ln = ln

	mux := http.NewServeMux()
	roomPath := "/" + attachwire.VersionPathSegment + "/rooms/" + s.cfg.RoomID
	mux.HandleFunc(roomPath, s.handleWS)
	mux.HandleFunc(roomPath+"/host/sse", s.handleHostSSE)
	mux.HandleFunc(roomPath+"/host/output", s.handleHostOutput)

	s.server = &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	s.started = true
	go func() { _ = s.server.Serve(ln) }()
	return nil
}

// Addr is the bound "host:port".
func (s *StubRelay) Addr() string {
	if s.ln == nil {
		return ""
	}
	return s.ln.Addr().String()
}

// BaseWSURL is the ws:// attach URL for this room (ends in /v1/rooms/<roomId>).
func (s *StubRelay) BaseWSURL() string {
	return "ws://" + s.Addr() + "/" + attachwire.VersionPathSegment + "/rooms/" + s.cfg.RoomID
}

// SetRefuseWSS toggles WSS-upgrade refusal at runtime (upgrade-back testing).
func (s *StubRelay) SetRefuseWSS(v bool) { s.refuseWSS.Store(v) }

// Close stops the server.
func (s *StubRelay) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.server == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	return s.server.Shutdown(ctx)
}

// ---- test accessors ---------------------------------------------------------

// HostBound reports whether a host leg is currently bound.
func (s *StubRelay) HostBound() bool {
	s.room.mu.Lock()
	defer s.room.mu.Unlock()
	return s.room.hostBound
}

// Head is the highest host seq currently in the ring.
func (s *StubRelay) Head() uint64 {
	s.room.mu.Lock()
	defer s.room.mu.Unlock()
	return s.room.head
}

// Epoch is the current room-generation epoch.
func (s *StubRelay) Epoch() int64 {
	s.room.mu.Lock()
	defer s.room.mu.Unlock()
	return s.room.epoch
}

// RingSeqs returns the host seqs currently buffered, in order — for contiguity /
// no-regression assertions.
func (s *StubRelay) RingSeqs() []uint64 {
	s.room.mu.Lock()
	defer s.room.mu.Unlock()
	out := make([]uint64, len(s.room.ring))
	for i, f := range s.room.ring {
		out[i] = f.Seq
	}
	return out
}

// HostAckSeq is the highest contiguous host seq acked on the degraded lane.
func (s *StubRelay) HostAckSeq() int64 {
	s.room.mu.Lock()
	defer s.room.mu.Unlock()
	return s.room.hostAckSeq
}

// SendToHost injects a relay→host frame directly (bypassing stamping) — used by
// the input-trust test to deliver an UNSTAMPED Input from a hostile relay.
func (s *StubRelay) SendToHost(f attachwire.Frame) { s.room.sendToHost(f) }

// ---- WSS lane ---------------------------------------------------------------

func (s *StubRelay) handleWS(w http.ResponseWriter, r *http.Request) {
	if s.refuseWSS.Load() {
		http.Error(w, "wss upgrade refused", http.StatusNotFound)
		return
	}
	claims, err := parseClaims(bearer(r))
	if err != nil || claims.Aud != "relay" {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{
		Subprotocols:       []string{attachwire.SubprotocolVersion},
		InsecureSkipVerify: true, // loopback tests
	})
	if err != nil {
		return
	}
	if conn.Subprotocol() != attachwire.SubprotocolVersion {
		_ = conn.Close(websocket.StatusPolicyViolation, "version subprotocol required")
		return
	}
	conn.SetReadLimit(64 << 20)

	ctx := r.Context()
	// First frame must be a subscribe control.
	sub, err := readSubscribe(ctx, conn)
	if err != nil {
		_ = conn.Close(websocket.StatusPolicyViolation, "expected subscribe")
		return
	}
	switch sub.AsRole {
	case attachwire.RoleHost:
		if !claims.hasEpoch {
			_ = writeFrame(ctx, conn, errorControlFrame(attachwire.CodeAuth, "host token missing epoch", false))
			_ = conn.Close(websocket.StatusPolicyViolation, "host epoch required")
			return
		}
		s.serveHostWSS(ctx, conn, claims)
	default:
		s.serveViewerWSS(ctx, conn, claims, sub)
	}
}

func (s *StubRelay) serveHostWSS(ctx context.Context, conn *websocket.Conn, claims claims) {
	legCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	out := make(chan attachwire.Frame, 256)
	if s.room.bindHost(claims.Epoch, claims.Jti, out, cancel) == bindStale {
		_ = writeFrame(ctx, conn, errorControlFrame(attachwire.CodeEpochStale, "a newer or equal host leg is bound", false))
		_ = conn.Close(websocket.StatusPolicyViolation, "epoch-stale")
		return
	}
	defer s.room.unbindHost(out)

	// Writer: relay→host frames → the connection.
	go func() {
		for {
			select {
			case <-legCtx.Done():
				return
			case f := <-out:
				if err := writeFrame(legCtx, conn, f); err != nil {
					cancel()
					return
				}
			}
		}
	}()

	// Reader: host frames → the ring.
	for {
		typ, data, err := conn.Read(legCtx)
		if err != nil {
			return
		}
		if typ != websocket.MessageBinary {
			continue
		}
		frame, derr := attachwire.DecodeFrame(data)
		if derr != nil {
			_ = writeFrame(legCtx, conn, errorControlFrame(attachwire.CodeFraming, derr.Error(), false))
			_ = conn.Close(websocket.StatusProtocolError, "framing")
			return
		}
		s.ingestHostFrame(frame)
	}
}

// ingestHostFrame routes one host-produced frame: Control (subscribe/error) is
// handled locally; everything else feeds the ring.
func (s *StubRelay) ingestHostFrame(f attachwire.Frame) {
	if f.Type == attachwire.TypeControl {
		if j, err := attachwire.DecodeControlPayload(f.Payload); err == nil {
			if msg, err := attachwire.DecodeControl(j); err == nil {
				switch msg.(type) {
				case attachwire.Subscribe, attachwire.ControlError:
					return // host subscribe echo / error — nothing to store
				}
			}
		}
		return
	}
	s.room.appendHostFrame(f)
}

func (s *StubRelay) serveViewerWSS(ctx context.Context, conn *websocket.Conn, claims claims, sub attachwire.Subscribe) {
	legCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	connID := claims.Jti
	s.room.join(member{userID: claims.UserID, connID: connID, role: string(sub.AsRole)})
	defer s.room.leave(connID)
	s.room.assignPenIfDriver(connID, claims.UserID, string(sub.AsRole))

	// Join controls (§ 7): room_state, pen_state, presence.
	s.room.mu.Lock()
	sinceSeq := int64(s.room.head) //nolint:gosec // G115: host seq lives in the protocol varint domain
	ended := s.room.ended
	s.room.mu.Unlock()
	state := attachwire.RoomLive
	if ended {
		state = attachwire.RoomEnded
	}
	_ = writeFrame(legCtx, conn, roomStateFrame(state, &sinceSeq))
	hu, hc, hg := s.room.penSnapshot()
	_ = writeFrame(legCtx, conn, penStateFrame(hu, hc, hg))
	_ = writeFrame(legCtx, conn, presenceFrame(attachwire.PresenceJoin, s.membersList()))

	// Inbound: viewer Input / controls.
	go s.viewerInbound(legCtx, conn, connID, claims)

	// Outbound: resume (§ 13) then live tail.
	resumeFrom := int64(0)
	if sub.ResumeFrom != nil {
		resumeFrom = *sub.ResumeFrom
	}
	if err := s.deliverToViewer(legCtx, conn, uint64(resumeFrom)); err != nil {
		_ = conn.Close(websocket.StatusNormalClosure, "bye")
		return
	}
	_ = conn.Close(websocket.StatusNormalClosure, "bye")
}

// deliverToViewer implements § 13: ring hit → replay from seq+1; ring miss →
// snapshot + tail with the atSeq+1 contiguity rule.
func (s *StubRelay) deliverToViewer(ctx context.Context, conn *websocket.Conn, resumeFrom uint64) error {
	cursor := resumeFrom + 1

	// § 13: resumeFrom null ≡ 0 ≡ "no applied history" → ALWAYS snapshot + tail
	// (seq 0 never addresses a buffered frame). A positive resumeFrom hits the
	// ring only when still buffered.
	s.room.mu.Lock()
	hit := resumeFrom > 0 && s.room.ringHasLocked(cursor)
	s.room.mu.Unlock()

	if !hit {
		reason := attachwire.ReasonJoin
		if resumeFrom > 0 {
			reason = attachwire.ReasonRingMiss
		}
		snap, atSeq, err := s.requestSnapshot(ctx, reason)
		if err != nil {
			return err
		}
		if err := writeFrame(ctx, conn, snap); err != nil {
			return err
		}
		cursor = atSeq + 1 // tail starts exactly at atSeq+1 (§ 13)
	}

	for {
		s.room.mu.Lock()
		pending := s.room.framesFromLocked(cursor)
		wait := s.room.waitLocked()
		ended := s.room.ended
		exitSeq := s.room.exitSeq
		s.room.mu.Unlock()

		for _, f := range pending {
			if err := writeFrame(ctx, conn, f); err != nil {
				return err
			}
			cursor = f.Seq + 1
		}
		if ended && cursor > exitSeq {
			return nil // delivered through Exit
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-wait:
		}
	}
}

// requestSnapshot sends snapshot_request to the host and returns the resulting
// Snapshot frame + its atSeq (pre-Exit seq-bearing, or the post-Exit final). It
// re-requests periodically so a host that binds late (e.g. mid-reconnect) still
// answers a waiting late joiner.
func (s *StubRelay) requestSnapshot(ctx context.Context, reason attachwire.SnapshotReason) (attachwire.Frame, uint64, error) {
	s.room.mu.Lock()
	reqHead := s.room.head
	s.room.mu.Unlock()

	s.room.sendToHost(snapshotRequestFrame(reason))
	ticker := time.NewTicker(150 * time.Millisecond)
	defer ticker.Stop()

	for {
		s.room.mu.Lock()
		if s.room.finalSnapshot != nil {
			f := *s.room.finalSnapshot
			s.room.mu.Unlock()
			env, err := attachwire.DecodeSnapshotEnvelope(f.Payload)
			if err != nil {
				return attachwire.Frame{}, 0, fmt.Errorf("attachtest: decoding final snapshot: %w", err)
			}
			return f, env.AtSeq, nil
		}
		if s.room.lastSnapshotSeq > reqHead {
			f, ok := s.room.ringFrameLocked(s.room.lastSnapshotSeq)
			s.room.mu.Unlock()
			if ok {
				if env, err := attachwire.DecodeSnapshotEnvelope(f.Payload); err == nil {
					return f, env.AtSeq, nil
				}
			}
			continue
		}
		wait := s.room.waitLocked()
		s.room.mu.Unlock()
		select {
		case <-ctx.Done():
			return attachwire.Frame{}, 0, ctx.Err()
		case <-wait:
		case <-ticker.C:
			s.room.sendToHost(snapshotRequestFrame(reason))
		}
	}
}

func (s *StubRelay) viewerInbound(ctx context.Context, conn *websocket.Conn, connID string, claims claims) {
	for {
		typ, data, err := conn.Read(ctx)
		if err != nil {
			return
		}
		if typ != websocket.MessageBinary {
			continue
		}
		frame, derr := attachwire.DecodeFrame(data)
		if derr != nil {
			return
		}
		if frame.Type == attachwire.TypeInput {
			in, err := attachwire.DecodeInput(frame.Payload)
			if err != nil {
				continue
			}
			if !s.room.isPenHolder(connID) {
				continue // dropped: not the pen holder (§ 5 admission)
			}
			in.UserID = []byte(claims.UserID) // relay stamps the verified userId
			s.room.sendToHost(attachwire.Frame{Type: attachwire.TypeInput, Payload: in.Encode()})
		}
		// Other viewer controls (resume_from, grab/release) are out of the stub's
		// minimal scope; the tests drive resume via subscribe.resumeFrom.
	}
}

func (s *StubRelay) membersList() []attachwire.PresenceMember {
	s.room.mu.Lock()
	defer s.room.mu.Unlock()
	out := make([]attachwire.PresenceMember, 0, len(s.room.members))
	for _, m := range s.room.members {
		out = append(out, attachwire.PresenceMember{UserID: m.userID, ConnID: m.connID, Role: m.role, Driving: m.driving})
	}
	return out
}

// ---- degraded host lane -----------------------------------------------------

func (s *StubRelay) handleHostSSE(w http.ResponseWriter, r *http.Request) {
	claims, err := parseClaims(bearer(r))
	if err != nil || claims.Aud != "relay" {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	if !claims.hasEpoch {
		http.Error(w, "host epoch required", http.StatusUnauthorized)
		return
	}
	epoch := claims.Epoch
	if q := r.URL.Query().Get("epoch"); q != "" {
		if e, perr := strconv.ParseInt(q, 10, 64); perr == nil {
			epoch = e
		}
	}

	legCtx, cancel := context.WithCancel(r.Context())
	defer cancel()

	out := make(chan attachwire.Frame, 256)
	if s.room.bindHost(epoch, claims.Jti, out, cancel) == bindStale {
		http.Error(w, "epoch-stale", http.StatusConflict)
		return
	}
	defer s.room.unbindHost(out)

	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	heartbeat := time.NewTicker(15 * time.Second)
	defer heartbeat.Stop()
	for {
		select {
		case <-legCtx.Done():
			return
		case <-heartbeat.C:
			if _, err := io.WriteString(w, ": heartbeat\n\n"); err != nil {
				return
			}
			flusher.Flush()
		case f := <-out:
			if _, err := fmt.Fprintf(w, "event: frame\ndata: %s\n\n", attachwire.EncodeFrameBase64(f)); err != nil {
				return
			}
			flusher.Flush()
		}
	}
}

func (s *StubRelay) handleHostOutput(w http.ResponseWriter, r *http.Request) {
	claims, err := parseClaims(bearer(r))
	if err != nil || claims.Aud != "relay" {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 64<<20))
	if err != nil {
		http.Error(w, "bad body", http.StatusBadRequest)
		return
	}
	var batch attachwire.HostFrameBatch
	if err := json.Unmarshal(body, &batch); err != nil {
		http.Error(w, "bad batch", http.StatusBadRequest)
		return
	}

	s.room.mu.Lock()
	if ack, seen := s.room.batchSeen[batch.BatchID]; seen {
		s.room.mu.Unlock()
		writeJSON(w, http.StatusOK, attachwire.HostBatchAccepted{BatchID: batch.BatchID, AckSeq: ack})
		return
	}
	// Contiguity (§ 14): a non-empty frame batch must start at hostAckSeq+1.
	if len(batch.Frames) > 0 && batch.FirstSeq != s.room.hostAckSeq+1 {
		ack := s.room.hostAckSeq
		s.room.mu.Unlock()
		writeJSON(w, http.StatusConflict, attachwire.HostBatchRejected{BatchID: batch.BatchID, AckSeq: ack})
		return
	}
	for _, b64 := range batch.Frames {
		if f, derr := attachwire.DecodeFrameBase64(b64); derr == nil {
			s.room.appendHostFrameLocked(f)
		}
	}
	for _, b64 := range batch.OutOfSeq {
		if f, derr := attachwire.DecodeFrameBase64(b64); derr == nil {
			s.applyOutOfSeqLocked(f)
		}
	}
	if len(batch.Frames) > 0 {
		s.room.hostAckSeq = batch.LastSeq
	}
	ack := s.room.hostAckSeq
	s.room.batchSeen[batch.BatchID] = ack
	s.room.mu.Unlock()

	if s.cfg.DropHostPOSTOnce && s.droppedPOST.CompareAndSwap(false, true) {
		// Simulate a lost 200: applied, but the client sees a failure and retries
		// the same batchId → the dedup branch above returns the same ack.
		http.Error(w, "simulated dropped response", http.StatusServiceUnavailable)
		return
	}
	writeJSON(w, http.StatusOK, attachwire.HostBatchAccepted{BatchID: batch.BatchID, AckSeq: ack})
}

// applyOutOfSeqLocked handles an out-of-namespace host frame from a POST batch:
// a post-Exit Snapshot feeds finalSnapshot; subscribe/error are informational.
func (s *StubRelay) applyOutOfSeqLocked(f attachwire.Frame) {
	if f.Type == attachwire.TypeSnapshot {
		s.room.appendHostFrameLocked(f) // seq==0 → stored as finalSnapshot
		return
	}
	// Control (subscribe echo / error) — nothing to store in the stub.
}

// ---- helpers ----------------------------------------------------------------

func bearer(r *http.Request) string {
	h := r.Header.Get("Authorization")
	if strings.HasPrefix(h, "Bearer ") {
		return strings.TrimPrefix(h, "Bearer ")
	}
	if t := r.URL.Query().Get("access_token"); t != "" {
		return t
	}
	return ""
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeFrame(ctx context.Context, conn *websocket.Conn, f attachwire.Frame) error {
	return conn.Write(ctx, websocket.MessageBinary, f.Encode())
}

func readSubscribe(ctx context.Context, conn *websocket.Conn) (attachwire.Subscribe, error) {
	rctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	typ, data, err := conn.Read(rctx)
	if err != nil {
		return attachwire.Subscribe{}, err
	}
	if typ != websocket.MessageBinary {
		return attachwire.Subscribe{}, fmt.Errorf("attachtest: subscribe must be binary")
	}
	frame, err := attachwire.DecodeFrame(data)
	if err != nil {
		return attachwire.Subscribe{}, err
	}
	if frame.Type != attachwire.TypeControl {
		return attachwire.Subscribe{}, fmt.Errorf("attachtest: first frame is not Control")
	}
	j, err := attachwire.DecodeControlPayload(frame.Payload)
	if err != nil {
		return attachwire.Subscribe{}, err
	}
	msg, err := attachwire.DecodeControl(j)
	if err != nil {
		return attachwire.Subscribe{}, err
	}
	sub, ok := msg.(attachwire.Subscribe)
	if !ok {
		return attachwire.Subscribe{}, fmt.Errorf("attachtest: first control is not subscribe")
	}
	return sub, nil
}

// ---- control-frame builders -------------------------------------------------

func mustControlFrame(m attachwire.ControlMessage) attachwire.Frame {
	f, err := attachwire.BuildControlFrame(m)
	if err != nil {
		return attachwire.NewControlFrame(nil)
	}
	return f
}

func roomStateFrame(state attachwire.RoomStateValue, sinceSeq *int64) attachwire.Frame {
	return mustControlFrame(attachwire.RoomState{State: state, SinceSeq: sinceSeq})
}

func penStateFrame(user, conn string, gen int64) attachwire.Frame {
	var up, cp *string
	if user != "" {
		up = &user
	}
	if conn != "" {
		cp = &conn
	}
	return mustControlFrame(attachwire.PenState{HolderUserID: up, HolderConnID: cp, PenGeneration: gen})
}

func presenceFrame(op attachwire.PresenceOp, members []attachwire.PresenceMember) attachwire.Frame {
	return mustControlFrame(attachwire.Presence{Op: op, Members: members})
}

func snapshotRequestFrame(reason attachwire.SnapshotReason) attachwire.Frame {
	return mustControlFrame(attachwire.SnapshotRequest{Reason: reason})
}

func errorControlFrame(code attachwire.ErrorCode, msg string, retryable bool) attachwire.Frame {
	return mustControlFrame(attachwire.ControlError{Code: code, Message: msg, Retryable: retryable})
}

// ---- unverified claims ------------------------------------------------------

type claims struct {
	SessionID string `json:"sessionId"`
	RoomID    string `json:"roomId"`
	UserID    string `json:"userId"`
	Role      string `json:"role"`
	Epoch     int64  `json:"epoch"`
	OrgID     string `json:"orgId"`
	Aud       string `json:"aud"`
	Jti       string `json:"jti"`
	hasEpoch  bool
}

// parseClaims decodes the JWT payload WITHOUT verifying the signature — the stub
// only checks aud and epoch presence (cheap conformance, not security).
func parseClaims(token string) (claims, error) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return claims{}, fmt.Errorf("attachtest: malformed JWT")
	}
	raw, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(parts[1], "="))
	if err != nil {
		return claims{}, err
	}
	var probe map[string]json.RawMessage
	if err := json.Unmarshal(raw, &probe); err != nil {
		return claims{}, err
	}
	var c claims
	if err := json.Unmarshal(raw, &c); err != nil {
		return claims{}, err
	}
	_, c.hasEpoch = probe["epoch"]
	return c, nil
}
