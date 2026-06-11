package agycli

import (
	"encoding/json"
	"os"
	"path/filepath"
)

// agy gates some operations on whether the workspace is "trusted". There is no
// --skip-trust flag; the only lever is the trustedWorkspaces array in
//
//	<stateHome>/antigravity-cli/settings.json
//
// A probe showed that an untrusted-cwd `-p` run with
// --dangerously-skip-permissions succeeds for a no-tool task, so this is
// DEFENSIVE belt-and-suspenders for tool/edit operations, not a hard
// requirement. It is fully best-effort: any failure is ignored and the run
// proceeds.

// ensureWorkspaceTrusted adds cwd to trustedWorkspaces in agy's settings.json
// if not already present. Add-only and deduplicated — it never removes another
// session's entry (avoiding a read-modify-write restore race between concurrent
// sessions). Unknown settings fields are preserved. Returns nil on success or
// any best-effort failure that should be ignored; the bool reports whether a
// write actually happened (for logging/tests).
func ensureWorkspaceTrusted(stateHome, cwd string) (changed bool) {
	if stateHome == "" || cwd == "" {
		return false
	}
	path := filepath.Join(stateHome, "antigravity-cli", "settings.json")

	var settings map[string]any
	data, err := os.ReadFile(path) //nolint:gosec // config path, not user input
	if err == nil {
		_ = json.Unmarshal(data, &settings) // tolerate a malformed file → start fresh below
	}
	if settings == nil {
		settings = make(map[string]any)
	}

	existing := stringSlice(settings["trustedWorkspaces"])
	for _, w := range existing {
		if w == cwd {
			return false // already trusted
		}
	}
	existing = append(existing, cwd)
	settings["trustedWorkspaces"] = existing

	body, err := json.MarshalIndent(settings, "", "  ")
	if err != nil {
		return false
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return false
	}
	//nolint:gosec // 0644 matches agy's own settings.json perms; no secrets here
	if err := os.WriteFile(path, body, 0o644); err != nil {
		return false
	}
	return true
}

// stringSlice coerces a decoded JSON value into []string, dropping non-string
// elements. Tolerates nil and wrong types (returns an empty slice).
func stringSlice(v any) []string {
	arr, ok := v.([]any)
	if !ok {
		return nil
	}
	out := make([]string, 0, len(arr))
	for _, e := range arr {
		if s, ok := e.(string); ok {
			out = append(out, s)
		}
	}
	return out
}
