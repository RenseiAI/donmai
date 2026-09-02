package attachclient

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/RenseiAI/donmai/attachwire"
)

// fakeSession is a scripted, in-memory Session for tests: a monotonic
// seq-keyed ring of host-produced frames with live fan-out, plus recorded
// WriteInput/Resize calls. It stands in for the real PTY host (a separate lane)
// and never blocks the client under lock (each subscription uses a generously
// buffered channel — tests are low-volume).
type fakeSession struct {
	mu     sync.Mutex
	epoch  uint64
	head   uint64
	ring   []attachwire.Frame
	subs   map[*fakeSub]struct{}
	doneCh chan struct{}

	exited      bool
	exitSeq     uint64
	exitPayload attachwire.ExitPayload

	inputs           [][]byte
	attributedInputs []attributedInput
	resizes          []attachwire.ResizePayload

	// evictBelow simulates the real host's own bounded local ring having
	// rotated past this seq (ptyhost's RingBytes eviction, § 13): a Subscribe
	// for a positive fromSeq < evictBelow can no longer be served locally,
	// mirroring the real agent.ErrRingMiss case. Zero (default) never evicts.
	evictBelow uint64

	// subscribeSeqs records every fromSeq argument RunHost has passed to
	// Subscribe, in order — tests use it to prove a §13 ring-miss reset
	// re-attaches with fromSeq 0 (no resume position), not the prior head.
	subscribeSeqs []attachwire.HostSeq
}

type fakeSub struct {
	fs     *fakeSession
	ch     chan attachwire.Frame
	closed bool
}

func newFakeSession(epoch uint64) *fakeSession {
	return &fakeSession{
		epoch:  epoch,
		subs:   make(map[*fakeSub]struct{}),
		doneCh: make(chan struct{}),
	}
}

func (fs *fakeSession) appendLocked(f attachwire.Frame) {
	fs.head++
	f.Seq = fs.head
	f.RelTime = fs.head
	fs.ring = append(fs.ring, f)
	for sub := range fs.subs {
		sub.ch <- f
	}
	if f.Type == attachwire.TypeExit {
		if ep, err := attachwire.DecodeExit(f.Payload); err == nil {
			fs.exitPayload = ep
		}
		fs.exited = true
		fs.exitSeq = f.Seq
		for sub := range fs.subs {
			if !sub.closed {
				close(sub.ch)
				sub.closed = true
			}
		}
		fs.subs = make(map[*fakeSub]struct{})
		close(fs.doneCh)
	}
}

// PushOutput appends an Output frame.
func (fs *fakeSession) PushOutput(data []byte) {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	fs.appendLocked(attachwire.Frame{Type: attachwire.TypeOutput, Payload: attachwire.EncodeOutput(data)})
}

// PushMarker appends a Marker frame.
func (fs *fakeSession) PushMarker(label string) {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	fs.appendLocked(attachwire.Frame{Type: attachwire.TypeMarker, Payload: attachwire.MarkerPayload{Label: label}.Encode()})
}

// PushExit appends the terminal Exit frame and marks the session done.
func (fs *fakeSession) PushExit(code uint64) {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	fs.appendLocked(attachwire.Frame{Type: attachwire.TypeExit, Payload: attachwire.NewNormalExit(code).Encode()})
}

func (fs *fakeSession) Inputs() [][]byte {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	out := make([][]byte, len(fs.inputs))
	copy(out, fs.inputs)
	return out
}

// attributedInput is one recorded WriteAttributedInput call.
type attributedInput struct {
	UserID []byte
	Data   []byte
}

// AttributedInputs returns every WriteAttributedInput call fakeSession has
// observed, in order — tests use it to prove a caller's userID reached the
// session (systemAttributedWriter routing, § 5).
func (fs *fakeSession) AttributedInputs() []attributedInput {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	out := make([]attributedInput, len(fs.attributedInputs))
	copy(out, fs.attributedInputs)
	return out
}

// SubscriberCount reports the number of live subscriptions — used by tests to
// deterministically sequence a push after the client has subscribed.
func (fs *fakeSession) SubscriberCount() int {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	return len(fs.subs)
}

func (fs *fakeSession) Resizes() []attachwire.ResizePayload {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	out := make([]attachwire.ResizePayload, len(fs.resizes))
	copy(out, fs.resizes)
	return out
}

// ---- Session implementation -------------------------------------------------

func (fs *fakeSession) WriteInput(p []byte) (int, error) {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	fs.inputs = append(fs.inputs, append([]byte(nil), p...))
	return len(p), nil
}

// WriteAttributedInput implements the OPTIONAL systemAttributedWriter
// capability (session.go) so tests can prove writeStampedInput actually
// routes a stamped Input's userID through, instead of silently falling back
// to WriteInput. It also appends to the same fs.inputs record WriteInput
// does, so every existing Inputs()-based assertion is unaffected by which of
// the two paths a given test exercises.
func (fs *fakeSession) WriteAttributedInput(userID, p []byte) (int, error) {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	fs.inputs = append(fs.inputs, append([]byte(nil), p...))
	fs.attributedInputs = append(fs.attributedInputs, attributedInput{
		UserID: append([]byte(nil), userID...),
		Data:   append([]byte(nil), p...),
	})
	return len(p), nil
}

func (fs *fakeSession) Resize(cols, rows, pxWidth, pxHeight uint32) error {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	fs.resizes = append(fs.resizes, attachwire.ResizePayload{
		Cols: uint64(cols), Rows: uint64(rows), PxWidth: uint64(pxWidth), PxHeight: uint64(pxHeight),
	})
	return nil
}

func (fs *fakeSession) screenLocked() attachwire.Screen {
	const cols, rows = 8, 1
	cells := make([]attachwire.Cell, cols*rows)
	for i := range cells {
		cells[i] = attachwire.Cell{RuneBytes: []byte(" ")}
	}
	return attachwire.Screen{
		Epoch:        fs.epoch,
		EchoMode:     attachwire.EchoOn,
		Cols:         cols,
		Rows:         rows,
		ActiveBuffer: attachwire.BufferPrimary,
		CursorShape:  attachwire.CursorShapeBlock,
		Primary:      cells,
	}
}

func (fs *fakeSession) Snapshot() (attachwire.Screen, attachwire.HostSeq, error) {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	return fs.screenLocked(), attachwire.HostSeq(fs.head), nil
}

func (fs *fakeSession) EmitSnapshot() (attachwire.Frame, bool, error) {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	snap, err := fs.screenLocked().Encode()
	if err != nil {
		return attachwire.Frame{}, false, err
	}
	if fs.exited {
		env := attachwire.SnapshotEnvelope{AtSeq: fs.exitSeq, SnapFormat: attachwire.SnapFormatScreen, Snap: snap}
		return attachwire.Frame{Type: attachwire.TypeSnapshot, Seq: attachwire.PostExitSnapshotSeq, Payload: env.Encode()}, false, nil
	}
	fs.head++
	env := attachwire.SnapshotEnvelope{AtSeq: fs.head, SnapFormat: attachwire.SnapFormatScreen, Snap: snap}
	f := attachwire.Frame{Type: attachwire.TypeSnapshot, Seq: fs.head, RelTime: fs.head, Payload: env.Encode()}
	fs.ring = append(fs.ring, f)
	for sub := range fs.subs {
		sub.ch <- f
	}
	return f, true, nil
}

func (fs *fakeSession) EmitMarker(label string) error {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	if fs.exited {
		return errors.New("fakeSession: exited")
	}
	fs.appendLocked(attachwire.Frame{Type: attachwire.TypeMarker, Payload: attachwire.MarkerPayload{Label: label}.Encode()})
	return nil
}

// errFakeSessionEvicted mirrors agent.ErrRingMiss's shape without importing
// agent (attachclient deliberately does not, see session.go): production code
// only ever branches on Subscribe failing at all in this context, never on the
// concrete error identity, so a plain sentinel here is a faithful stand-in.
var errFakeSessionEvicted = errors.New("fakeSession: fromSeq evicted from local ring")

// SetEvictBelow arms the simulated local-ring eviction boundary (see
// evictBelow) for ring-miss tests.
func (fs *fakeSession) SetEvictBelow(seq uint64) {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	fs.evictBelow = seq
}

// SubscribeSeqs returns every fromSeq RunHost has passed to Subscribe so far.
func (fs *fakeSession) SubscribeSeqs() []attachwire.HostSeq {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	out := make([]attachwire.HostSeq, len(fs.subscribeSeqs))
	copy(out, fs.subscribeSeqs)
	return out
}

func (fs *fakeSession) Subscribe(fromSeq attachwire.HostSeq) (Subscription, error) {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	fs.subscribeSeqs = append(fs.subscribeSeqs, fromSeq)
	if fromSeq > 0 && uint64(fromSeq) < fs.evictBelow {
		return nil, errFakeSessionEvicted
	}
	sub := &fakeSub{fs: fs, ch: make(chan attachwire.Frame, 16384)}
	for _, f := range fs.ring {
		if uint64(f.Seq) > uint64(fromSeq) {
			sub.ch <- f
		}
	}
	if fs.exited {
		close(sub.ch)
		sub.closed = true
	} else {
		fs.subs[sub] = struct{}{}
	}
	return sub, nil
}

func (fs *fakeSession) Done() <-chan struct{} { return fs.doneCh }

func (fs *fakeSession) Exit() (attachwire.ExitPayload, bool) {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	return fs.exitPayload, fs.exited
}

func (s *fakeSub) Frames() <-chan attachwire.Frame { return s.ch }

func (s *fakeSub) Close() error {
	s.fs.mu.Lock()
	defer s.fs.mu.Unlock()
	delete(s.fs.subs, s)
	if !s.closed {
		close(s.ch)
		s.closed = true
	}
	return nil
}

// ---- token helpers ----------------------------------------------------------

func b64url(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }

// mkHostToken builds an unverified host JWT (the stub does not check the
// signature). epoch is included iff withEpoch.
func mkHostToken(sessionID string, epoch int64, jti string, withEpoch bool) string {
	claims := map[string]any{
		"sessionId": sessionID,
		"roomId":    sessionID,
		"role":      "host",
		"orgId":     "org-1",
		"aud":       "relay",
		"jti":       jti,
		"iat":       time.Now().Unix(),
		"exp":       time.Now().Add(time.Hour).Unix(),
	}
	if withEpoch {
		claims["epoch"] = epoch
	}
	return fakeJWT(claims)
}

func mkViewerToken(sessionID, userID, jti, role string) string {
	claims := map[string]any{
		"sessionId": sessionID,
		"roomId":    sessionID,
		"userId":    userID,
		"role":      role,
		"orgId":     "org-1",
		"aud":       "relay",
		"jti":       jti,
		"iat":       time.Now().Unix(),
		"exp":       time.Now().Add(time.Hour).Unix(),
	}
	return fakeJWT(claims)
}

func fakeJWT(claims map[string]any) string {
	hdr := b64url([]byte(`{"alg":"EdDSA","typ":"JWT"}`))
	pb, _ := json.Marshal(claims)
	return strings.Join([]string{hdr, b64url(pb), b64url([]byte("sig"))}, ".")
}

// staticToken returns a TokenSource that always yields tok and counts calls.
func staticToken(tok string, calls *atomic.Int64) TokenSource {
	return func(_ context.Context) (string, error) {
		if calls != nil {
			calls.Add(1)
		}
		return tok, nil
	}
}
