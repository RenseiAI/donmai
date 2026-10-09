package claude

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
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

// seedWorkspaceTrust records trust for exactly the session's authorized
// boundary. It never returns an error: every failure mode is silent, and
// the session proceeds to the modal it would have seen without the seed.
func seedWorkspaceTrust(spec agent.Spec) {
	dirs := trustBoundaryDirs(spec)
	if len(dirs) == 0 {
		return
	}
	home, err := claudeConfigHome()
	if err != nil {
		return
	}
	seedTrustFile(filepath.Join(home, claudeTrustFileName), dirs)
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
			if candidate == "" {
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

// claudeConfigHome returns the directory holding the CLI's trust record:
// CLAUDE_CONFIG_DIR when set, otherwise the operator's home. A relative
// override is an error the caller treats as "do not seed": the CLI
// resolves its own config home by its own rules, so resolving a relative
// override against the worker process cwd would seed the wrong home — a
// seed that silently does nothing while the session still hits the modal.
func claudeConfigHome() (string, error) {
	if override := os.Getenv(claudeConfigDirEnv); override != "" {
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
// when absent. Malformed content starts fresh; unknown fields are
// preserved; existing trust entries are kept. Every failure is silent.
//
// The read-modify-write merges under a sibling lock directory, so
// concurrent sessions sharing one config home cannot lose each other's
// entries: without the lock the loser of the race rewrites the winner's
// seed away and parks on the workspace-trust modal. The final write rides
// a temp-file rename, so a concurrent reader never sees a half-written
// record.
func seedTrustFile(path string, dirs []string) {
	if path == "" || len(dirs) == 0 {
		return
	}
	release, ok := acquireTrustLock(path)
	if !ok {
		return
	}
	defer release()
	var record map[string]any
	if data, err := os.ReadFile(path); err == nil { //nolint:gosec // config path, not session input
		_ = json.Unmarshal(data, &record) // tolerate a malformed file → start fresh below
	}
	if record == nil {
		record = make(map[string]any)
	}
	var projects map[string]any
	if raw, ok := record[claudeTrustProjectsKey]; ok {
		projects, _ = raw.(map[string]any)
	}
	if projects == nil {
		projects = make(map[string]any)
	}
	for _, dir := range dirs {
		var entry map[string]any
		if raw, ok := projects[dir]; ok {
			entry, _ = raw.(map[string]any)
		}
		if entry == nil {
			entry = make(map[string]any)
		}
		entry[claudeTrustAcceptedKey] = true
		projects[dir] = entry
	}
	record[claudeTrustProjectsKey] = projects
	body, err := json.Marshal(record)
	if err != nil {
		return
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".claude.json.tmp-*")
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
	if err := os.Rename(tmpName, path); err != nil {
		_ = os.Remove(tmpName)
	}
}

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
// that cannot still be merging. Anything unreadable or unparseable reads
// as live: only a positively old stamp breaks the lock.
func trustLockIsStale(lockDir string) bool {
	raw, err := os.ReadFile(filepath.Join(lockDir, "holder")) //nolint:gosec // harness-owned lock dir, fixed filename
	if err != nil {
		return false
	}
	stamped, err := time.Parse(time.RFC3339Nano, strings.TrimSpace(string(raw)))
	if err != nil {
		return false
	}
	return time.Since(stamped) > trustLockStaleAfter
}

// claudeTrustProjectsKey / claudeTrustAcceptedKey are the trust-record
// field names the CLI reads: projects[<cwd>].hasTrustDialogAccepted.
const (
	claudeTrustProjectsKey = "projects"
	claudeTrustAcceptedKey = "hasTrustDialogAccepted"
)
