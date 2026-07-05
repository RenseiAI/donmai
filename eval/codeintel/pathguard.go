package codeintel

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// ── Contamination guard (brief 06 §1.5 / §5 risk 1) ──────────────────────────
//
// The donmai binary — and thus `donmai code` — is baked into 4 of the 7 sandbox
// images. A WITHOUT arm that merely omits code-intel from the prompt leaks the
// tool into the control group (a curious agent runs `donmai code --help`),
// silently flattening the measured delta. The control arm MUST physically strip
// the binary from PATH. These helpers make that strip, and prove it took.

// pathListSep is the OS PATH separator (":" on unix, ";" on windows).
var pathListSep = string(os.PathListSeparator)

// envPath extracts the PATH value from an environment slice ("KEY=VALUE"
// entries). Returns "" when PATH is unset. The last PATH entry wins, matching
// exec's own resolution.
func envPath(env []string) string {
	path := ""
	for _, kv := range env {
		if v, ok := strings.CutPrefix(kv, "PATH="); ok {
			path = v
		}
	}
	return path
}

// setEnvPath returns a copy of env with PATH replaced by value (adding a PATH
// entry if none existed).
func setEnvPath(env []string, value string) []string {
	out := make([]string, 0, len(env)+1)
	replaced := false
	for _, kv := range env {
		if strings.HasPrefix(kv, "PATH=") {
			out = append(out, "PATH="+value)
			replaced = true
			continue
		}
		out = append(out, kv)
	}
	if !replaced {
		out = append(out, "PATH="+value)
	}
	return out
}

// isExecutable reports whether p is a regular file with any execute bit set.
func isExecutable(p string) bool {
	fi, err := os.Stat(p)
	if err != nil || fi.IsDir() {
		return false
	}
	return fi.Mode().Perm()&0o111 != 0
}

// BinaryOnPath resolves an executable named `name` against the given PATH string
// (NOT the process env), returning its absolute path and true when found. It
// mirrors exec.LookPath's directory scan but against an explicit PATH so the
// harness can inspect a scrubbed arm-PATH without mutating the process env.
func BinaryOnPath(name, path string) (string, bool) {
	if name == "" {
		return "", false
	}
	for _, dir := range strings.Split(path, pathListSep) {
		if dir == "" {
			continue // an empty PATH element means "cwd"; we don't treat it as a hit.
		}
		cand := filepath.Join(dir, name)
		if isExecutable(cand) {
			return cand, true
		}
	}
	return "", false
}

// ScrubBinaryFromEnv returns a copy of env whose PATH has every directory that
// contains an executable named `name` removed, plus the list of directories it
// dropped. After this, BinaryOnPath(name, <scrubbed PATH>) is guaranteed false.
//
// This is the mandatory WITHOUT-arm guard. It is deliberately dir-granular
// (drop the whole dir that holds the binary) rather than shimming, so the agent
// genuinely cannot exec the binary — a shim that errors would still let the
// agent "find" donmai. In a sandbox image the donmai binary lives at a
// dedicated location, so dropping its dir does not strip sibling tools; on a dev
// host the harness points the WITH arm at an explicit --donmai-bin dir (its own
// directory), so only that dir is dropped here.
func ScrubBinaryFromEnv(env []string, name string) (scrubbed []string, dropped []string) {
	path := envPath(env)
	kept := make([]string, 0)
	for _, dir := range strings.Split(path, pathListSep) {
		if dir == "" {
			kept = append(kept, dir)
			continue
		}
		if isExecutable(filepath.Join(dir, name)) {
			dropped = append(dropped, dir)
			continue
		}
		kept = append(kept, dir)
	}
	return setEnvPath(env, strings.Join(kept, pathListSep)), dropped
}

// PrependPath returns a copy of env with dir prepended to PATH (highest
// precedence). Used to make the donmai binary reachable in the WITH arm when it
// is not already on the base PATH (e.g. a freshly built binary in a dev
// scratch dir).
func PrependPath(env []string, dir string) []string {
	if dir == "" {
		return env
	}
	path := envPath(env)
	if path == "" {
		return setEnvPath(env, dir)
	}
	return setEnvPath(env, dir+pathListSep+path)
}

// NeutralizeBinaryInEnv returns a copy of env whose PATH resolves every tool it
// did before EXCEPT `name`, plus a cleanup for the shadow directories it creates.
//
// Unlike ScrubBinaryFromEnv — which drops the WHOLE directory that holds the
// binary, taking sibling tools (rg/gh/git) down with it — this masks ONLY
// `name`: any PATH dir that contains an executable `name` is replaced, in place
// in the PATH order, by a freshly-built shadow dir that symlinks every sibling
// entry except `name`. After this, BinaryOnPath(name, PATH) is false while every
// other tool resolves exactly as before.
//
// This is what makes the A/B control clean WITHOUT biasing it: on a dogfooding
// host donmai is co-installed alongside baseline tools (e.g. /opt/homebrew/bin),
// so dropping that whole dir would strip ripgrep/gh from the control only. The
// driver neutralizes donmai on the SHARED base for BOTH arms and re-adds it only
// for WITH via StageBinaryOnlyDir, so the two arms resolve an identical set of
// non-donmai tools and differ solely on donmai.
func NeutralizeBinaryInEnv(env []string, name string) (out []string, cleanup func(), err error) {
	var shadows []string
	cleanup = func() {
		for _, d := range shadows {
			_ = os.RemoveAll(d)
		}
	}
	path := envPath(env)
	newDirs := make([]string, 0)
	for _, dir := range strings.Split(path, pathListSep) {
		if dir == "" || !isExecutable(filepath.Join(dir, name)) {
			newDirs = append(newDirs, dir)
			continue
		}
		shadow, serr := shadowDirExcluding(dir, name)
		if serr != nil {
			cleanup()
			return nil, nil, fmt.Errorf("neutralize %q in %s: %w", name, dir, serr)
		}
		shadows = append(shadows, shadow)
		newDirs = append(newDirs, shadow)
	}
	return setEnvPath(env, strings.Join(newDirs, pathListSep)), cleanup, nil
}

// shadowDirExcluding builds a temp dir that symlinks every entry of src except
// `exclude`, so the shadow resolves every sibling tool but not the excluded one.
func shadowDirExcluding(src, exclude string) (string, error) {
	entries, err := os.ReadDir(src)
	if err != nil {
		return "", err
	}
	shadow, err := os.MkdirTemp("", "codeintel-eval-mask-")
	if err != nil {
		return "", err
	}
	for _, e := range entries {
		if e.Name() == exclude {
			continue
		}
		// Best-effort: skip entries we can't link (name clashes, odd perms); the
		// goal is to preserve resolvable sibling tools, not mirror the dir exactly.
		_ = os.Symlink(filepath.Join(src, e.Name()), filepath.Join(shadow, e.Name()))
	}
	return shadow, nil
}

// StageBinaryOnlyDir creates a temp dir containing a single symlink `name` →
// binPath, and returns the dir plus a cleanup. Prepending this dir to a
// neutralized PATH makes exactly ONE binary reachable and nothing else — the
// dedicated donmai-only directory the WITH arm gets, so re-adding donmai cannot
// re-introduce any sibling tool the WITHOUT arm lacks.
func StageBinaryOnlyDir(binPath, name string) (dir string, cleanup func(), err error) {
	dir, err = os.MkdirTemp("", "codeintel-eval-bin-")
	if err != nil {
		return "", nil, err
	}
	if err := os.Symlink(binPath, filepath.Join(dir, name)); err != nil {
		_ = os.RemoveAll(dir)
		return "", nil, fmt.Errorf("stage %q: %w", name, err)
	}
	return dir, func() { _ = os.RemoveAll(dir) }, nil
}

// VerifyControlClean asserts the mandatory contamination guard held: the given
// arm environment must NOT resolve `name`. Returns a descriptive error when the
// binary is still reachable (the control is contaminated) — callers treat this
// as fatal for the WITHOUT arm.
func VerifyControlClean(env []string, name string) error {
	if p, found := BinaryOnPath(name, envPath(env)); found {
		return fmt.Errorf("control-arm contamination: %q is still reachable on PATH at %s — the WITHOUT arm must not resolve it", name, p)
	}
	return nil
}
