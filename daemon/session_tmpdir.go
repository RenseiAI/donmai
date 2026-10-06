package daemon

// Per-session scratch directories (executor half of seat isolation).
//
// Every seat session gets its own scratch directory on the seat host, so
// concurrent seats cannot read or overwrite each other's temp files. The
// platform stamps exactly one advisory key on the seat's non-secret scope
// env — DONMAI_SESSION_TMPDIR=<absolute path> — and the daemon's per-session
// env composition carries it into the env the executor receives. The
// platform never stamps TMPDIR, TMP or TEMP itself, and no key means today's
// behaviour.
//
// The executor (this spawner's launch path) validates the advisory value and
// creates the directory (mode 0700, owned by the seat user) before the
// worker process starts, then binds TMPDIR, TMP and TEMP to it for the
// worker and every child it spawns. Creation failure fails the launch
// closed: it never falls back to the shared host temp.
//
// Teardown removes the directory on every terminal path (success, failure,
// stop/cancel). Crash recovery is covered by an executor-side record — a
// small JSON file naming exactly the directories this executor created —
// swept at daemon startup for sessions that are no longer running. The
// sweep never globs a name prefix: only recorded paths that still validate
// are removed.
//
// Validation fails closed throughout: the value must be absolute and stable
// under filepath.Clean, a direct child of /tmp (/private/tmp accepted, the
// macOS spelling of the same directory), with a final segment in
// [A-Za-z0-9._-]{1,160} and no "..". A path that already exists is reused
// only when Lstat shows a real directory (never a symlink), owned by the
// current uid, with mode exactly 0700 — which closes the squat and symlink
// attacks a shared /tmp invites.
//
// Concurrent sessions stamped with the SAME path (a platform duplication,
// never the contract) share it without deleting each other's files: every
// cleanup proves ownership of the exact generation it removes via a marker
// file written at creation, so an older generation never removes a newer
// generation's directory.

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"time"

	runtimeenv "github.com/RenseiAI/donmai/runtime/env"
)

// SessionTmpDirEnv is the advisory key the platform stamps on a seat's
// non-secret scope env naming that seat's scratch directory. Advisory: the
// executor validates it before trusting it, and the platform never stamps
// TMPDIR, TMP or TEMP (stamping those would reach the worker before any
// directory exists).
const SessionTmpDirEnv = "DONMAI_SESSION_TMPDIR"

// sessionTmpBindingKeys are the variables bound to the session scratch
// directory for the worker and every child it spawns.
var sessionTmpBindingKeys = []string{"TMPDIR", "TMP", "TEMP"}

// sessionTmpOwnerFile is the marker written into every executor-created
// scratch directory at creation. It names the owning session and the
// creation generation, so a cleanup proves it removes its own generation's
// directory — never a newer generation's sharing the same path.
const sessionTmpOwnerFile = ".owner.json"

// sessionTmpMaxNameLen bounds the final path segment. The platform's
// session ids are at most 128 chars; headroom covers the file name the
// platform derives from one without admitting unbounded names.
const sessionTmpMaxNameLen = 160

// validateSessionTmpDir checks an advisory scratch-directory value and
// returns its cleaned form. Every refusal fails closed with a message
// naming the offending value and the rule it broke.
func validateSessionTmpDir(raw string) (string, error) {
	if raw == "" {
		return "", errors.New("session tmpdir: empty value")
	}
	if !filepath.IsAbs(raw) {
		return "", fmt.Errorf("session tmpdir: %q is not an absolute path", raw)
	}
	cleaned := filepath.Clean(raw)
	if cleaned != raw {
		return "", fmt.Errorf("session tmpdir: %q is not stable under Clean (want %q)", raw, cleaned)
	}
	if parent := filepath.Dir(cleaned); parent != "/tmp" && parent != "/private/tmp" {
		return "", fmt.Errorf("session tmpdir: %q is not a direct child of /tmp", raw)
	}
	base := filepath.Base(cleaned)
	if len(base) == 0 || len(base) > sessionTmpMaxNameLen {
		return "", fmt.Errorf("session tmpdir: name %q exceeds %d characters", base, sessionTmpMaxNameLen)
	}
	if base == "." || base == ".." {
		return "", fmt.Errorf("session tmpdir: name %q is not a real directory name", base)
	}
	for _, r := range base {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '.' || r == '_' || r == '-' {
			continue
		}
		return "", fmt.Errorf("session tmpdir: name %q holds %q, outside [A-Za-z0-9._-]", base, string(r))
	}
	return cleaned, nil
}

// errSessionTmpOwnershipUnavailable reports that the current process cannot
// prove directory ownership on this platform (no uid-carrying stat). It is
// returned instead of an ownership verdict wherever the check cannot run.
var errSessionTmpOwnershipUnavailable = errors.New("session tmpdir: ownership cannot be verified on this platform")

// verifySessionTmpOwner reports whether fi names a directory owned by uid.
// The assertion is on the Lstat result the caller already holds, so the
// ownership verdict and the type/mode verdicts below judge one observation.
func verifySessionTmpOwner(fi os.FileInfo, uid int) error {
	owner, err := sessionTmpOwnerUID(fi)
	if err != nil {
		return err
	}
	if owner != uid {
		return fmt.Errorf("session tmpdir: owned by uid %d, want uid %d", owner, uid)
	}
	return nil
}

// verifySessionTmpExisting judges an Lstat observation of a path the
// executor did not just create: a real directory (never a symlink), mode
// exactly 0700, owned by uid. Anything else is refused, which closes the
// squat and symlink attacks in a shared /tmp.
func verifySessionTmpExisting(dir string, fi os.FileInfo, uid int) error {
	if fi.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("session tmpdir: %q is a symlink (refused)", dir)
	}
	if !fi.Mode().IsDir() {
		return fmt.Errorf("session tmpdir: %q is not a directory", dir)
	}
	if perm := fi.Mode().Perm(); perm != 0o700 {
		return fmt.Errorf("session tmpdir: %q has mode %04o, want 0700", dir, perm)
	}
	if err := verifySessionTmpOwner(fi, uid); err != nil {
		return fmt.Errorf("session tmpdir: %q: %w", dir, err)
	}
	return nil
}

// ensureSessionTmpDir validates the advisory value and makes the directory
// real: created 0700 when absent, reused only when it still passes the
// ownership checks above. It returns the cleaned path. A creation umask can
// only strip bits the explicit Chmod then restores, and the final Lstat
// judges what is on disk rather than what Mkdir asked for.
func ensureSessionTmpDir(raw string) (string, error) {
	dir, err := validateSessionTmpDir(raw)
	if err != nil {
		return "", err
	}
	fi, err := os.Lstat(dir)
	if err != nil {
		if !os.IsNotExist(err) {
			return "", fmt.Errorf("session tmpdir: inspect %q: %w", dir, err)
		}
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return "", fmt.Errorf("session tmpdir: create %q: %w", dir, err)
		}
		if err := os.Chmod(dir, 0o700); err != nil { //nolint:gosec // G302: restoring the exact 0700 the reuse check demands after a umask-weakened MkdirAll.
			_ = os.Remove(dir)
			return "", fmt.Errorf("session tmpdir: secure %q: %w", dir, err)
		}
		fi, err = os.Lstat(dir)
		if err != nil {
			return "", fmt.Errorf("session tmpdir: re-inspect %q: %w", dir, err)
		}
	}
	if err := verifySessionTmpExisting(dir, fi, os.Getuid()); err != nil {
		return "", err
	}
	return dir, nil
}

// sessionTmpClaim is one generation's ownership of a scratch directory: the
// path plus the spawner-local generation that created it. The zero value
// owns nothing.
type sessionTmpClaim struct {
	Dir string
	Gen uint64
}

// owned reports whether the claim names a directory.
func (c sessionTmpClaim) owned() bool {
	return c.Dir != ""
}

// sessionTmpOwnerRecord is the marker file content: the owning session and
// generation. A cleanup removes the directory only when the marker still
// names its own claim, so generations sharing one path never delete each
// other's files.
type sessionTmpOwnerRecord struct {
	SessionID string `json:"sessionId"`
	Gen       uint64 `json:"gen"`
}

// writeSessionTmpOwner stamps the ownership marker inside a directory the
// executor just validated. The directory is ours (0700, our uid), so its
// contents are ours to write.
func writeSessionTmpOwner(dir, sessionID string, gen uint64) error {
	raw, err := json.Marshal(sessionTmpOwnerRecord{SessionID: sessionID, Gen: gen})
	if err != nil {
		return fmt.Errorf("session tmpdir: encode owner for %q: %w", dir, err)
	}
	if err := os.WriteFile(filepath.Join(dir, sessionTmpOwnerFile), raw, 0o600); err != nil {
		return fmt.Errorf("session tmpdir: stamp owner for %q: %w", dir, err)
	}
	return nil
}

// sessionTmpOwnerMatches reports whether the marker inside dir still names
// this exact claim. A missing or unreadable marker, or one naming another
// claim, refuses the removal — the directory is either already gone or no
// longer ours to remove.
func sessionTmpOwnerMatches(dir string, claim sessionTmpClaim) bool {
	raw, err := os.ReadFile(filepath.Join(dir, sessionTmpOwnerFile)) //nolint:gosec // G304: dir is a validated /tmp child and the name a constant.
	if err != nil {
		return false
	}
	var record sessionTmpOwnerRecord
	if err := json.Unmarshal(raw, &record); err != nil {
		return false
	}
	return record.Gen == claim.Gen && record.Gen != 0
}

// applySessionTmpBindings returns env with the three scratch bindings set
// to dir. Any prior binding of those keys is dropped first, so the session
// scratch is bound exactly once however the worker was otherwise
// configured; the advisory key itself is left alone.
func applySessionTmpBindings(env []string, dir string) []string {
	out := make([]string, 0, len(env)+len(sessionTmpBindingKeys))
dropped:
	for _, kv := range env {
		key := kv
		if i := strings.IndexByte(kv, '='); i >= 0 {
			key = kv[:i]
		}
		for _, bound := range sessionTmpBindingKeys {
			if key == bound {
				continue dropped
			}
		}
		out = append(out, kv)
	}
	for _, key := range sessionTmpBindingKeys {
		out = append(out, key+"="+dir)
	}
	return out
}

// sessionTmpRecordEntry is one recorded creation: the path plus when this
// executor created it. The generation is deliberately not recorded: after a
// restart generations reset, and the sweep protects live sessions by id,
// not by generation.
type sessionTmpRecordEntry struct {
	Path      string `json:"path"`
	CreatedAt string `json:"createdAt"`
}

// sessionTmpRecord is the executor-side record of created scratch
// directories, persisted as JSON. It names exactly what this executor
// created — the startup sweep removes only recorded paths that still
// validate, never a name-prefix glob of the shared temp.
type sessionTmpRecord struct {
	Version int                              `json:"version"`
	Entries map[string]sessionTmpRecordEntry `json:"entries"`
}

// sessionTmpRecordFileName is the file inside the record directory holding
// the executor-side record.
const sessionTmpRecordFileName = "session-tmpdirs.json"

// loadSessionTmpRecord reads the record. A missing file is an empty record;
// a corrupt one is an error — the executor cannot safely update what it
// cannot decode, so callers fail closed.
func loadSessionTmpRecord(dir string) (sessionTmpRecord, error) {
	record := sessionTmpRecord{Version: 1, Entries: map[string]sessionTmpRecordEntry{}}
	if dir == "" {
		return record, nil
	}
	raw, err := os.ReadFile(filepath.Join(dir, sessionTmpRecordFileName)) //nolint:gosec // G304: dir is operator configuration and the name a constant.
	if err != nil {
		if os.IsNotExist(err) {
			return record, nil
		}
		return sessionTmpRecord{}, fmt.Errorf("session tmpdir: read record: %w", err)
	}
	var decoded sessionTmpRecord
	if err := json.Unmarshal(raw, &decoded); err != nil {
		return sessionTmpRecord{}, fmt.Errorf("session tmpdir: decode record: %w", err)
	}
	if decoded.Entries == nil {
		decoded.Entries = map[string]sessionTmpRecordEntry{}
	}
	return decoded, nil
}

// saveSessionTmpRecord persists the record atomically (temp file plus
// rename) under a 0700 directory: it names session paths, so it is not
// world-readable.
func saveSessionTmpRecord(dir string, record sessionTmpRecord) error {
	if dir == "" {
		return errors.New("session tmpdir: no record directory configured")
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("session tmpdir: create record directory: %w", err)
	}
	raw, err := json.Marshal(record)
	if err != nil {
		return fmt.Errorf("session tmpdir: encode record: %w", err)
	}
	tmp, err := os.CreateTemp(dir, "session-tmpdirs-*.tmp")
	if err != nil {
		return fmt.Errorf("session tmpdir: stage record: %w", err)
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(raw); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
		return fmt.Errorf("session tmpdir: write record: %w", err)
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpName)
		return fmt.Errorf("session tmpdir: write record: %w", err)
	}
	if err := os.Chmod(tmpName, 0o600); err != nil {
		_ = os.Remove(tmpName)
		return fmt.Errorf("session tmpdir: secure record: %w", err)
	}
	if err := os.Rename(tmpName, filepath.Join(dir, sessionTmpRecordFileName)); err != nil {
		_ = os.Remove(tmpName)
		return fmt.Errorf("session tmpdir: publish record: %w", err)
	}
	return nil
}

// nextSessionTmpGen issues creation generations. It is process-local:
// generations prove same-path ownership between overlapping lifetimes in
// this process, and the marker resets them on every creation.
var nextSessionTmpGen atomic.Uint64

// prepareSessionTmpDir validates the advisory value carried on the session
// env, ensures the directory, stamps ownership and records the creation.
// Empty (no key) returns no claim: today's behaviour. Every error fails the
// launch closed before any worker process starts.
func (s *WorkerSpawner) prepareSessionTmpDir(spec SessionSpec) (sessionTmpClaim, error) {
	raw := runtimeenv.FilterHostOwnedMap(spec.Env)[SessionTmpDirEnv]
	if raw == "" {
		return sessionTmpClaim{}, nil
	}
	dir, err := ensureSessionTmpDir(raw)
	if err != nil {
		return sessionTmpClaim{}, err
	}
	gen := nextSessionTmpGen.Add(1)
	if gen == 0 {
		gen = nextSessionTmpGen.Add(1)
	}
	claim := sessionTmpClaim{Dir: dir, Gen: gen}
	if err := writeSessionTmpOwner(dir, spec.SessionID, gen); err != nil {
		_ = os.RemoveAll(dir)
		return sessionTmpClaim{}, err
	}
	s.mu.Lock()
	if s.sessionTmpDirs == nil {
		s.sessionTmpDirs = map[string]sessionTmpClaim{}
	}
	s.sessionTmpDirs[spec.SessionID] = claim
	recordDir := s.opts.SessionTmpRecordDir
	s.mu.Unlock()
	if recordDir != "" {
		record, err := loadSessionTmpRecord(recordDir)
		if err != nil {
			s.forgetSessionTmpClaim(spec.SessionID, claim)
			_ = os.RemoveAll(dir)
			return sessionTmpClaim{}, err
		}
		record.Entries[spec.SessionID] = sessionTmpRecordEntry{Path: dir, CreatedAt: time.Now().UTC().Format(time.RFC3339)}
		if err := saveSessionTmpRecord(recordDir, record); err != nil {
			s.forgetSessionTmpClaim(spec.SessionID, claim)
			_ = os.RemoveAll(dir)
			return sessionTmpClaim{}, err
		}
	}
	return claim, nil
}

// forgetSessionTmpClaim drops the in-memory claim when it still names this
// exact generation. A generation that already replaced it is left alone.
func (s *WorkerSpawner) forgetSessionTmpClaim(sessionID string, claim sessionTmpClaim) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if current, ok := s.sessionTmpDirs[sessionID]; ok && current == claim {
		delete(s.sessionTmpDirs, sessionID)
	}
}

// forgetSessionTmpRecordEntry drops one record entry and persists. A missing
// record directory (memory-only mode) makes it a no-op.
func (s *WorkerSpawner) forgetSessionTmpRecordEntry(sessionID string) {
	recordDir := s.opts.SessionTmpRecordDir
	if recordDir == "" {
		return
	}
	record, err := loadSessionTmpRecord(recordDir)
	if err != nil {
		return
	}
	if _, ok := record.Entries[sessionID]; !ok {
		return
	}
	delete(record.Entries, sessionID)
	_ = saveSessionTmpRecord(recordDir, record)
}

// sessionTmpClaimFor reports the live scratch-directory claim for a session,
// for owners that did not spawn the session themselves (the session-shim
// launcher stashes it on its adopted entry so terminal cleanup stays
// exact). False when the session has no claim.
func (s *WorkerSpawner) sessionTmpClaimFor(sessionID string) (sessionTmpClaim, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	claim, ok := s.sessionTmpDirs[sessionID]
	return claim, ok && claim.owned()
}

// removeSessionTmpClaimDir removes the claimed directory after proving the
// ownership marker still names the claim. Anything else — a missing marker,
// one naming another generation, a path that no longer validates — refuses
// the removal and reports false.
func removeSessionTmpClaimDir(claim sessionTmpClaim) bool {
	if !claim.owned() {
		return false
	}
	if _, err := validateSessionTmpDir(claim.Dir); err != nil {
		return false
	}
	if !sessionTmpOwnerMatches(claim.Dir, claim) {
		return false
	}
	if err := os.RemoveAll(claim.Dir); err != nil {
		return false
	}
	return true
}

// cleanupSessionTmpClaim removes one generation's scratch directory when
// the live claim still names it, then forgets the claim and its record
// entry. Idempotent: a replaced or already-removed claim is a no-op. It
// runs on every terminal path, so removal is best-effort — the session
// already ended — but a skipped removal is always a proven decision, never
// a silent leak: the startup sweep retries whatever is still recorded.
func (s *WorkerSpawner) cleanupSessionTmpClaim(sessionID string, claim sessionTmpClaim) {
	if !claim.owned() {
		return
	}
	s.mu.Lock()
	current, ok := s.sessionTmpDirs[sessionID]
	if !ok || current != claim {
		s.mu.Unlock()
		return
	}
	delete(s.sessionTmpDirs, sessionID)
	s.mu.Unlock()
	_ = removeSessionTmpClaimDir(claim)
	s.forgetSessionTmpRecordEntry(sessionID)
}

// CleanupSessionTmpDir removes the live scratch-directory claim for a
// session, whatever generation holds it. It is the terminal-cleanup entry
// point for owners that only know the session id (the daemon's session-end
// listener, which covers shim-owned sessions the spawner keeps no process
// bookkeeping for). A replaced claim is still proven by its marker before
// removal.
func (s *WorkerSpawner) CleanupSessionTmpDir(sessionID string) {
	s.mu.Lock()
	claim, ok := s.sessionTmpDirs[sessionID]
	if !ok {
		s.mu.Unlock()
		return
	}
	delete(s.sessionTmpDirs, sessionID)
	s.mu.Unlock()
	_ = removeSessionTmpClaimDir(claim)
	s.forgetSessionTmpRecordEntry(sessionID)
}

// SessionTmpSweepReport counts one startup sweep of recorded scratch
// directories.
type SessionTmpSweepReport struct {
	// Examined is the number of record entries read.
	Examined int
	// Removed is the number of recorded directories proven ours, unowned
	// by a live session, and deleted.
	Removed int
	// KeptLive is the number of recorded directories still owned by a
	// running session.
	KeptLive int
	// Dropped is the number of record entries discarded without deleting:
	// already gone, malformed, or no longer provably ours.
	Dropped int
}

// SweepSessionTmpDirs removes recorded scratch directories whose sessions
// are no longer running. liveIDs names the sessions still alive (adopted
// lineages across a restart, plus the spawner's own live sessions, which
// are always protected). Only recorded paths that still validate AND still
// pass the ownership checks are removed — never a name-prefix glob, never
// an unproven path. A removal failure keeps the entry for the next restart.
// It never fails startup: every anomaly is counted, and the sweep degrades
// to leaving directories behind rather than deleting the wrong ones.
func (s *WorkerSpawner) SweepSessionTmpDirs(liveIDs map[string]struct{}) SessionTmpSweepReport {
	var report SessionTmpSweepReport
	recordDir := s.opts.SessionTmpRecordDir
	if recordDir == "" {
		return report
	}
	record, err := loadSessionTmpRecord(recordDir)
	if err != nil {
		slog.Warn("session scratch sweep: unreadable record, reclaiming nothing", "err", err)
		return report
	}
	protected := map[string]struct{}{}
	for id := range liveIDs {
		protected[id] = struct{}{}
	}
	s.mu.Lock()
	for id := range s.sessions {
		protected[id] = struct{}{}
	}
	for id := range s.spawnReservations {
		protected[id] = struct{}{}
	}
	s.mu.Unlock()
	changed := false
	for sessionID, entry := range record.Entries {
		report.Examined++
		if _, live := protected[sessionID]; live {
			report.KeptLive++
			continue
		}
		if dropSessionTmpSweepEntry(entry) {
			delete(record.Entries, sessionID)
			changed = true
			report.Dropped++
			continue
		}
		if err := os.RemoveAll(entry.Path); err != nil {
			continue
		}
		delete(record.Entries, sessionID)
		changed = true
		report.Removed++
	}
	if changed {
		if err := saveSessionTmpRecord(recordDir, record); err != nil {
			slog.Warn("session scratch sweep: could not persist the pruned record", "err", err)
		}
	}
	return report
}

// sweepSessionTmpDirs reclaims per-session scratch directories recorded by
// a previous daemon generation for sessions that are no longer running.
// Live adopted lineages (known because adoption ran earlier in Start) keep
// their directories. It never fails startup.
func (d *Daemon) sweepSessionTmpDirs() {
	if d.spawner == nil {
		return
	}
	live := map[string]struct{}{}
	for _, id := range d.AdoptedSessionShims() {
		live[id.SessionID] = struct{}{}
	}
	report := d.spawner.SweepSessionTmpDirs(live)
	if report.Examined == 0 {
		return
	}
	slog.Info("daemon: session scratch sweep complete",
		"examined", report.Examined,
		"removed", report.Removed,
		"keptLive", report.KeptLive,
		"dropped", report.Dropped,
	)
}

// dropSessionTmpSweepEntry reports whether a sweep entry must be discarded
// without deleting: an empty path, one that no longer validates, one
// already gone, or one that fails the ownership checks (not provably ours
// anymore — a squat the executor must not "reclaim").
func dropSessionTmpSweepEntry(entry sessionTmpRecordEntry) bool {
	if _, err := validateSessionTmpDir(entry.Path); err != nil {
		return true
	}
	fi, err := os.Lstat(entry.Path)
	if err != nil {
		return true
	}
	return verifySessionTmpExisting(entry.Path, fi, os.Getuid()) != nil
}
