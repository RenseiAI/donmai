package daemon

// logrotate.go — size-capped rotation for the launchd-managed daemon logs.
//
// On macOS the installer's launchd plist points the daemon's stdout/stderr at
// ~/Library/Logs/<brand>/daemon.log and daemon-error.log with no rotation —
// nothing else on the host rotates them, so a long-lived daemon grows them
// without bound (tens of MB of routine token-refresh and poll chatter). On
// Linux the systemd unit logs to the journal, which rotates itself; this
// helper is a no-op there because the files simply don't exist.

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
)

// DefaultLogRotateMaxBytes is the size threshold above which a daemon log
// file is rotated. 10 MiB keeps weeks of routine chatter while bounding the
// on-disk footprint to at most ~2x the threshold per stream (live + one
// rotated generation).
const DefaultLogRotateMaxBytes int64 = 10 << 20

// RotateLogIfOver rotates path in place when it exceeds maxBytes: the current
// contents are copied to path+".1" (replacing any previous generation) and the
// live file is truncated to zero.
//
// Copy-truncate, NOT rename: launchd holds the open file descriptor for the
// daemon's stdout/stderr, so renaming would carry the fd to the new name and
// keep growing it. launchd opens the streams with O_APPEND, so writers
// continue cleanly at offset zero after the in-place truncate.
//
// A missing file, empty path, or non-positive maxBytes is a no-op. Returns
// whether a rotation happened.
func RotateLogIfOver(path string, maxBytes int64) (bool, error) {
	if path == "" || maxBytes <= 0 {
		return false, nil
	}
	fi, err := os.Stat(path)
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("logrotate: stat %s: %w", path, err)
	}
	if !fi.Mode().IsRegular() || fi.Size() <= maxBytes {
		return false, nil
	}

	src, err := os.Open(path) //nolint:gosec // G304: operator-owned daemon log path
	if err != nil {
		return false, fmt.Errorf("logrotate: open %s: %w", path, err)
	}
	defer func() { _ = src.Close() }()

	rotated := path + ".1"
	dst, err := os.OpenFile(rotated, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600) //nolint:gosec // G304: derived from the log path above
	if err != nil {
		return false, fmt.Errorf("logrotate: create %s: %w", rotated, err)
	}
	if _, err := io.Copy(dst, src); err != nil {
		_ = dst.Close()
		return false, fmt.Errorf("logrotate: copy %s -> %s: %w", path, rotated, err)
	}
	if err := dst.Close(); err != nil {
		return false, fmt.Errorf("logrotate: close %s: %w", rotated, err)
	}

	// In-place truncate keeps launchd's fd valid; O_APPEND writers continue
	// at the new EOF. Anything written between the copy and the truncate is
	// lost — acceptable for best-effort observability logs.
	if err := os.Truncate(path, 0); err != nil {
		return false, fmt.Errorf("logrotate: truncate %s: %w", path, err)
	}
	return true, nil
}
