// Package gitexec holds shared, brand-neutral helpers for invoking git from a
// headless daemon. Its central function, HardenedEnv, builds the environment
// overrides that keep git non-interactive and — optionally — suppress the OS
// credential helper and inject per-invocation HTTP auth.
//
// Two problems motivate this package:
//
//  1. Under a launchd / systemd service there is no controlling terminal and no
//     logged-in keychain session. A git operation that reaches the macOS
//     osxkeychain (or git-credential-manager) helper triggers a blocking GUI
//     popup ("A keychain cannot be found to store …") that hangs the daemon
//     forever. HardenedEnv resets the credential.helper list to empty so git
//     never consults a helper.
//
//  2. Baking a token into the persisted remote URL (https://x-access-token:T@host)
//     leaves the secret on disk in .git/config for the lifetime of the clone.
//     HardenedEnv can instead inject an http.extraHeader so auth travels per
//     invocation and never persists.
//
// All config is injected through git's GIT_CONFIG_COUNT / GIT_CONFIG_KEY_n /
// GIT_CONFIG_VALUE_n environment mechanism (git >= 2.31), NOT via -c argv flags:
// argv is visible to any local user via `ps`, env is not, and neither form
// writes to .git/config. The function reads any GIT_CONFIG_COUNT already present
// in the supplied base environment and continues numbering from there, so it
// composes cleanly with callers that have pre-seeded their own git config keys.
//
// HardenedEnv is a pure function: it never reads process state and returns a
// fresh slice, leaving the input untouched.
package gitexec

import (
	"fmt"
	"net/url"
	"strconv"
	"strings"
)

// envGitConfigCount is git's count variable: it tells git how many
// GIT_CONFIG_KEY_n / GIT_CONFIG_VALUE_n pairs to read from the environment.
const envGitConfigCount = "GIT_CONFIG_COUNT"

// HardenedEnv returns baseEnv plus the overrides that make a git invocation
// safe to run headless. The input slice is never mutated; a new slice is
// returned.
//
// Always appended (the non-interactive baseline):
//   - GIT_TERMINAL_PROMPT=0   — git fails fast instead of waiting on a TTY.
//   - GCM_INTERACTIVE=never   — git-credential-manager skips its device-code flow.
//
// When suppressCredentialHelper is true, the credential helper chain is
// neutralised so git never performs a keychain get/store (the source of the
// launchd GUI-popup hang):
//   - GIT_ASKPASS=""          — disables any askpass program.
//   - credential.helper=""    — an empty value RESETS the helper list, so no
//     configured helper (osxkeychain, manager, store, …) runs.
//
// When authHeader is non-empty it is injected as an http.extraHeader git config
// value, e.g. "AUTHORIZATION: basic <base64(x-access-token:TOKEN)>" or
// "Authorization: Bearer <TOKEN>". The header travels per invocation and is
// never written to .git/config. An empty authHeader adds no extraHeader key.
//
// Config values flow through GIT_CONFIG_COUNT/KEY/VALUE. The function reads any
// count already present in baseEnv and continues numbering, preserving every
// pre-existing GIT_CONFIG_* entry. The secret (authHeader) only ever appears in
// a VALUE position, never in a KEY name.
func HardenedEnv(baseEnv []string, suppressCredentialHelper bool, authHeader string) []string {
	// Copy the base env so we never mutate the caller's slice.
	out := make([]string, 0, len(baseEnv)+8)
	out = append(out, baseEnv...)

	// Non-interactive baseline (always, when this function is called).
	out = append(out, "GIT_TERMINAL_PROMPT=0", "GCM_INTERACTIVE=never")

	// Collect the config key/value pairs to inject, in a stable order.
	type kv struct{ key, value string }
	var pairs []kv

	if suppressCredentialHelper {
		// Empty credential.helper resets the helper list → no get/store →
		// no keychain prompt. GIT_ASKPASS="" disables any askpass binary.
		out = append(out, "GIT_ASKPASS=")
		pairs = append(pairs, kv{key: "credential.helper", value: ""})
	}

	if authHeader != "" {
		pairs = append(pairs, kv{key: "http.extraHeader", value: authHeader})
	}

	if len(pairs) == 0 {
		return out
	}

	// Continue numbering from any pre-existing GIT_CONFIG_COUNT so we never
	// clobber config keys the caller already injected.
	start := existingConfigCount(baseEnv)
	for i, p := range pairs {
		n := start + i
		out = append(out,
			fmt.Sprintf("GIT_CONFIG_KEY_%d=%s", n, p.key),
			fmt.Sprintf("GIT_CONFIG_VALUE_%d=%s", n, p.value),
		)
	}
	out = append(out, fmt.Sprintf("%s=%d", envGitConfigCount, start+len(pairs)))
	return out
}

// existingConfigCount returns the value of the LAST GIT_CONFIG_COUNT assignment
// in env, or 0 when absent or unparseable. The last assignment wins because
// that is the value git's execve-supplied environment resolves to for a
// duplicated key.
func existingConfigCount(env []string) int {
	prefix := envGitConfigCount + "="
	count := 0
	for _, e := range env {
		if !strings.HasPrefix(e, prefix) {
			continue
		}
		raw := strings.TrimPrefix(e, prefix)
		n, err := strconv.Atoi(strings.TrimSpace(raw))
		if err != nil || n < 0 {
			// A malformed count from upstream is treated as "no pairs"; we
			// still continue scanning in case a later, valid assignment wins.
			count = 0
			continue
		}
		count = n
	}
	return count
}

// CleanURL strips any embedded userinfo (user:password@) from an HTTP(S) git
// URL and returns the bare form, so an injected http.extraHeader can carry the
// auth instead of persisting a token in .git/config. It returns (clean, true)
// when userinfo was present and stripped, and (rawURL, false) when there was
// nothing to strip or the input is not a parseable http(s) URL (e.g. an SSH or
// scp-style remote, or a local path) — in which case the caller should leave
// the URL untouched.
func CleanURL(rawURL string) (string, bool) {
	if rawURL == "" {
		return rawURL, false
	}
	u, err := url.Parse(rawURL)
	if err != nil {
		return rawURL, false
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return rawURL, false
	}
	if u.User == nil {
		return rawURL, false
	}
	u.User = nil
	return u.String(), true
}
