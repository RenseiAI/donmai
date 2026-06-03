// Package statehome is the single host-identity seam for deriving the
// daemon's on-disk state and log directories from one brand token.
//
// Motivation: library packages must not read process environment variables
// to discover where state lives (config-via-API, not config-via-env). They
// call this package's resolvers, and the embedder configures the brand once
// at process init. The default brand is "donmai", so a bare library import
// resolves canonical OSS paths with no setup.
//
// Layout (brand = "donmai" by default):
//
//	State dir: ~/.donmai/<suffix>
//	Log dir:   ~/Library/Logs/donmai/
//	Log file:  ~/Library/Logs/donmai/daemon.log
//	Err file:  ~/Library/Logs/donmai/daemon-error.log
//
// Configuration is process-global and set-once at init. An embedder (the
// binary entrypoint, never a library package) may call SetBrand to rebrand
// the directories, or SetBaseHome to redirect the base home directory away
// from os.UserHomeDir() (honoring an explicit full-path override). Resolving
// a path BEFORE the brand is set is legal — the default applies — but emits
// a one-time slog.Warn so a late-setting embedder is caught in review.
//
// Stdlib-only by repo policy: os, path/filepath, sync, log/slog.
package statehome

import (
	"log/slog"
	"os"
	"path/filepath"
	"sync"
)

// DefaultBrand is the canonical OSS brand token. It is the default for both
// the state directory (~/.donmai) and the log directory
// (~/Library/Logs/donmai). This default is a review invariant — do not
// change it.
const DefaultBrand = "donmai"

var (
	mu sync.RWMutex

	// brand is the active brand token. Defaults to DefaultBrand.
	brand = DefaultBrand

	// baseHome overrides the home directory used to anchor every resolved
	// path. Empty means "resolve via os.UserHomeDir() at call time".
	baseHome string

	// brandSet records whether SetBrand has been called. Used to warn once
	// when a path is resolved before the embedder configured the brand.
	brandSet bool

	// warnedResolveBeforeSet guards the one-time resolve-before-set warning.
	warnedResolveBeforeSet bool
)

// SetBrand configures the brand token used to derive the state and log
// directories. Intended to be called once by the binary entrypoint at
// process init, before any daemon/runner construction. An empty token is
// ignored (the previous brand is kept) so a misconfigured embedder cannot
// silently produce "~/." / "~/Library/Logs/" with an empty leaf.
func SetBrand(b string) {
	if b == "" {
		return
	}
	mu.Lock()
	brand = b
	brandSet = true
	mu.Unlock()
}

// SetBaseHome overrides the base home directory that anchors every resolved
// path. Pass an absolute directory to honor an explicit full-path override
// (e.g. from a DONMAI_STATE_HOME environment variable read in the binary
// entrypoint). Pass "" to revert to resolving via os.UserHomeDir() at call
// time. Like SetBrand, this is intended for the embedder at init.
func SetBaseHome(absDir string) {
	mu.Lock()
	baseHome = absDir
	brandSet = true
	mu.Unlock()
}

// Brand returns the currently-configured brand token.
func Brand() string {
	mu.RLock()
	defer mu.RUnlock()
	return brand
}

// BaseHome returns the configured base-home override, or "" when paths are
// resolved via os.UserHomeDir() at call time.
func BaseHome() string {
	mu.RLock()
	defer mu.RUnlock()
	return baseHome
}

// resolveHome returns the home directory used to anchor paths and whether it
// resolved successfully. It prefers an explicit SetBaseHome override; absent
// one it falls back to os.UserHomeDir(). It also emits the one-time
// resolve-before-set warning when the brand has not been configured yet.
func resolveHome() (string, bool) {
	mu.RLock()
	base := baseHome
	configured := brandSet
	mu.RUnlock()

	if !configured {
		warnResolveBeforeSet()
	}

	if base != "" {
		return base, true
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", false
	}
	return home, true
}

// warnResolveBeforeSet emits a single slog.Warn the first time a path is
// resolved before SetBrand/SetBaseHome ran. This catches an embedder that
// constructs the daemon/runner before configuring the seam (the default
// brand silently applies, which is rarely what such an embedder wants).
func warnResolveBeforeSet() {
	mu.Lock()
	already := warnedResolveBeforeSet
	warnedResolveBeforeSet = true
	mu.Unlock()
	if already {
		return
	}
	slog.Warn("statehome: host-state path resolved before brand was configured; using default",
		"default_brand", DefaultBrand)
}

// StateDir returns the absolute path to a state entry under the brand state
// directory: ~/.<brand>/<suffix>. The suffix is joined verbatim (it may be a
// multi-segment relative path). When the home directory cannot be resolved
// and no base-home override is set, StateDir returns "" — callers that need a
// fallback should detect the empty string (see internal/statepath.Resolve).
func StateDir(suffix string) string {
	mu.RLock()
	b := brand
	mu.RUnlock()
	home, ok := resolveHome()
	if !ok {
		return ""
	}
	return filepath.Join(home, "."+b, suffix)
}

// LogDir returns the absolute path to the daemon log directory:
// ~/Library/Logs/<brand>. Returns "" when the home directory cannot be
// resolved and no base-home override is set.
func LogDir() string {
	mu.RLock()
	b := brand
	mu.RUnlock()
	home, ok := resolveHome()
	if !ok {
		return ""
	}
	return filepath.Join(home, "Library", "Logs", b)
}

// LogPath returns the daemon stdout log path: LogDir()/daemon.log.
// Returns "" when LogDir() is unresolvable.
func LogPath() string {
	dir := LogDir()
	if dir == "" {
		return ""
	}
	return filepath.Join(dir, "daemon.log")
}

// ErrorLogPath returns the daemon stderr log path:
// LogDir()/daemon-error.log. Returns "" when LogDir() is unresolvable.
func ErrorLogPath() string {
	dir := LogDir()
	if dir == "" {
		return ""
	}
	return filepath.Join(dir, "daemon-error.log")
}
