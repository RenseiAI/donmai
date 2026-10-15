package claude

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/RenseiAI/donmai/agent"
)

// This file pre-seeds the CLI's workspace-trust record for unattended
// interactive sessions.
//
// The interactive REPL parks on a workspace trust modal for any cwd the CLI
// has not recorded as trusted, and there is no flag that skips it —
// headless `-p` runs skip the dialog, interactive TTY runs do not. An
// unattended session that cannot answer the modal never reaches its first
// turn, so the harness records trust for exactly the platform-authorized
// boundary before the child starts.
//
// Trust lives in the CLI's own config file (`.claude.json` under the config
// home) as `projects[<resolved-cwd>].hasTrustDialogAccepted`. The seed is
// add-only and deduplicated: it never removes another session's entry (no
// read-modify-write restore race between concurrent sessions) and unknown
// file fields are preserved. The CLI keys trust on the RESOLVED cwd, so
// both the unresolved and the resolved spellings are seeded. Paths outside
// the authorized boundary are never seeded: the boundary is the session's
// own cwd plus its declared mutable repository paths
// (RepositoryAuthority.MutablePaths), which the runner authors — never
// caller-supplied extras, never parents, never siblings.
//
// Best-effort throughout: the file may be absent, malformed, or unwritable,
// and the config home may be redirected by CLAUDE_CONFIG_DIR. Any failure
// leaves the session to the modal rather than failing the spawn — the modal
// is the honest signal, a spawn failure would be a lie about what happened.

// seededTrust records what one seedWorkspaceTrust call changed, so the
// session-end cleanup can undo exactly that and nothing else. A directory
// that was already trusted before the seed is in neither list: that trust
// belongs to someone else (the operator, or another live session).
//
// session identifies the owning session for the on-disk ledger
// (trust_ledger.go): it lets a later daemon sweep tell this live session's
// entries from a crashed session's, and it is the only per-session identity
// the ledger carries.
type seededTrust struct {
	// path is the trust record the seed wrote.
	path string
	// dirs are the entries the seed CREATED (absent before the seed); the
	// cleanup removes them whole.
	dirs []string
	// flipped are entries that existed before the seed WITHOUT trust (for
	// example the operator's own per-project settings for that directory).
	// The cleanup puts only the trust key back to its prior state — prior
	// holds the previous value; a directory missing from prior had no key —
	// and leaves every other field on the entry as found.
	flipped []string
	prior   map[string]any
	// session identifies the owning session (agent.Spec.SessionID, or a
	// process-scoped fallback when the session carries no lifecycle id).
	// Empty means the
	// seed predates the ledger, or the session declined ownership — the
	// cleanup still removes exactly dirs/flipped, but the daemon crash
	// sweep cannot re-home the entry to a live session.
	session string
}

// seedWorkspaceTrust records trust for exactly the session's authorized
// boundary. It never returns an error: every failure mode is silent, and
// the session proceeds to the modal it would have seen without the seed.
// The returned record names what was seeded so the caller can remove those
// entries — and only those — at session end (see removeSeededTrust); nil
// means nothing was seeded and there is nothing to clean up.
func seedWorkspaceTrust(spec agent.Spec) *seededTrust {
	dirs := trustBoundaryDirs(spec)
	if len(dirs) == 0 {
		return nil
	}
	home, err := claudeConfigHome(spec.Env)
	if err != nil {
		return nil
	}
	seed := seedTrustFile(filepath.Join(home, claudeTrustFileName), dirs)
	if seed == nil {
		return nil
	}
	seed.session = trustLedgerOwner(spec)
	recordSeededTrustLedger(seed)
	return seed
}

// removeSeededTrust undoes what seed changed at session end: entries the
// seed created are removed whole, and entries it flipped get only their
// trust key put back to its prior state. Pre-existing trust — the
// operator's own, other sessions' seeds, even at the session's own path —
// is never in the seed record and is left untouched, as is a record that can no longer be read or parsed
// (it is not ours to rewrite) or whose projects field has an unexpected
// shape. Every failure is silent: cleanup must never fail a session that
// already ran. A nil seed is a no-op.
func removeSeededTrust(seed *seededTrust) {
	if seed == nil || seed.path == "" || (len(seed.dirs) == 0 && len(seed.flipped) == 0) {
		return
	}
	defer forgetSeededTrustLedger(seed)
	release, ok := acquireTrustLock(seed.path)
	if !ok {
		return
	}
	defer release()
	data, err := os.ReadFile(seed.path) //nolint:gosec // config path, not session input
	if err != nil {
		return
	}
	var record map[string]any
	if json.Unmarshal(data, &record) != nil {
		return
	}
	raw := record[claudeTrustProjectsKey]
	if raw == nil {
		return
	}
	projects, ok := raw.(map[string]any)
	if !ok {
		return
	}
	for _, dir := range seed.dirs {
		delete(projects, dir)
	}
	for _, dir := range seed.flipped {
		entry, ok := projects[dir].(map[string]any)
		if !ok {
			continue
		}
		if prev, had := seed.prior[dir]; had {
			entry[claudeTrustAcceptedKey] = prev
		} else {
			delete(entry, claudeTrustAcceptedKey)
		}
	}
	body, err := json.Marshal(record)
	if err != nil {
		return
	}
	tmp, err := os.CreateTemp(filepath.Dir(seed.path), ".claude.json.tmp-*")
	if err != nil {
		return
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(body); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
		return
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpName)
		return
	}
	if err := os.Rename(tmpName, seed.path); err != nil {
		_ = os.Remove(tmpName)
	}
}

// trustBoundaryDirs returns the exact directories trust may be seeded for:
// the session cwd plus its declared mutable repository paths, each
// canonicalized to an absolute path with its symlink-resolved twin. Empty
// entries, non-absolute results, and duplicates are dropped. The returned
// paths are never validated against the filesystem: the worktree may not
// exist yet at seed time on every lane, and trust is a record about where
// the session WILL run, not a statement that it already does.
func trustBoundaryDirs(spec agent.Spec) []string {
	var raw []string
	if spec.Cwd != "" {
		raw = append(raw, spec.Cwd)
	}
	if policy := spec.RepositoryAuthority; policy != nil {
		raw = append(raw, policy.MutablePaths...)
	}
	seen := make(map[string]struct{}, len(raw)*2)
	homes := operatorHomes()
	var out []string
	for _, dir := range raw {
		if strings.TrimSpace(dir) == "" {
			continue
		}
		abs, err := filepath.Abs(dir)
		if err != nil || !filepath.IsAbs(abs) {
			continue
		}
		cleaned := filepath.Clean(abs)
		for _, candidate := range []string{cleaned, resolveTrustPath(cleaned)} {
			if candidate == "" || tooBroadForTrust(candidate, homes) {
				continue
			}
			if _, dup := seen[candidate]; dup {
				continue
			}
			seen[candidate] = struct{}{}
			out = append(out, candidate)
		}
	}
	return out
}

// resolveTrustPath returns the symlink-resolved spelling of dir, or "" when
// it cannot be resolved (the path does not exist yet, or resolution fails).
// Callers always keep the unresolved spelling too: resolution may fail at
// seed time while the child — running after the worktree exists — resolves
// successfully, and the seed must cover the spelling the child checks.
func resolveTrustPath(dir string) string {
	resolved, err := filepath.EvalSymlinks(dir)
	if err != nil || resolved == "" || resolved == dir {
		return ""
	}
	return filepath.Clean(resolved)
}

// operatorHomes returns the operator's home directory in its cleaned and
// symlink-resolved spellings (either may be absent).
func operatorHomes() []string {
	home, err := os.UserHomeDir()
	if err != nil || strings.TrimSpace(home) == "" || !filepath.IsAbs(home) {
		return nil
	}
	cleaned := filepath.Clean(home)
	return []string{cleaned, resolveTrustPath(cleaned)}
}

// tooBroadForTrust reports whether dir is a root the seed must never record:
// the filesystem root, or the operator's home directory or any ancestor of
// it. The CLI's trust lookup honors an ancestor's entry for a cwd outside a
// repository, and the CLI itself never persists trust for the home
// directory (home trust is session-only), so a persisted entry at one of
// these roots would silently trust every directory beneath it for every
// later session on the host — a person's own included.
func tooBroadForTrust(dir string, homes []string) bool {
	if filepath.Dir(dir) == dir {
		return true
	}
	fold := runtime.GOOS == "darwin" || runtime.GOOS == "windows"
	for _, home := range homes {
		if home == "" {
			continue
		}
		d, h := dir, home
		if fold {
			d, h = strings.ToLower(d), strings.ToLower(h)
		}
		if d == h || strings.HasPrefix(h, d+string(filepath.Separator)) {
			return true
		}
	}
	return false
}

// claudeConfigHome returns the directory holding the CLI's trust record —
// the one the child will read: CLAUDE_CONFIG_DIR from the session's own env
// layer when set (the child env is that layer over the parent's), else the
// parent process's, otherwise the operator's home. A relative override is an
// error the caller treats as "do not seed": the CLI resolves its own config
// home by its own rules, so resolving a relative override against the
// worker process cwd would seed the wrong home — a seed that silently does
// nothing while the session still hits the modal.
func claudeConfigHome(sessionEnv map[string]string) (string, error) {
	override := sessionEnv[claudeConfigDirEnv]
	if override == "" {
		override = os.Getenv(claudeConfigDirEnv)
	}
	if override != "" {
		if !filepath.IsAbs(override) {
			return "", fmt.Errorf("relative %s %q: refusing to seed outside the CLI's own config home", claudeConfigDirEnv, override)
		}
		return override, nil
	}
	return os.UserHomeDir()
}

// claudeConfigDirEnv names the config-home redirect the CLI honors.
const claudeConfigDirEnv = "CLAUDE_CONFIG_DIR"

// claudeTrustFileName is the CLI's trust-record basename under the config
// home.
const claudeTrustFileName = ".claude.json"

// seedTrustFile adds dirs to the trust record at path, creating the file
// when absent. A record that cannot be read or parsed is left untouched (no
// seed); unknown fields are preserved; existing trust entries are kept.
// Every failure is silent. The returned record names exactly the entries
// this call created or flipped (nil when it wrote nothing), so the session-
// end cleanup never removes trust the seed did not add.
//
// The read-modify-write merges under a sibling lock directory, so
// concurrent sessions sharing one config home cannot lose each other's
// entries: without the lock the loser of the race rewrites the winner's
// seed away and parks on the workspace-trust modal. The final write rides
// a temp-file rename, so a concurrent reader never sees a half-written
// record.
func seedTrustFile(path string, dirs []string) *seededTrust {
	if path == "" || len(dirs) == 0 {
		return nil
	}
	release, ok := acquireTrustLock(path)
	if !ok {
		return nil
	}
	defer release()
	var record map[string]any
	data, err := os.ReadFile(path) //nolint:gosec // config path, not session input
	switch {
	case err == nil:
		// The record is the CLI's whole config, not just trust: a file that
		// does not parse (truncated, mid-write, hand-edited) is left exactly
		// as found rather than replaced by a trust-only record that would
		// drop every other field. The session meets the modal instead.
		if json.Unmarshal(data, &record) != nil {
			return nil
		}
	case errors.Is(err, os.ErrNotExist):
		// absent → create below
	default:
		return nil // unreadable: never overwrite what cannot be read
	}
	if record == nil {
		record = make(map[string]any)
	}
	var projects map[string]any
	if raw, ok := record[claudeTrustProjectsKey]; ok && raw != nil {
		if projects, ok = raw.(map[string]any); !ok {
			return nil // an unexpected shape is not ours to replace
		}
	}
	if projects == nil {
		projects = make(map[string]any)
	}
	seed := &seededTrust{path: path}
	for _, dir := range dirs {
		raw, existed := projects[dir]
		entry, isMap := raw.(map[string]any)
		switch {
		case !existed || raw == nil:
			entry = make(map[string]any)
			seed.dirs = append(seed.dirs, dir)
		case !isMap:
			continue // an unexpected shape is not ours to replace
		case entry[claudeTrustAcceptedKey] == true:
			continue // already trusted: someone else's trust, never ours to remove
		default:
			if prev, had := entry[claudeTrustAcceptedKey]; had {
				if seed.prior == nil {
					seed.prior = make(map[string]any)
				}
				seed.prior[dir] = prev
			}
			seed.flipped = append(seed.flipped, dir)
		}
		entry[claudeTrustAcceptedKey] = true
		projects[dir] = entry
	}
	if len(seed.dirs) == 0 && len(seed.flipped) == 0 {
		return nil // nothing to add, nothing to write, nothing to clean up
	}
	record[claudeTrustProjectsKey] = projects
	body, err := json.Marshal(record)
	if err != nil {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".claude.json.tmp-*")
	if err != nil {
		return nil
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(body); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
		return nil
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpName)
		return nil
	}
	if err := os.Rename(tmpName, path); err != nil {
		_ = os.Remove(tmpName)
		return nil
	}
	return seed
}

// Lock interop: the CLI itself reads and writes this same record — it
// records trust answers the operator gives at the terminal, and it rewrites
// the whole file when it does. The seed cannot stop the CLI from writing
// between the seed's own read and write, and the CLI does not take this
// lock — so the merge is optimistic: it re-reads under the lock, applies
// only its own entries, and writes through a temp-file rename, so the
// worst case is a lost CLI-side update landing in the same window, never a
// half-written record. The temp-file rename additionally keeps the CLI's
// own concurrent reader from ever seeing a half-written record, and the
// lock keeps every harness-side writer (concurrent seeds, live-path
// cleanups, ledger rows, daemon crash sweeps) serialized with each other.
// A per-session config home would remove the sharing entirely, but the CLI
// resolves its home by its own rules (claudeConfigHome) and the child must
// read the home the seed wrote — so the lock, not isolation, is the
// coordination.
//
// Trust-lock tuning: the seed holds the lock for one small read plus one
// small write (milliseconds), so a lock older than trustLockStaleAfter can
// only belong to a dead holder. Bounded retries keep a contended spawn
// from stalling: losing the race skips the seed and the modal says so.
const (
	trustLockSuffix     = ".lock.d"
	trustLockStaleAfter = 30 * time.Second
	trustLockRetries    = 100
	trustLockRetryWait  = 10 * time.Millisecond
)

// acquireTrustLock serializes trust-record merges across every session
// sharing the config home, in-process and cross-process: directory
// creation is atomic, so exactly one contender wins the Mkdir. The
// returned release removes the lock directory; ok is false when the lock
// cannot be taken in time or the parent cannot be prepared — both silent
// under the seed's best-effort contract.
func acquireTrustLock(path string) (release func(), ok bool) {
	lockDir := path + trustLockSuffix
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, false
	}
	for range trustLockRetries {
		if err := os.Mkdir(lockDir, 0o700); err == nil {
			stampTrustLock(lockDir)
			return func() { _ = os.RemoveAll(lockDir) }, true
		} else if !errors.Is(err, os.ErrExist) {
			return nil, false
		}
		if trustLockIsStale(lockDir) {
			_ = os.RemoveAll(lockDir)
			continue
		}
		time.Sleep(trustLockRetryWait)
	}
	return nil, false
}

// stampTrustLock records when this holder took the lock, so a contender
// can tell a live merge (milliseconds old) from a dead holder's residue.
// Best-effort: an unstamped lock simply never reads as stale.
func stampTrustLock(lockDir string) {
	_ = os.WriteFile( //nolint:gosec // harness-owned lock dir, fixed filename
		filepath.Join(lockDir, "holder"),
		[]byte(time.Now().UTC().Format(time.RFC3339Nano)),
		0o600,
	)
}

// trustLockIsStale reports whether the lock directory belongs to a holder
// that cannot still be merging: a positively old stamp, or — when the stamp
// is missing or unparseable (a holder that died between taking the lock and
// stamping it) — a lock directory that is itself old. A lock that cannot be
// inspected at all reads as live.
func trustLockIsStale(lockDir string) bool {
	raw, err := os.ReadFile(filepath.Join(lockDir, "holder")) //nolint:gosec // harness-owned lock dir, fixed filename
	if err == nil {
		if stamped, perr := time.Parse(time.RFC3339Nano, strings.TrimSpace(string(raw))); perr == nil {
			return time.Since(stamped) > trustLockStaleAfter
		}
	}
	info, err := os.Stat(lockDir)
	if err != nil {
		return false
	}
	return time.Since(info.ModTime()) > trustLockStaleAfter
}

// claudeTrustProjectsKey / claudeTrustAcceptedKey are the trust-record
// field names the CLI reads: projects[<cwd>].hasTrustDialogAccepted.
const (
	claudeTrustProjectsKey = "projects"
	claudeTrustAcceptedKey = "hasTrustDialogAccepted"
)
