package daemon

// Per-session scratch directories (executor-owned per-session scratch).
//
// Every seat session gets its own scratch directory on the seat host, so
// concurrent seats cannot read or overwrite each other's temp files, and
// whatever a seat's tools leave in their temp directory (a Node compile
// cache, a tsx cache, harness probe files) is deleted with the session
// instead of piling up in the host's shared per-user temp, which macOS
// empties at boot before any user process may start.
//
// Where the directory comes from. The launch path resolves it once, from
// the env the worker will actually exec with — after the embedder's
// OnPreSpawn hook, because that hook is where an embedding product
// delivers its per-session scope env:
//
//  1. Declared: DONMAI_SESSION_TMPDIR=<absolute path> in that final env.
//     It may come from the work item's env or from OnPreSpawn; the hook
//     runs last, so its value wins. A declared value always wins over the
//     derived default below. An inherited copy of the key never counts,
//     on any layer: the daemon's own process env and BaseEnv (one map for
//     every session, which a standalone embedder seeds from a snapshot of
//     its process env) both drop it, because it names whatever session
//     the daemon itself was started inside, not this one.
//  2. Derived: with nothing declared and SpawnerOptions.SessionTmpDerivedPrefix
//     set (the daemon sets DefaultSessionTmpDerivedPrefix unless its
//     sessionScratch.derive setting is off), the launch
//     derives /tmp/<prefix><12 random hex digits> and states it to the
//     worker as DONMAI_SESSION_TMPDIR, so the session sees what it was
//     given. The name is random rather than session-derived: it cannot
//     be squatted ahead of the launch, and two daemons running the same
//     session id never share one.
//  3. None: derivation disabled and nothing declared. The worker inherits
//     the daemon's own TMPDIR, TMP and TEMP unchanged.
//
// An orchestrator stamps only the advisory key, never TMPDIR, TMP or TEMP:
// those would reach the worker before any directory exists.
//
// The executor (this spawner's launch path) validates the value and
// creates the directory (mode 0700, owned by the seat user) before the
// worker process starts, then binds TMPDIR, TMP and TEMP to it for the
// worker and every child it spawns. Creation failure fails the launch
// closed: it never falls back to the shared host temp.
//
// Path length. A unix socket path must fit sun_path: 104 bytes on macOS
// with the terminating NUL, 108 on Linux. Tools bind sockets under TMPDIR
// (tsx's IPC pipe is TMPDIR/tsx-<uid>/<pid>.pipe), so an over-long scratch
// path breaks them far from the cause. Every directory is held to
// sessionTmpMaxPathLen bytes — the macOS cap, less the NUL and
// sessionTmpSocketHeadroom bytes for the socket's own name below it — and
// a longer value refuses the launch with an error naming both lengths.
// The derived default is 24 bytes long.
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
// A launch whose cleaned path is already claimed by another live session,
// or whose directory still carries another session's ownership marker (the
// same directory under another spelling, or a live adopted session this
// process never claimed after a restart), is refused fail-closed: the
// newer launch never reuses a live session's directory, so ending one
// session can never delete another live session's files. A duplicated
// stamp is outside the contract, which derives a per-session path.

import (
	"crypto/rand"
	"encoding/hex"
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

// SessionTmpDirEnv is the advisory key that declares a seat's scratch
// directory, whether an orchestrator stamped it on the work item, the
// embedder's OnPreSpawn hook supplied it, or the daemon derived it.
// Advisory: the executor validates it before trusting it. Orchestrators
// never stamp TMPDIR, TMP or TEMP (those would reach the worker before any
// directory exists).
const SessionTmpDirEnv = "DONMAI_SESSION_TMPDIR"

// DefaultSessionTmpDerivedPrefix is the name prefix the daemon gives the
// scratch directory of a session that declares none:
// /tmp/donmai-<12 random hex digits>. See SpawnerOptions.SessionTmpDerivedPrefix.
const DefaultSessionTmpDerivedPrefix = "donmai-"

// sessionTmpDerivedRandomBytes is the entropy in a derived name: 6 bytes,
// rendered as 12 hex digits.
const sessionTmpDerivedRandomBytes = 6

// Path-length budget (see the file header). unixSocketPathMax is the
// macOS sun_path size, the strictest of the supported platforms;
// sessionTmpSocketHeadroom is what a socket name below the scratch
// directory may use, which covers tsx (tsx-<uid>/<pid>.pipe), tmux
// (tmux-<uid>/default), ssh-agent (ssh-XXXXXXXXXX/agent.<pid>) and a
// Postgres socket (.s.PGSQL.<port>).
const (
	unixSocketPathMax        = 104
	sessionTmpSocketHeadroom = 40
	sessionTmpMaxPathLen     = unixSocketPathMax - 1 - sessionTmpSocketHeadroom
)

// sessionTmpBindingKeys are the variables bound to the session scratch
// directory for the worker and every child it spawns.
var sessionTmpBindingKeys = []string{"TMPDIR", "TMP", "TEMP"}

// Sources of a launch's scratch directory, reported in the bind log line
// and in refusals.
const (
	sessionTmpSourceDeclared = "declared"
	sessionTmpSourceDerived  = "derived"
)

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

// validateSessionTmpLaunchValue is validateSessionTmpDir plus the
// path-length budget a launch must meet. The startup sweep validates
// without the budget, so it can still reclaim a recorded directory that
// predates it.
func validateSessionTmpLaunchValue(raw string) (string, error) {
	dir, err := validateSessionTmpDir(raw)
	if err != nil {
		return "", err
	}
	if len(dir) > sessionTmpMaxPathLen {
		return "", fmt.Errorf("session tmpdir: %q is %d bytes, over the %d-byte limit that keeps a unix socket under it within the %d-byte socket path cap (refused)", dir, len(dir), sessionTmpMaxPathLen, unixSocketPathMax)
	}
	return dir, nil
}

// deriveSessionTmpDir names a fresh scratch directory for a session that
// declares none: /tmp/<prefix><12 random hex digits>. A failed entropy read
// fails the launch closed rather than falling back to a guessable name.
func deriveSessionTmpDir(prefix string) (string, error) {
	var b [sessionTmpDerivedRandomBytes]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("session tmpdir: derive a directory name: %w", err)
	}
	return "/tmp/" + prefix + hex.EncodeToString(b[:]), nil
}

// declaredSessionTmpDir reports the value the final spawn env declares for
// the advisory key. The last entry wins, as it does for exec.
func declaredSessionTmpDir(env []string) string {
	prefix := SessionTmpDirEnv + "="
	value := ""
	for _, kv := range env {
		if strings.HasPrefix(kv, prefix) {
			value = kv[len(prefix):]
		}
	}
	return value
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
	dir, _, err := ensureSessionTmpDirCreated(raw)
	return dir, err
}

// ensureSessionTmpDirCreated behaves as ensureSessionTmpDir and
// additionally reports whether this call created the directory. Callers
// roll back only what they created: a reused directory is never removed on
// a later bookkeeping failure.
//
// sessionTmpAfterCreateHookForTest, when non-nil, runs between creation and
// the re-inspection below. Tests use it to sabotage the fresh directory
// (for example chmod it off 0700) so the production verification and the
// production created-rollback beneath it run against a real failure. It
// must only mutate the filesystem; it never decides the verdict. It is nil
// in production.
var sessionTmpAfterCreateHookForTest func(dir string)

func ensureSessionTmpDirCreated(raw string) (string, bool, error) {
	dir, err := validateSessionTmpDir(raw)
	if err != nil {
		return "", false, err
	}
	created := false
	fi, err := os.Lstat(dir)
	if err != nil {
		if !os.IsNotExist(err) {
			return "", false, fmt.Errorf("session tmpdir: inspect %q: %w", dir, err)
		}
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return "", false, fmt.Errorf("session tmpdir: create %q: %w", dir, err)
		}
		if err := os.Chmod(dir, 0o700); err != nil { //nolint:gosec // G302: restoring the exact 0700 the reuse check demands after a umask-weakened MkdirAll.
			_ = os.Remove(dir)
			return "", false, fmt.Errorf("session tmpdir: secure %q: %w", dir, err)
		}
		if sessionTmpAfterCreateHookForTest != nil {
			sessionTmpAfterCreateHookForTest(dir)
		}
		fi, err = os.Lstat(dir)
		if err != nil {
			return "", false, fmt.Errorf("session tmpdir: re-inspect %q: %w", dir, err)
		}
		created = true
	}
	if err := verifySessionTmpExisting(dir, fi, os.Getuid()); err != nil {
		if created {
			// Just created and already failing its own checks (nothing
			// else could have written here yet): Remove deletes only an
			// empty directory, so a reused directory is never at risk.
			_ = os.Remove(dir)
		}
		return "", false, err
	}
	return dir, created, nil
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
// the generation. A cleanup removes the directory only when the marker still
// names its own claim, so a stale generation never deletes its session's
// live generation's directory. A launch stamped with another session's path
// is refused before stamping (see prepareSessionTmpDir), so cross-session
// sharing never reaches this fence: the marker proves same-session
// generations, the refusal guards cross-session duplicates.
type sessionTmpOwnerRecord struct {
	SessionID string `json:"sessionId"`
	Gen       uint64 `json:"gen"`
}

// readSessionTmpOwnerRecord reports the ownership marker inside dir. A
// missing marker is no owner: the caller proceeds. Any other read
// failure, or a marker that is present but does not decode to a named
// session, fails closed with an error: the caller refuses the launch. A
// transient read error must never let a second session take over a live
// directory whose marker it cannot read.
func readSessionTmpOwnerRecord(dir string) (sessionTmpOwnerRecord, bool, error) {
	marker := filepath.Join(dir, sessionTmpOwnerFile) //nolint:gosec // G304: dir is a validated /tmp child and the name a constant.
	if _, err := os.Lstat(marker); err != nil {
		if os.IsNotExist(err) {
			return sessionTmpOwnerRecord{}, false, nil
		}
		return sessionTmpOwnerRecord{}, false, fmt.Errorf("session tmpdir: inspect owner for %q: %w", dir, err)
	}
	raw, err := os.ReadFile(marker) //nolint:gosec // G304: dir is a validated /tmp child and the name a constant.
	if err != nil {
		if os.IsNotExist(err) {
			return sessionTmpOwnerRecord{}, false, nil
		}
		return sessionTmpOwnerRecord{}, false, fmt.Errorf("session tmpdir: read owner for %q: %w", dir, err)
	}
	var record sessionTmpOwnerRecord
	if err := json.Unmarshal(raw, &record); err != nil {
		return sessionTmpOwnerRecord{}, false, fmt.Errorf("session tmpdir: decode owner for %q: %w", dir, err)
	}
	if record.SessionID == "" {
		return sessionTmpOwnerRecord{}, false, fmt.Errorf("session tmpdir: owner for %q names no session (refused)", dir)
	}
	return record, true, nil
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

// applySessionTmpBindings returns env with the three scratch bindings and
// the advisory key set to dir. Any prior entry for those keys is dropped
// first, so each is bound exactly once however the worker was otherwise
// configured, and a session given a derived directory sees it named in
// DONMAI_SESSION_TMPDIR just as a declared one does.
func applySessionTmpBindings(env []string, dir string) []string {
	keys := append(append([]string(nil), sessionTmpBindingKeys...), SessionTmpDirEnv)
	// Capacity from env alone: the four bound keys append past it at most
	// once, and an added length would read as an allocation-size overflow.
	out := make([]string, 0, len(env))
dropped:
	for _, kv := range env {
		key := kv
		if i := strings.IndexByte(kv, '='); i >= 0 {
			key = kv[:i]
		}
		for _, bound := range keys {
			if key == bound {
				continue dropped
			}
		}
		out = append(out, kv)
	}
	for _, key := range keys {
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
	// PID is the daemon process that recorded the entry. The startup sweep
	// reclaims what a previous daemon process left behind, so it keeps an
	// entry this very process wrote: that entry belongs to a live spawner
	// in this process (another daemon sharing the state home), which
	// removes it itself. Zero — an entry written before the field existed
	// — always counts as a previous process's.
	PID int `json:"pid,omitempty"`
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

// workItemSessionTmpDir reports the scratch directory a work item's own env
// declares, after the host-owned filter the spawn composition applies.
func workItemSessionTmpDir(spec SessionSpec) string {
	return runtimeenv.FilterHostOwnedMap(spec.Env)[SessionTmpDirEnv]
}

// bindSessionTmpDir resolves one launch's scratch directory from the env
// the worker will exec with — after OnPreSpawn, so a value the embedder
// delivers there counts — prepares it, and returns the claim plus that env
// with TMPDIR, TMP, TEMP and the advisory key bound to the directory. A
// declared value wins; with none, the configured prefix derives one; with
// neither, the zero claim and the env come back unchanged. Every error
// fails the launch closed before any worker process starts.
func (s *WorkerSpawner) bindSessionTmpDir(spec SessionSpec, env []string) (sessionTmpClaim, []string, error) {
	raw, source := declaredSessionTmpDir(env), sessionTmpSourceDeclared
	if raw == "" {
		if s.opts.SessionTmpDerivedPrefix == "" {
			return sessionTmpClaim{}, env, nil
		}
		derived, err := deriveSessionTmpDir(s.opts.SessionTmpDerivedPrefix)
		if err != nil {
			return sessionTmpClaim{}, env, err
		}
		raw, source = derived, sessionTmpSourceDerived
	}
	claim, err := s.prepareSessionTmpDir(spec.SessionID, raw)
	if err != nil {
		return sessionTmpClaim{}, env, fmt.Errorf("%s scratch directory for session %q: %w", source, spec.SessionID, err)
	}
	slog.Info("session scratch directory bound", "sessionId", spec.SessionID, "dir", claim.Dir, "source", source)
	return claim, applySessionTmpBindings(env, claim.Dir), nil
}

// prepareSessionTmpDir validates a launch's scratch-directory value (shape
// and path-length budget), ensures the directory, stamps ownership and
// records the creation. Every error fails the launch closed before any
// worker process starts.
func (s *WorkerSpawner) prepareSessionTmpDir(sessionID, raw string) (sessionTmpClaim, error) {
	if _, err := validateSessionTmpLaunchValue(raw); err != nil {
		return sessionTmpClaim{}, err
	}
	dir, created, err := ensureSessionTmpDirCreated(raw)
	if err != nil {
		return sessionTmpClaim{}, err
	}
	gen := nextSessionTmpGen.Add(1)
	if gen == 0 {
		gen = nextSessionTmpGen.Add(1)
	}
	claim := sessionTmpClaim{Dir: dir, Gen: gen}
	// removeOwn rolls back only a directory this launch created, and only
	// while the ownership marker still names this generation. A reused
	// directory (the prior turn's files) is never removed: the launch
	// already fails closed with an error, and wiping on a bookkeeping
	// failure would destroy data that is not ours to delete.
	removeOwn := func() {
		if created && sessionTmpOwnerMatches(dir, claim) {
			_ = os.RemoveAll(dir)
		}
	}
	// Serialize the duplicate-path check, the ownership stamp and the live
	// claim insert under mu so two concurrent launches stamping the same
	// path cannot both succeed: the second blocks here until the first
	// inserts, then sees the live claim and refuses before touching the
	// directory's marker. A duplicated stamp (outside the contract, which
	// derives a per-session path) fails closed naming both sessions, so
	// ending one session can never delete another live session's files.
	// The cleaned-string scan above cannot see a live session holding the
	// same directory under another spelling (the macOS /tmp alias, a case
	// variant on a case-insensitive volume) or a live adopted session this
	// process never claimed (after a restart, only the record and the
	// marker name it). The marker lives in the directory itself, so every
	// spelling reads the same owner: a marker naming another session
	// refuses this launch before the stamp is overwritten. A marker naming
	// this session (a continued turn) proceeds to a fresh generation. A
	// marker that is present but unreadable or undecodable also refuses
	// the launch fail-closed: the second session must never take over a
	// directory whose owner it cannot prove.
	s.mu.Lock()
	for id, existing := range s.sessionTmpDirs {
		if id != sessionID && existing.owned() && existing.Dir == dir {
			s.mu.Unlock()
			if created {
				_ = os.Remove(dir)
			}
			return sessionTmpClaim{}, fmt.Errorf("session tmpdir: %q already claimed by live session %q (session %q refused)", dir, id, sessionID)
		}
	}
	if owner, hasOwner, ownerErr := readSessionTmpOwnerRecord(dir); ownerErr != nil {
		s.mu.Unlock()
		if created {
			_ = os.Remove(dir)
		}
		return sessionTmpClaim{}, fmt.Errorf("session tmpdir: %q ownership marker unreadable (session %q refused): %w", dir, sessionID, ownerErr)
	} else if hasOwner && owner.SessionID != sessionID {
		s.mu.Unlock()
		if created {
			_ = os.Remove(dir)
		}
		return sessionTmpClaim{}, fmt.Errorf("session tmpdir: %q already owned by session %q (session %q refused)", dir, owner.SessionID, sessionID)
	}
	if err := writeSessionTmpOwner(dir, sessionID, gen); err != nil {
		s.mu.Unlock()
		if created {
			_ = os.RemoveAll(dir)
		}
		return sessionTmpClaim{}, err
	}
	if s.sessionTmpDirs == nil {
		s.sessionTmpDirs = map[string]sessionTmpClaim{}
	}
	s.sessionTmpDirs[sessionID] = claim
	recordDir := s.opts.SessionTmpRecordDir
	s.mu.Unlock()
	if recordDir != "" {
		s.sessionTmpRecordMu.Lock()
		record, err := loadSessionTmpRecord(recordDir)
		if err != nil {
			s.sessionTmpRecordMu.Unlock()
			s.forgetSessionTmpClaim(sessionID, claim)
			removeOwn()
			return sessionTmpClaim{}, err
		}
		record.Entries[sessionID] = sessionTmpRecordEntry{Path: dir, CreatedAt: time.Now().UTC().Format(time.RFC3339), PID: os.Getpid()}
		err = saveSessionTmpRecord(recordDir, record)
		s.sessionTmpRecordMu.Unlock()
		if err != nil {
			s.forgetSessionTmpClaim(sessionID, claim)
			removeOwn()
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
// record directory (memory-only mode) makes it a no-op. The read-modify-save
// runs under the record mutex so a reaper cleanup overlapping a launch
// cannot overwrite the launch's entry.
func (s *WorkerSpawner) forgetSessionTmpRecordEntry(sessionID string) {
	recordDir := s.opts.SessionTmpRecordDir
	if recordDir == "" {
		return
	}
	s.sessionTmpRecordMu.Lock()
	defer s.sessionTmpRecordMu.Unlock()
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
	// running session, or recorded by this process itself.
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
	// Snapshot liveness before taking the record lock: prepares and
	// cleanups take mu briefly for the claim map and the record lock only
	// for the file, never nested, so snapshotting first keeps the lock
	// order flat and the sweep never blocks a launch on a directory
	// removal.
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
	s.sessionTmpRecordMu.Lock()
	defer s.sessionTmpRecordMu.Unlock()
	record, err := loadSessionTmpRecord(recordDir)
	if err != nil {
		slog.Warn("session scratch sweep: unreadable record, reclaiming nothing", "err", err)
		return report
	}
	// An entry is live when its session is, or when this very process
	// recorded it: the sweep reclaims what a previous daemon process left
	// behind, and an entry of this process's own belongs to a spawner here
	// that removes it itself (see sessionTmpRecordEntry.PID).
	self := os.Getpid()
	isLive := func(sessionID string, entry sessionTmpRecordEntry) bool {
		_, live := protected[sessionID]
		return live || (entry.PID != 0 && entry.PID == self)
	}
	// Protected paths: a live entry's path is never deleted, even by a
	// dead entry naming the same path (a duplicated stamp). A dead entry
	// sharing a live path is dropped without deleting.
	protectedPaths := map[string]struct{}{}
	for sessionID, entry := range record.Entries {
		if isLive(sessionID, entry) && entry.Path != "" {
			protectedPaths[entry.Path] = struct{}{}
		}
	}
	changed := false
	for sessionID, entry := range record.Entries {
		report.Examined++
		if isLive(sessionID, entry) {
			report.KeptLive++
			continue
		}
		if _, shielded := protectedPaths[entry.Path]; shielded {
			delete(record.Entries, sessionID)
			changed = true
			report.Dropped++
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
// Live adopted and quarantined lineages (known because adoption ran earlier
// in Start) keep their directories: a quarantined lineage keeps its shim
// alive by design, so its worker's live scratch must survive the sweep.
// It never fails startup.
func (d *Daemon) sweepSessionTmpDirs() {
	if d.spawner == nil {
		return
	}
	live := map[string]struct{}{}
	for _, id := range d.AdoptedSessionShims() {
		live[id.SessionID] = struct{}{}
	}
	for _, q := range d.QuarantinedSessions() {
		live[q.SessionID] = struct{}{}
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
