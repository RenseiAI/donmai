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
