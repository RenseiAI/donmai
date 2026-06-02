// Package credentials provides the AF-TUI standalone credential source used
// when `af` runs outside of rensei-tui (no daemon credential pipeline, no
// platform session). Agents inherit credentials from the af process per the
// "Credentials in standalone mode" precedence ladder:
//
//  1. Existing environment variables in the af process (os.Environ())
//  2. ${gitRoot}/.env.local, parsed once at af startup
//  3. Fail-open with a redacted stderr warning ([creds] no source for KEY …)
//
// AGENT_ENV_BLOCKLIST is enforced before any value is forwarded into a
// child process; the canonical list lives in
// github.com/RenseiAI/donmai/internal/credentials. The blocklist
// keeps the daemon's own auth tokens (DONMAI_DAEMON_JWT, M2M_JWT_SECRET,
// WORKER_API_KEY, …) from leaking through to agents even when they appear
// in process env or a developer's .env.local.
//
// The LocalSource NEVER writes .env.local into the worktree — values stay
// in the AF-TUI process and are merged into the child env at spawn time
// only.
package credentials

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	creds "github.com/RenseiAI/donmai/internal/credentials"
)

// sourceLabel is the provenance tag returned by LocalSource.Resolve.
type sourceLabel string

const (
	// SourceProcess indicates the variable was inherited from
	// AF-TUI's os.Environ() at LoadLocalSource time.
	SourceProcess sourceLabel = "process"
	// SourceFile indicates the variable came from ${gitRoot}/.env.local.
	SourceFile sourceLabel = "file"
)

// LocalSource is the in-memory, immutable view of the standalone-mode
// credential surface. Construct via LoadLocalSource once at AF-TUI
// startup; do not mutate after construction.
//
// processEnv is the AF-TUI os.Environ() snapshot at load time.
// fileEnv is the parsed ${gitRoot}/.env.local (may be empty).
// sources maps each var name to its provenance for diagnostics.
type LocalSource struct {
	processEnv map[string]string
	fileEnv    map[string]string
	sources    map[string]sourceLabel

	// envLocalPath is the resolved path that was parsed (or attempted).
	// Empty when no path was attempted (gitRoot == "").
	envLocalPath string

	// stderr is the sink for redacted warning lines. Defaults to
	// os.Stderr; tests inject a buffer.
	stderr io.Writer
}

// LoadLocalSource captures the current process env and (if present)
// parses ${gitRoot}/.env.local into a LocalSource.
//
// gitRoot is the absolute path of the working directory's git root.
// When empty, no .env.local lookup is performed; only process env is
// captured. AF-TUI does NOT walk parent directories — secrets must not
// bleed across project boundaries when `af` is invoked from a nested
// repo.
//
// Errors only on truly unrecoverable conditions (an os.Open returning a
// non-NotExist error). A missing .env.local is normal and yields a
// LocalSource with empty fileEnv.
//
// Side effects: a single redacted stderr warning when .env.local is
// world-readable; per-malformed-line stderr notices (variable names
// only — values are never logged).
func LoadLocalSource(gitRoot string) (*LocalSource, error) {
	s := &LocalSource{
		processEnv: snapshotProcessEnv(),
		fileEnv:    map[string]string{},
		sources:    map[string]sourceLabel{},
		stderr:     os.Stderr,
	}

	if gitRoot != "" {
		s.envLocalPath = filepath.Join(gitRoot, ".env.local")
		if err := s.loadEnvLocal(s.envLocalPath); err != nil {
			return nil, fmt.Errorf("credentials: load .env.local: %w", err)
		}
	}

	// Build the provenance map: process env wins.
	for k := range s.processEnv {
		s.sources[k] = SourceProcess
	}
	for k := range s.fileEnv {
		if _, ok := s.sources[k]; !ok {
			s.sources[k] = SourceFile
		}
	}
	return s, nil
}

// snapshotProcessEnv copies os.Environ() into a map so LocalSource is
// stable even if the caller later mutates the host env (e.g. for
// per-command overrides).
func snapshotProcessEnv() map[string]string {
	env := os.Environ()
	out := make(map[string]string, len(env))
	for _, entry := range env {
		idx := strings.IndexByte(entry, '=')
		if idx < 0 {
			continue
		}
		out[entry[:idx]] = entry[idx+1:]
	}
	return out
}

// loadEnvLocal reads, validates permissions on, and parses the
// .env.local file at path. Missing-file is silent; other open errors
// propagate. Parse errors are logged (variable names only) and skipped.
func (s *LocalSource) loadEnvLocal(path string) error {
	info, err := os.Stat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}

	// World-readable bits — warn but don't refuse.
	if info.Mode().Perm()&0o044 != 0 {
		_, _ = fmt.Fprintf(s.stderr,
			"AF-TUI: %s is world-readable. Recommend `chmod 600`. Continuing.\n",
			path,
		)
	}

	f, err := os.Open(path) //nolint:gosec // path is gitRoot/.env.local, operator-owned
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()

	scanner := bufio.NewScanner(f)
	lineNo := 0
	for scanner.Scan() {
		lineNo++
		line := scanner.Text()
		k, v, ok := parseDotenvLine(line)
		if !ok {
			// Strip the value side so a redacted assignment doesn't leak.
			_, _ = fmt.Fprintf(s.stderr,
				"AF-TUI: %s line %d malformed, skipping\n",
				path, lineNo,
			)
			continue
		}
		if k == "" {
			continue
		}
		s.fileEnv[k] = v
	}
	if err := scanner.Err(); err != nil {
		return err
	}
	return nil
}

// parseDotenvLine returns (key, value, ok) for a single .env.local
// line. Blank lines and #-comments return ok=true with an empty key
// (the caller treats those as "skip silently"). Lines without "=" are
// malformed and return ok=false.
//
// Quoting rules (minimal MVP):
//   - Leading/trailing whitespace on key is trimmed.
//   - Leading whitespace on value is trimmed; trailing whitespace
//     OUTSIDE quotes is trimmed; INSIDE quotes whitespace is preserved.
//   - Surrounding single or double quotes on the value are stripped.
//   - No shell expansion (${VAR} stays literal).
//   - No backslash escapes (a backslash is a literal backslash).
func parseDotenvLine(line string) (key, value string, ok bool) {
	// Strip a leading UTF-8 BOM (U+FEFF) that some editors prepend on
	// the first line.
	line = strings.TrimPrefix(line, "\ufeff")
	trimmed := strings.TrimSpace(line)
	if trimmed == "" || strings.HasPrefix(trimmed, "#") {
		return "", "", true
	}
	// Tolerate the dotenv "export" prefix that some users source.
	if strings.HasPrefix(trimmed, "export ") {
		trimmed = strings.TrimSpace(strings.TrimPrefix(trimmed, "export "))
	}
	idx := strings.IndexByte(trimmed, '=')
	if idx < 0 {
		return "", "", false
	}
	key = strings.TrimSpace(trimmed[:idx])
	if key == "" {
		return "", "", false
	}
	value = strings.TrimLeft(trimmed[idx+1:], " \t")
	// Surrounding quotes?
	if n := len(value); n >= 2 {
		first := value[0]
		last := value[n-1]
		if (first == '"' && last == '"') || (first == '\'' && last == '\'') {
			value = value[1 : n-1]
			return key, value, true
		}
	}
	// Unquoted — trim trailing whitespace and inline comments. We only
	// honour an inline " #" sequence (space-hash) as a comment start so
	// values containing '#' as part of the literal are preserved.
	if cut := strings.Index(value, " #"); cut >= 0 {
		value = value[:cut]
	}
	value = strings.TrimRight(value, " \t")
	return key, value, true
}

// Resolve returns the value, provenance label, and presence flag for
// the named variable. processEnv beats fileEnv; missing in both → found
// is false.
func (s *LocalSource) Resolve(varName string) (value string, source string, found bool) {
	if v, ok := s.processEnv[varName]; ok {
		return v, string(SourceProcess), true
	}
	if v, ok := s.fileEnv[varName]; ok {
		return v, string(SourceFile), true
	}
	return "", "", false
}

// ApplyToChildEnv merges LocalSource entries into the child env slice
// in os.Environ() form, returning the merged slice. Precedence rules:
//
//   - childEnv (caller-supplied) wins — the caller is most-specific.
//   - Blocked variables (AGENT_ENV_BLOCKLIST) are NEVER appended,
//     even when present in process env or file env.
//   - Process env wins over file env when both define the same key.
//
// The returned slice is the union; the input slice is not mutated.
func (s *LocalSource) ApplyToChildEnv(childEnv []string) []string {
	// Index childEnv for collision detection.
	have := make(map[string]struct{}, len(childEnv))
	for _, entry := range childEnv {
		idx := strings.IndexByte(entry, '=')
		if idx < 0 {
			continue
		}
		have[entry[:idx]] = struct{}{}
	}

	out := append([]string(nil), childEnv...)

	appendIfNew := func(k, v string) {
		if creds.IsBlocked(k) {
			// Filtered — don't forward. We intentionally do NOT log
			// here because the blocklist filter fires on every spawn
			// and we don't want stderr churn. Diagnostic surfaces can
			// call IsBlocked directly.
			return
		}
		if _, exists := have[k]; exists {
			return
		}
		out = append(out, k+"="+v)
		have[k] = struct{}{}
	}

	// Process env first (highest precedence among the two LocalSource
	// layers); then file env fills gaps.
	for k, v := range s.processEnv {
		appendIfNew(k, v)
	}
	for k, v := range s.fileEnv {
		appendIfNew(k, v)
	}
	return out
}

// MergeIntoBaseEnv returns base augmented with any LocalSource entries
// not already present and not blocked. base wins; LocalSource fills
// gaps. The returned map is a new map — base is not mutated.
//
// This is the integration helper used by daemon_run.go to seed the
// spawner's BaseEnv map[string]string field.
func (s *LocalSource) MergeIntoBaseEnv(base map[string]string) map[string]string {
	out := make(map[string]string, len(base)+len(s.processEnv)+len(s.fileEnv))
	for k, v := range base {
		out[k] = v
	}
	// Process env first.
	for k, v := range s.processEnv {
		if creds.IsBlocked(k) {
			continue
		}
		if _, exists := out[k]; exists {
			continue
		}
		out[k] = v
	}
	for k, v := range s.fileEnv {
		if creds.IsBlocked(k) {
			continue
		}
		if _, exists := out[k]; exists {
			continue
		}
		out[k] = v
	}
	return out
}

// WarnMissing emits a redacted "[creds] no source for KEY — agent may
// fail" stderr line for each name not present in either source. Values
// are never logged.
func (s *LocalSource) WarnMissing(names []string) {
	for _, n := range names {
		if _, _, ok := s.Resolve(n); ok {
			continue
		}
		_, _ = fmt.Fprintf(s.stderr, "[creds] no source for %s — agent may fail\n", n)
	}
}

// EnvLocalPath returns the resolved .env.local path attempted at load
// time (empty when gitRoot was empty). Useful for diagnostic output.
func (s *LocalSource) EnvLocalPath() string { return s.envLocalPath }

// FileEnvKeys returns the sorted list of variable names sourced from
// .env.local. Useful for tests and `af creds setup` (future).
func (s *LocalSource) FileEnvKeys() []string {
	out := make([]string, 0, len(s.fileEnv))
	for k := range s.fileEnv {
		out = append(out, k)
	}
	return out
}
