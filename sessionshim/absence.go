package sessionshim

// absence.go — the withdrawn-record sidecar a daemon leaves behind when it
// proves a shim is no longer observable.
//
// WHY A RENAME AND NOT AN UNLINK
//
// A shim-absent attestation must state two facts at the moment it is composed:
// the recorded process identity is not running, and the discovery record for
// that exact incarnation is gone (§D10). The second fact is the daemon's to
// make true — a shim killed without running its finalizer leaves its record
// behind — but making it true by UNLINKING the record destroys the only copy
// of the first fact. A daemon that unlinks, then has its report refused, then
// restarts, can never compose the attestation again: the pid and start time it
// proved absent are off disk, so the lineage is invisible to Adopt, absent
// from the batch, and still held by the composer. That is worse than never
// having tried — an omitted correlation refuses the whole host's next complete
// batch.
//
// So the record is RENAMED rather than removed. A `.absent` sidecar is not a
// discovery record: Scan filters on the record suffix and never enumerates it,
// so Adopt cannot re-classify it stale and no daemon can adopt it. The
// discovery record for the incarnation is genuinely gone. But the bytes
// survive, so a daemon that restarts mid-flight re-reads the sidecar and
// re-submits the same attestation with the same deterministic evidence id.
//
// The sidecar is disposed only after the composer durably accepts, which is
// the same place the tombstone path disposes its proof and for the same
// reason: it is the last artifact that can re-derive the fact.
//
// The write happens BEFORE the unlink, so a crash between them leaves both the
// record and the sidecar rather than neither. That direction is recoverable —
// the record is re-classified stale and re-attested, and publishing the
// sidecar again is idempotent — while the other direction is the loss this
// file exists to prevent.

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"strconv"
)

// absenceSuffix names the withdrawn-record sidecar.
//
// It deliberately does NOT end in the record suffix. entryNames matches by
// suffix, so a name ending in `.json` would be enumerated by Scan and the
// withdrawal would not have withdrawn anything — the same trap the tombstone
// alias has to exclude itself out of. `.absent` cannot collide with the
// record, socket, tombstone, or log suffixes.
const absenceSuffix = ".absent"

// absenceName is the per-incarnation sidecar filename, digested from the exact
// correlation the same way the tombstone and durable-ack sidecars are. Two
// incarnations of one lifecycle identity therefore get separate files, which is
// what keeps §D7's duplicate-identity case from collapsing into one.
func absenceName(id Identity, shimID string, processEpoch uint64) string {
	correlation := id.Key() + "\x1f" + shimID + "\x1f" + strconv.FormatUint(processEpoch, 10)
	sum := sha256.Sum256([]byte(correlation))
	return hex.EncodeToString(sum[:]) + absenceSuffix
}

// WithdrawIncarnationForAbsence renames the exact incarnation's discovery
// record to its absence sidecar, making the record gone while keeping the
// process identity it recorded readable.
//
// It reports whether a record was actually withdrawn. False with a nil error
// means there was no record for this incarnation to withdraw — which is not a
// failure and not a proof either; the caller decides what that means.
//
// The order is publish-then-unlink. A crash between the two leaves both files;
// a crash the other way around would leave neither, which is precisely the
// unrecoverable state this whole mechanism exists to avoid.
func (r *Registry) WithdrawIncarnationForAbsence(id Identity, shimID string, processEpoch uint64) (bool, error) {
	entries, err := r.Scan()
	if err != nil {
		return false, err
	}
	var raw []byte
	for _, entry := range entries {
		if entry.Err != nil || entry.Record.Identity() != id ||
			entry.Record.ShimID != shimID || entry.Record.ProcessEpoch != processEpoch {
			continue
		}
		data, readErr := r.readEntry(entry.Name)
		if readErr != nil {
			return false, fmt.Errorf("sessionshim: read record for absence withdrawal: %w", readErr)
		}
		raw = data
		break
	}
	if raw == nil {
		return false, nil
	}
	if err := r.publish(absenceName(id, shimID, processEpoch), raw); err != nil {
		return false, fmt.Errorf("sessionshim: publish absence sidecar: %w", err)
	}
	if err := r.RemoveIncarnation(id, shimID, processEpoch); err != nil {
		return false, err
	}
	return true, nil
}

// GetWithdrawnAbsence reads one incarnation's sidecar. A missing sidecar
// returns ok=false with a nil error: "this daemon holds no withdrawn record for
// that incarnation" is an answer, not a fault.
func (r *Registry) GetWithdrawnAbsence(id Identity, shimID string, processEpoch uint64) (Record, bool, error) {
	name := absenceName(id, shimID, processEpoch)
	raw, err := r.readEntry(name)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return Record{}, false, nil
		}
		return Record{}, false, fmt.Errorf("sessionshim: read absence sidecar: %w", err)
	}
	rec, err := decodeRecord(raw)
	if err != nil {
		return Record{}, false, fmt.Errorf("sessionshim: decode absence sidecar: %w", err)
	}
	if rec.Identity() != id || rec.ShimID != shimID || rec.ProcessEpoch != processEpoch {
		return Record{}, false, errors.New("sessionshim: absence sidecar does not name its own incarnation")
	}
	return rec, true, nil
}

// ScanWithdrawnAbsences reads every sidecar in the registry, sorted by
// filename. It is how a restarted daemon rediscovers the discharges its
// previous incarnation withdrew but never got acknowledged.
//
// An undecodable sidecar is skipped rather than failing the scan: it can no
// longer prove anything, and refusing the whole pass over one corrupt file
// would strand every other pending discharge behind it.
func (r *Registry) ScanWithdrawnAbsences() ([]Record, error) {
	if err := r.checkDirMode(); err != nil {
		return nil, err
	}
	names, err := r.entryNames(absenceSuffix)
	if err != nil {
		return nil, err
	}
	out := make([]Record, 0, len(names))
	for _, name := range names {
		raw, readErr := r.readEntry(name)
		if readErr != nil {
			continue
		}
		rec, decodeErr := decodeRecord(raw)
		if decodeErr != nil {
			continue
		}
		out = append(out, rec)
	}
	return out, nil
}

// DisposeWithdrawnAbsence removes one incarnation's sidecar. Like every other
// registry removal it is idempotent, and like the tombstone's disposal it is
// called ONLY after the composer has durably accepted the evidence the file
// backs — it is the last artifact that can re-derive the proof.
func (r *Registry) DisposeWithdrawnAbsence(id Identity, shimID string, processEpoch uint64) error {
	root, err := r.openRoot()
	if err != nil {
		return err
	}
	defer func() { _ = root.Close() }()
	if err := root.Remove(absenceName(id, shimID, processEpoch)); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("sessionshim: remove absence sidecar: %w", err)
	}
	return nil
}
