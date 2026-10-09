package claude

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"

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
// CLAUDE_CONFIG_DIR when set (absolute, or resolved against the cwd),
// otherwise the operator's home. An empty or unusable home is an error the
// caller treats as "do not seed".
func claudeConfigHome() (string, error) {
	if override := os.Getenv(claudeConfigDirEnv); override != "" {
		if !filepath.IsAbs(override) {
			abs, err := filepath.Abs(override)
			if err != nil {
				return "", err
			}
			override = abs
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
func seedTrustFile(path string, dirs []string) {
	if path == "" || len(dirs) == 0 {
		return
	}
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
	//nolint:gosec // 0600 owner-only; the record holds no secrets but the CLI's own file is 0600
	_ = os.WriteFile(path, body, 0o600)
}

// claudeTrustProjectsKey / claudeTrustAcceptedKey are the trust-record
// field names the CLI reads: projects[<cwd>].hasTrustDialogAccepted.
const (
	claudeTrustProjectsKey = "projects"
	claudeTrustAcceptedKey = "hasTrustDialogAccepted"
)
