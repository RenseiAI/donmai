package ptyhost

import (
	"errors"
	"sync"

	"github.com/RenseiAI/donmai/agent"
	"github.com/RenseiAI/donmai/attachwire"
	"github.com/RenseiAI/donmai/attachwire/sanitize"
)

// LocalUserID is the fixed user id the local attach stamps onto input (§5
// standalone stamper). There is no relay in the standalone path, so the local
// endpoint plays the stamper: it stamps "local" and applies the trivial
// single-local-driver policy.
const LocalUserID = "local"

// ErrLocalReadOnly is returned by a read-only LocalAttach's write/resize
// methods. Only the single live driver attach may write (§11.1 standalone
// single-local-driver policy).
var ErrLocalReadOnly = errors.New("ptyhost: local attach is read-only (another local driver holds the pen)")

// LocalAttachOptions configures a local attach.
type LocalAttachOptions struct {
	// FromSeq is the resume position (§13): the highest host seq already applied.
	// Zero replays from the oldest buffered frame.
	FromSeq attachwire.HostSeq
}

// LocalAttach is the in-process viewer/driver surface for the OSS-standalone
// case (§5, §12 local-attach scope). It is NOT a network endpoint: this package
// opens no listener of any kind (a CI grep asserts it). All viewer-bound Output
// bytes pass a fresh §9 sanitizer (defense in depth) before reaching Frames();
// the raw host→relay leg (Session.Subscribe) is unaffected.
//
// Driver policy: the first live attach becomes the single driver (CanDrive() ==
// true) and may WriteInput/Resize; a concurrent second attach is read-only until
// the driver closes. This is the standalone single-local-driver minimum (§11.1);
// richer arbitration is platform-defined and out of OSS scope.
type LocalAttach struct {
	sess   *Session
	sub    agent.InteractiveSubscription
	san    *sanitize.Sanitizer
	frames chan attachwire.Frame
	userID string
	driver bool

	closeOnce sync.Once
}

// AttachLocal opens an in-process local attach. It never fails on the
// single-driver policy — a second concurrent attach simply comes back read-only
// (CanDrive() == false). It returns an error only if the underlying subscription
// misses the ring (agent.ErrRingMiss); recover by snapshotting and re-attaching
// from the snapshot's atSeq.
func (s *Session) AttachLocal(opts LocalAttachOptions) (*LocalAttach, error) {
	sub, err := s.Subscribe(opts.FromSeq)
	if err != nil {
		return nil, err
	}

	s.mu.Lock()
	driver := !s.localDriver
	if driver {
		s.localDriver = true
	}
	s.mu.Unlock()

	la := &LocalAttach{
		sess:   s,
		sub:    sub,
		san:    sanitize.New(),
		frames: make(chan attachwire.Frame),
		userID: LocalUserID,
		driver: driver,
	}
	go la.pump()
	return la, nil
}

// pump forwards subscription frames, passing Output payloads through the §9
// sanitizer (viewer-bound bytes). Other frame types pass through unchanged —
// snapFormat 0x01 Snapshot payloads are escape-safe by construction (§12.1) so
// they carry no sanitizable escapes.
func (la *LocalAttach) pump() {
	defer close(la.frames)
	for f := range la.sub.Frames() {
		if f.Type == attachwire.TypeOutput {
			f.Payload = la.san.Write(f.Payload)
		}
		la.frames <- f
	}
}

// Frames is the sanitized, viewer-bound frame feed.
func (la *LocalAttach) Frames() <-chan attachwire.Frame { return la.frames }

// CanDrive reports whether this attach holds the single local-driver pen and may
// write input / resize.
func (la *LocalAttach) CanDrive() bool { return la.driver }

// UserID is the stamped local user id ("local", §5).
func (la *LocalAttach) UserID() string { return la.userID }

// WriteInput writes input to the PTY as the local driver (§5). A read-only
// attach returns ErrLocalReadOnly.
func (la *LocalAttach) WriteInput(p []byte) (int, error) {
	if !la.driver {
		return 0, ErrLocalReadOnly
	}
	return la.sess.WriteInput(p)
}

// Resize applies geometry as the local driver (§8). A read-only attach returns
// ErrLocalReadOnly.
func (la *LocalAttach) Resize(cols, rows, pxWidth, pxHeight uint32) error {
	if !la.driver {
		return ErrLocalReadOnly
	}
	return la.sess.Resize(cols, rows, pxWidth, pxHeight)
}

// Snapshot returns the current screen (§12.1), read-only. Available to viewers
// and drivers alike.
func (la *LocalAttach) Snapshot() (attachwire.Screen, attachwire.HostSeq, error) {
	return la.sess.Snapshot()
}

// Close releases the attach and, if it was the driver, frees the driver pen for
// a future attach. Idempotent.
func (la *LocalAttach) Close() error {
	la.closeOnce.Do(func() {
		if la.driver {
			la.sess.mu.Lock()
			la.sess.localDriver = false
			la.sess.mu.Unlock()
		}
		_ = la.sub.Close()
	})
	return nil
}
