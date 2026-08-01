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
// # The auth header is always URL-scoped
//
// git's http.extraHeader accepts an optional URL scope: the bare key
// `http.extraHeader` attaches the header to EVERY http(s) request git makes,
// while `http.<url>.extraHeader` attaches it only to requests whose URL matches
// <url>. Because the header is delivered through GIT_CONFIG_* environment
// variables, an unscoped key does not just affect the one git invocation it was
// built for: every descendant process that inherits the environment — a
// submodule fetch, a `go mod download`, npm/pnpm git dependencies, pip VCS
// installs, SwiftPM — attaches the same credential to every git remote it
// touches. That is two bugs at once:
//
//   - a credential leak: a token minted for one remote is offered to unrelated
//     hosts;
//   - a correctness bug that reads as the opposite of its cause: attaching ANY
//     credential to an ANONYMOUS clone of a PUBLIC repository makes the server
//     authenticate it, so a stale or wrong-audience token turns a clone that
//     would have succeeded into `remote: Invalid username or token`. The
//     message sends you hunting for a better credential when the fix is to send
//     none.
//
// Scoping to the host alone (`http.https://github.com/.extraHeader`) does not
// fix the second case: the unrelated public repository is usually on the SAME
// forge. HardenedEnv therefore scopes to the full remote URL — see
// ExtraHeaderConfigKey — and emits NO header at all when the caller cannot name
// an http(s) remote for it. Never re-introduce the bare key.
//
// Producing only scoped keys is half the fix. The other half is defensive:
// this process may itself have been started with an unscoped http.extraHeader
// in its environment, and that inherited value would otherwise flow straight
// through into every git invocation built here. git treats extraHeader as a
// multi-valued key and documents an EMPTY value as the way to reset the
// inherited list, so HardenedEnv always emits `http.extraHeader=` before
// anything else. A URL-scoped key added afterwards still applies to the remote
// it names.
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

// Auth is a per-invocation HTTP authorization header together with the remote
// URL it authenticates. The two are one value because they are never
// independently useful: a header without a remote cannot be scoped, and
// HardenedEnv refuses to emit an unscoped credential (see the package doc).
//
// The zero value means "no auth" and is what every non-authenticating caller
// passes.
type Auth struct {
	// Header is the complete HTTP header line, e.g.
	// "AUTHORIZATION: basic <base64(x-access-token:TOKEN)>" or
	// "Authorization: Bearer <TOKEN>". Empty means no header.
	Header string

	// RemoteURL is the http(s) remote that Header authenticates, e.g.
	// "https://github.com/org/repo.git". It may carry userinfo (it is
	// stripped before use). When it is empty, or is not an http(s) URL —
	// an SSH/scp-style remote, a local path, a purely local git operation
	// with no remote at all — Header cannot be scoped and is therefore NOT
	// emitted. That is deliberate: see HardenedEnv.
	RemoteURL string
}

// ExtraHeaderConfigKey returns the URL-scoped git config key that carries an
// HTTP authorization header for remoteURL, and whether one could be derived:
//
//	https://github.com/org/repo.git
//	  → http.https://github.com/org/repo.git.extraHeader
//
// git resolves an http.<url>.<var> key against a request URL by comparing
// scheme (exact), host (case-insensitive), port (exact, after default-port
// normalisation) and path (exact, or a prefix ending on a "/" boundary). The
// path rule is what makes a whole-remote scope correct rather than brittle: the
// smart-HTTP endpoints git actually requests — .../info/refs,
// .../git-upload-pack, .../git-receive-pack — are all slash-boundary children
// of the remote URL and so still match, while a DIFFERENT repository on the
// same forge does not.
//
// Two components of remoteURL are deliberately discarded:
//
//   - Userinfo. A config key that carries a user name matches only requests
//     with that exact user name; a key with none matches a request with any
//     user name or none. Dropping it makes one key cover both the
//     userinfo-stripped clone URL (see CleanURL) and a remote that still
//     carries one.
//   - Query and fragment. Neither is part of a git remote, and git's URL
//     matcher does not consider them.
//
// (false is returned for an empty, unparseable, hostless or non-http(s)
// remoteURL. Callers that resolved a credential for such a remote are
// misconfigured — nothing over the wire can consume an HTTP header there — and
// should say so rather than fall back to an unscoped key.)
func ExtraHeaderConfigKey(remoteURL string) (string, bool) {
	u, err := url.Parse(strings.TrimSpace(remoteURL))
	if err != nil {
		return "", false
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return "", false
	}
	if u.Host == "" {
		return "", false
	}
	// Rebuilt from vetted components rather than string-sliced out of the
	// input, so nothing unparsed can reach the config key name.
	return "http." + u.Scheme + "://" + u.Host + u.EscapedPath() + ".extraHeader", true
}

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
// This one IS process-wide by design: the keychain popup can be provoked by any
// host, and suppressing a helper only ever removes a credential — it can never
// attach the wrong one to an unrelated remote, which is the failure mode the
// auth header has.
//
// When auth.Header is non-empty AND auth.RemoteURL yields a config scope, the
// header is injected as `http.<remote>.extraHeader`. It travels per invocation
// and is never written to .git/config. When the header cannot be scoped —
// no remote, an SSH/scp-style remote, a local path — NO header is emitted.
// Emitting it unscoped instead would attach the credential to every remote git
// (and every process inheriting this environment) subsequently touches; see the
// package doc for why that is worse than emitting nothing.
//
// An unscoped http.extraHeader INHERITED from baseEnv (a daemon started with
// GIT_CONFIG_* already set, or an operator's global git config) is always reset
// to the empty list first — see the package doc. That reset is unconditional:
// it is exactly as necessary for an invocation that supplies no auth of its own
// (a `git push` that should use the remote's own credential) as for one that
// does.
//
// Config values flow through GIT_CONFIG_COUNT/KEY/VALUE. The function reads any
// count already present in baseEnv and continues numbering, preserving every
// pre-existing GIT_CONFIG_* entry. The secret (auth.Header) only ever appears
// in a VALUE position, never in a KEY name.
func HardenedEnv(baseEnv []string, suppressCredentialHelper bool, auth Auth) []string {
	// Copy the base env so we never mutate the caller's slice.
	out := make([]string, 0, len(baseEnv)+10)
	out = append(out, baseEnv...)

	// Non-interactive baseline (always, when this function is called).
	out = append(out, "GIT_TERMINAL_PROMPT=0", "GCM_INTERACTIVE=never")

	// Collect the config key/value pairs to inject, in a stable order.
	type kv struct{ key, value string }
	var pairs []kv

	// FIRST, always: reset any inherited unscoped http.extraHeader. git treats
	// extraHeader as a multi-valued key and documents the empty value as the
	// way to reset an inherited list — the same idiom as credential.helper
	// below. Without this, a stale unscoped header already in the environment
	// (the shape this package no longer produces, but other tooling still
	// does) outranks whatever credential the invocation actually intends to
	// use: pushes fail with a credential that is present and valid, and an
	// anonymous fetch of a public repo fails *because* a credential is
	// attached. Ordering matters — a later URL-scoped key still applies to the
	// remote it names, so this clears only the unscoped list.
	pairs = append(pairs, kv{key: "http.extraHeader", value: ""})

	if suppressCredentialHelper {
		// Empty credential.helper resets the helper list → no get/store →
		// no keychain prompt. GIT_ASKPASS="" disables any askpass binary.
		out = append(out, "GIT_ASKPASS=")
		pairs = append(pairs, kv{key: "credential.helper", value: ""})
	}

	if auth.Header != "" {
		// No scope, no header. Never fall back to the bare "http.extraHeader".
		if key, ok := ExtraHeaderConfigKey(auth.RemoteURL); ok {
			pairs = append(pairs, kv{key: key, value: auth.Header})
		}
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
