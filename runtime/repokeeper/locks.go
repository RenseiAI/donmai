package repokeeper

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"time"
)

// Cross-process mirror locks.
//
// The in-process mutex map (lockMirror) serializes goroutines inside one
// Store, but two Store instances — two daemon processes, or a daemon and a
// CLI — share one state dir with no shared memory. Every mirror digest owns
// one lock file, <locks>/<digest>.lock, and every mutation or use takes a
// POSIX flock on it: readers (leases) take LOCK_SH, maintenance (create,
// fetch, evict, remove) takes LOCK_EX. A holder that dies releases its lock
// with its file description, so a crash never wedges the store; a waiter
// that loses a create race re-verifies the winner instead of failing.
//
// Lock files are never deleted: two creators racing an unlink could flock
// different inodes and both believe they won. They are zero bytes, one per
// mirror ever created, inside the daemon-managed state dir.
//
// Like the rest of the store this targets the daemon's platforms
// (darwin/linux), where syscall.Flock exists — the same assumption the
// workarea acquisition locks already make.

// mirrorLockRetryInterval paces blocking flock acquisition so ctx
// cancellation stays responsive while waiting on another process.
const mirrorLockRetryInterval = 10 * time.Millisecond

// lockFileNameFor validates the digest shape before it reaches the path so
// a caller cannot steer the lock at an arbitrary file. It mirrors
// MirrorDir's validation; the ".lock" suffix keeps lock files distinct
// from mirror directories.
func lockFileNameFor(digest string) (string, error) {
	if len(digest) != 64 {
		return "", fmt.Errorf("repo keeper: refusing to lock a path outside the mirrors directory")
	}
	for _, c := range digest {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return "", fmt.Errorf("repo keeper: refusing to lock a path outside the mirrors directory")
		}
	}
	return digest + ".lock", nil
}

// openMirrorLock opens (creating owner-only) the flock file for digest,
// confined under the store root. The locks directory is created first so
// the first Ensure on a fresh state home can lock before it publishes.
// The create-then-open retries on ENOENT: two creators racing first use
// can both pass MkdirAll yet resolve the parent before the other's
// commit lands, and the lock must survive that race, not fail it.
// The caller owns the returned file and must unlockAndClose it.
func (s *Store) openMirrorLock(digest string) (*os.File, error) {
	name, err := lockFileNameFor(digest)
	if err != nil {
		return nil, err
	}
	var lock *os.File
	for attempt := 0; ; attempt++ {
		if err := s.root.MkdirAll(filepath.Join(keeperDirName, locksDirName), 0o700); err != nil {
			return nil, fmt.Errorf("repo keeper: create lock dir: %w", err)
		}
		lock, err = s.root.OpenFile(locksRelPath(name), os.O_CREATE|os.O_RDWR, 0o600)
		if err == nil {
			return lock, nil
		}
		if !os.IsNotExist(err) || attempt >= 20 {
			return nil, fmt.Errorf("repo keeper: open mirror lock: %w", err)
		}
		time.Sleep(time.Millisecond)
	}
}

// lockSharedCtx takes a shared (reader) flock, retrying until ctx lapses.
// Leases hold it from verification to Release; maintenance takes the
// exclusive side.
func lockSharedCtx(fd int, ctx ctxDone) error {
	for {
		if err := syscall.Flock(fd, syscall.LOCK_SH|syscall.LOCK_NB); err == nil {
			return nil
		} else if !errors.Is(err, syscall.EWOULDBLOCK) && !errors.Is(err, syscall.EAGAIN) {
			return fmt.Errorf("repo keeper: take shared mirror lock: %w", err)
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("repo keeper: take shared mirror lock: %w", ctx.Err())
		case <-time.After(mirrorLockRetryInterval):
		}
	}
}

// lockExclusiveCtx takes an exclusive (maintenance) flock, retrying until
// ctx lapses.
func lockExclusiveCtx(fd int, ctx ctxDone) error {
	for {
		if err := syscall.Flock(fd, syscall.LOCK_EX|syscall.LOCK_NB); err == nil {
			return nil
		} else if !errors.Is(err, syscall.EWOULDBLOCK) && !errors.Is(err, syscall.EAGAIN) {
			return fmt.Errorf("repo keeper: take exclusive mirror lock: %w", err)
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("repo keeper: take exclusive mirror lock: %w", ctx.Err())
		case <-time.After(mirrorLockRetryInterval):
		}
	}
}

// syscallFlockExclusive takes an exclusive flock, blocking until it is
// available. For short critical sections with no ctx to honor (the catalog
// publish); a dead holder releases its lock with its file description, so
// this cannot wedge on a crash.
func syscallFlockExclusive(fd int) error {
	if err := syscall.Flock(fd, syscall.LOCK_EX); err != nil {
		return fmt.Errorf("repo keeper: take exclusive mirror lock: %w", err)
	}
	return nil
}

// unlockAndClose releases the flock and closes the file. Either half may
// fail on a torn-down descriptor; the first error is reported.
func unlockAndClose(lock *os.File) error {
	if lock == nil {
		return nil
	}
	fd, err := intFileDescriptor(lock.Fd())
	if err != nil {
		_ = lock.Close()
		return err
	}
	unlockErr := syscall.Flock(fd, syscall.LOCK_UN)
	closeErr := lock.Close()
	if unlockErr != nil {
		return fmt.Errorf("repo keeper: release mirror lock: %w", unlockErr)
	}
	if closeErr != nil {
		return fmt.Errorf("repo keeper: close mirror lock: %w", closeErr)
	}
	return nil
}

// intFileDescriptor narrows a uintptr fd to int for syscall.Flock.
func intFileDescriptor(fd uintptr) (int, error) {
	intFD := int(fd)
	if intFD < 0 || uintptr(intFD) != fd {
		return 0, fmt.Errorf("repo keeper: lock file descriptor out of range")
	}
	return intFD, nil
}
