package agycli

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// API-key mode session isolation (keymode.go).
//
// agy 1.1.13+ runs keyed when its settings file selects the Gemini model
// provider and GEMINI_API_KEY is in the environment:
//
//	$HOME/.gemini/antigravity-cli/settings.json: {"modelProvider":"gemini"}
//	GEMINI_API_KEY=<session key> (env)
//	GOOGLE_GEMINI_BASE_URL=<Google-compatible endpoint> (env, optional)
//
// A keyed spawn therefore needs a HOME that carries exactly that settings
// file. Pointing HOME at the operator's real home would write session
// state (and the keyed settings selection) into the operator's own
// ~/.gemini — shared, persistent, and racing every concurrent session —
// so the provider builds a per-session HOME instead: a fresh directory
// holding ONLY .gemini/antigravity-cli/settings.json, with HOME (and the
// XDG base dirs, which agy-family CLIs may consult) redirected at it for
// the child alone. The directory is removed when the session ends; the
// operator's real ~/.gemini is never read or written on this path.
//
// The host-session path (no session key) is untouched: the child inherits
// the ambient HOME and agy reads the operator's own login state.

// Keyed-session env-var NAMES (never values). keyEnvNames is the closed
// declaration of every variable name this provider injects into a keyed
// child's environment on top of the inherited + Spec.Env composition:
// the session key itself, the optional compatible-endpoint override, and
// the HOME/XDG redirects that isolate the session. keyedChildEnvKeys
// pins this declaration against the live keyed composition, so a new
// injected name updates the declaration in the same change instead of
// riding the child env undeclared.
const (
	// EnvGeminiAPIKey carries the session's Gemini API key on a keyed
	// spawn, read from Spec.Env (the runner-resolved cell credential).
	EnvGeminiAPIKey = "GEMINI_API_KEY" //nolint:gosec // G101: env-var NAME, never credential bytes.

	// EnvGoogleGeminiBaseURL optionally points a keyed spawn at a
	// Google-compatible endpoint, passed through from Spec.Env when the
	// resolved cell binds one.
	EnvGoogleGeminiBaseURL = "GOOGLE_GEMINI_BASE_URL"
)

// keyEnvNames is the closed injected-env-keys declaration for the keyed
// spawn path: every variable name the provider sets on the keyed child
// environment beyond the inherited + Spec.Env merge. HOME and the XDG
// base dirs redirect the child at its per-session HOME; the key and the
// optional base-URL override carry the session credential and route.
var keyEnvNames = []string{
	EnvGeminiAPIKey,
	EnvGoogleGeminiBaseURL,
	"HOME",
	"XDG_CONFIG_HOME",
	"XDG_DATA_HOME",
	"XDG_STATE_HOME",
	"XDG_CACHE_HOME",
}

// keyedSessionKey reports whether spec carries a keyed-session credential:
// a non-empty GEMINI_API_KEY on the trusted Spec.Env layer. Empty means
// host-session — the provider injects no key and isolates no HOME.
func keyedSessionKey(specEnv map[string]string) (string, bool) {
	key := specEnv[EnvGeminiAPIKey]
	if key == "" {
		return "", false
	}
	return key, true
}

// keyedSettingsName is the settings-file basename agy reads API-key mode
// from, relative to the session HOME.
var keyedSettingsRelPath = filepath.Join(".gemini", "antigravity-cli", "settings.json")

// keyedModelProviderSelection is the exact settings.json content that
// selects API-key mode. Extra fields are deliberately absent: the file is
// agy's only keyed input besides the environment, and anything else would
// be session-opaque agy behavior the provider does not own.
const keyedModelProviderSelection = `{"modelProvider":"gemini"}`

// prepareKeyedHome builds the per-session HOME for a keyed spawn: a fresh
// directory holding ONLY the API-key-mode settings.json, and returns its
// path. The caller redirects the child's HOME/XDG there and removes the
// directory when the session ends (see keyedHomeCleanup). Any failure
// aborts the spawn — a keyed session without its isolated HOME would
// otherwise fall back to the operator's real ~/.gemini.
func prepareKeyedHome() (string, error) {
	dir, err := os.MkdirTemp("", "donmai-agy-keyed-home-*")
	if err != nil {
		return "", fmt.Errorf("agy keyed spawn: create isolated HOME: %w", err)
	}
	settingsPath := filepath.Join(dir, keyedSettingsRelPath)
	if err := os.MkdirAll(filepath.Dir(settingsPath), 0o700); err != nil {
		_ = os.RemoveAll(dir)
		return "", fmt.Errorf("agy keyed spawn: create isolated settings dir: %w", err)
	}
	var decoded map[string]any
	if err := json.Unmarshal([]byte(keyedModelProviderSelection), &decoded); err != nil {
		_ = os.RemoveAll(dir)
		return "", fmt.Errorf("agy keyed spawn: invalid embedded settings: %w", err)
	}
	body, err := json.Marshal(decoded)
	if err != nil {
		_ = os.RemoveAll(dir)
		return "", fmt.Errorf("agy keyed spawn: encode isolated settings: %w", err)
	}
	// 0600: the directory selects keyed mode, and a sibling session's
	// settings file is none of this session's business. (The key itself
	// rides the environment, never disk.)
	if err := os.WriteFile(settingsPath, body, 0o600); err != nil {
		_ = os.RemoveAll(dir)
		return "", fmt.Errorf("agy keyed spawn: write isolated settings: %w", err)
	}
	return dir, nil
}

// keyedHomeCleanup removes a per-session HOME prepared by prepareKeyedHome.
// Best-effort: spawn already succeeded, so a leftover temp dir is litter,
// not a session failure. Kept as a named function so the handle owns
// exactly one cleanup closure.
func keyedHomeCleanup(dir string) error {
	if dir == "" {
		return nil
	}
	return os.RemoveAll(dir)
}

// keyedHomeEnv returns the HOME/XDG redirect layer for a keyed child: every
// directory-bearing variable the provider sets points inside home, so agy
// (and any XDG-consulting helper it shells to) stays under the isolated
// tree. HOME itself is included. Callers overlay this layer over the
// composed child env (keyed layer wins), then set the session key.
func keyedHomeEnv(home string) map[string]string {
	return map[string]string{
		"HOME":            home,
		"XDG_CONFIG_HOME": filepath.Join(home, ".config"),
		"XDG_DATA_HOME":   filepath.Join(home, ".local", "share"),
		"XDG_STATE_HOME":  filepath.Join(home, ".local", "state"),
		"XDG_CACHE_HOME":  filepath.Join(home, ".cache"),
	}
}
