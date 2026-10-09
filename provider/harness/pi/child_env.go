package pi

import (
	"os"
	"sort"
	"strings"

	"github.com/RenseiAI/donmai/agent"
	runtimeenv "github.com/RenseiAI/donmai/runtime/env"
)

// This file builds the pi harness child's EXEC environment from an
// allowlist. pi renames its own process at startup (node's process.title),
// and on some platforms a same-user process listing then renders the leading
// entries of the child's exec-time environment block as if they were its
// command line. Whatever the child is exec'd with is therefore readable by
// any same-user process, and lands in terminals, logs and transcripts the
// moment someone runs a listing while debugging.
//
// A name-based strip cannot close that surface: the next secret arrives
// under a name nobody listed yet. So the exec environment is built from
// scratch out of names known to carry no secret, and every other entry is
// kept OUT of it by default:
//
//   - allowlisted names (childEnvAllowed below) form the exec environment —
//     what pi itself needs to start: search path, home, scratch, locale,
//     terminal, TLS trust paths, proxy routes without credentials, and the
//     runtime startup controls receipt admission inspects;
//   - session model credentials (sessionCredentialNames) and refused names
//     (childEnvRefused) are dropped outright, and so is any other entry that
//     carries a model credential's VALUE (sessionCredentialValues): a
//     control plane may file the session's model key under a second,
//     provider-native name beside the injected provider's key, and that copy
//     is the same secret. The model credentials reach the session only
//     through the owner-only credential file (the injected provider's key)
//     and the session auth.json (native routes);
//   - every other entry is a session binding the agent's TOOLS may need (a
//     tracker or VCS token, a toolchain home, a platform-declared binding).
//     It rides the owner-only credential file's environment section instead
//     of the exec environment, and the policy extension restores it into
//     pi's in-process environment at load, before any tool can run. A tool
//     subprocess pi starts inherits it from there; the renamed process's
//     listing never shows it, because a listing renders the exec-time block
//     and an in-process assignment never writes into that block.
//
// Both spawn lanes build their exec environment through partitionChildEnv,
// and neither inherits the parent environment past it: the headless lane
// assigns the composed slice to exec.Cmd.Env, and the interactive lane asks
// the PTY host for an exact environment (ptyhost.Spec.ExactEnv), so no
// parent name rides around the allowlist and no empty shadow is needed to
// mask one.

// childEnvAllowedNames are the exact variable names admitted into the pi
// child's exec environment. Each one names process context, not authority:
// a value under one of these names configures how the process runs, never
// what it may access. Nothing session-specific is listed — session bindings
// ride the credential file's environment section (see the file comment).
//
// Adding a name here puts its value in every same-user process listing of
// a live pi session. TestChildEnvAllowlistCarriesNoSecretShapedName refuses
// any name that looks like a credential.
var childEnvAllowedNames = map[string]struct{}{
	// Process identity and search path.
	"PATH": {}, "HOME": {}, "USER": {}, "LOGNAME": {}, "SHELL": {},
	// Scratch space.
	"TMPDIR": {}, "TMP": {}, "TEMP": {},
	// Locale and time zone. LC_* categories are admitted by prefix.
	"LANG": {}, "LANGUAGE": {}, "TZ": {},
	// Terminal presentation.
	"TERM": {}, "COLORTERM": {}, "NO_COLOR": {}, "FORCE_COLOR": {},
	// XDG base directories.
	"XDG_CONFIG_HOME": {}, "XDG_CACHE_HOME": {}, "XDG_DATA_HOME": {},
	"XDG_STATE_HOME": {}, "XDG_RUNTIME_DIR": {}, "XDG_CONFIG_DIRS": {},
	"XDG_DATA_DIRS": {},
	// TLS trust anchors, read once at startup: a path, never key material.
	"NODE_EXTRA_CA_CERTS": {}, "SSL_CERT_FILE": {}, "SSL_CERT_DIR": {},
	// Proxy bypass lists (host names only).
	"NO_PROXY": {}, "no_proxy": {},
	// The runtime mode marker. The runtime startup controls receipt admission
	// inspects (receiptUnsafeStartupEnv) are admitted alongside it below, so
	// a host that sets them keeps both its startup behavior and the receipt
	// refusal that guards it.
	"NODE_ENV": {},
	// pi's documented offline-posture bindings: a session may re-enable
	// either explicitly (offlinePostureEnv).
	piOfflineEnvVar: {}, piSkipVersionCheckEnvVar: {},
}

// childEnvAllowedPrefixes admits whole families by prefix. Only locale
// categories qualify: every LC_* value is a locale name.
var childEnvAllowedPrefixes = []string{"LC_"}

// childEnvProxyNames are proxy routes pi needs at startup to reach a model
// endpoint through a proxy. A proxy URL may embed credentials in its
// userinfo, so one is admitted only when it carries none
// (proxyURLCarriesNoUserinfo); one that does is kept out of the exec
// environment and rides the credential file like any other binding.
var childEnvProxyNames = map[string]struct{}{
	"HTTP_PROXY": {}, "HTTPS_PROXY": {}, "ALL_PROXY": {},
	"http_proxy": {}, "https_proxy": {}, "all_proxy": {},
}

// childEnvHarnessPrefixes are the namespaces this harness owns on the child:
// the policy extension's own controls (DONMAI_PI_*) and pi's configuration
// (PI_*). The harness composes every entry it means the child to see under
// them (providerPinEnv, the config-home redirect, the credential-file path,
// the tool-policy lists), so an inherited or snapshot copy never rides
// along — in the exec environment or the credential file — where it could
// steer the extension or pi's config. The two allowlisted offline-posture
// names are the one exception, by design.
var childEnvHarnessPrefixes = []string{"DONMAI_PI_", "PI_"}

// childEnvAllowed reports whether name=value may ride the pi child's exec
// environment.
func childEnvAllowed(name, value string) bool {
	if _, ok := childEnvAllowedNames[name]; ok {
		return true
	}
	if receiptUnsafeStartupEnv[name] {
		return true
	}
	if _, ok := childEnvProxyNames[name]; ok {
		return proxyURLCarriesNoUserinfo(value)
	}
	for _, prefix := range childEnvAllowedPrefixes {
		if strings.HasPrefix(name, prefix) && len(name) > len(prefix) {
			return true
		}
	}
	return false
}

// proxyURLCarriesNoUserinfo reports whether a proxy route names no
// credentials. Proxy variables accept a URL or a bare user:pass@host:port
// form, and parsing the bare form as a URL misreads its userinfo as a
// scheme — so any '@' at all counts as userinfo, rather than trusting a
// parse to find it.
func proxyURLCarriesNoUserinfo(value string) bool {
	return !strings.Contains(value, "@")
}

// childEnvRefused reports whether name must never reach the pi child in any
// form — neither its exec environment nor the credential file's environment
// section. These are the session's model credentials (which reach it only
// through the credential file's credential section and the session
// auth.json), the known secret-valued bindings no child is entitled to, the
// per-session read credential the worker consumed during bootstrap, the
// host model-auth names the shared blocklist guards, runner-only controls,
// and the harness-owned namespaces.
func childEnvRefused(name string) bool {
	if isSessionCredentialName(name) || runtimeenv.IsRunnerOnly(name) || name == runtimeenv.SessionReadTokenEnv {
		return true
	}
	for _, refused := range undeclaredSecretValueNames {
		if name == refused {
			return true
		}
	}
	for _, blocked := range runtimeenv.AgentEnvBlocklist {
		if name == blocked {
			return true
		}
	}
	if name == piOfflineEnvVar || name == piSkipVersionCheckEnvVar {
		return false
	}
	for _, prefix := range childEnvHarnessPrefixes {
		if strings.HasPrefix(name, prefix) {
			return true
		}
	}
	return false
}

// childEnvPartition is a session environment split by where each entry may
// ride.
type childEnvPartition struct {
	// exec is the allowlisted KEY=VALUE set the child is exec'd with, sorted.
	exec []string
	// deferred is every remaining, non-refused binding, sorted by name. It
	// rides the credential file's environment section; the policy extension
	// restores it into pi's in-process environment at load.
	deferred []sessionCredential
}

// partitionChildEnv composes the session environment exactly as every
// harness does — the parent environment filtered by the shared blocklist
// (with the supervisor's declared re-admissions), overlaid by the trusted
// Spec.Env layer, runner-only controls refused at both — and then splits it:
// allowlisted entries form the exec environment, refused entries are
// dropped, and everything else is deferred to the credential file.
//
// Composing first keeps the session's effective environment identical to
// the one the other harnesses build, so a tool pi starts sees the same
// bindings it saw before the exec environment was narrowed; only where each
// entry rides changes.
func partitionChildEnv(parent []string, spec agent.Spec) childEnvPartition {
	var out childEnvPartition
	composed := runtimeenv.NewComposer().Compose(envSliceToMap(parent), spec)
	credentialValues := sessionCredentialValues(composed)
	for _, entry := range composed {
		i := strings.IndexByte(entry, '=')
		if i <= 0 {
			continue
		}
		name, value := entry[:i], entry[i+1:]
		_, carriesCredential := credentialValues[value]
		switch {
		case childEnvRefused(name), carriesCredential:
			continue
		case childEnvAllowed(name, value):
			out.exec = append(out.exec, entry)
		case runtimeenv.ValidEnvKey(name):
			out.deferred = append(out.deferred, sessionCredential{Env: name, Value: value})
		}
	}
	sort.Strings(out.exec)
	sort.Slice(out.deferred, func(a, b int) bool { return out.deferred[a].Env < out.deferred[b].Env })
	return out
}

// sessionCredentialValues collects every non-empty value the composed
// session environment carries under a model-credential name
// (isSessionCredentialName), except the binding-identity names
// (manifestIdentityEnvNames), whose values are not secret. partitionChildEnv
// refuses any other entry that carries one of these values, so a model key
// filed under a second name — a provider-native spelling a control plane
// fans out beside the injected provider's key, say — reaches neither the
// exec environment nor the environment section the policy extension
// restores for pi's tools.
func sessionCredentialValues(composed []string) map[string]struct{} {
	values := make(map[string]struct{})
	for _, entry := range composed {
		i := strings.IndexByte(entry, '=')
		if i <= 0 {
			continue
		}
		name, value := entry[:i], entry[i+1:]
		if _, identity := manifestIdentityEnvNames[name]; identity || value == "" || !isSessionCredentialName(name) {
			continue
		}
		values[value] = struct{}{}
	}
	return values
}

// sessionChildEnv partitions the session environment against this process's
// own environment, the parent every pi child would otherwise inherit.
func sessionChildEnv(spec agent.Spec) childEnvPartition {
	return partitionChildEnv(os.Environ(), spec)
}

// withoutTerminalEnv drops the parent's terminal descriptors. They describe
// the process that launched the harness, not the PTY the interactive child
// runs in; the PTY host supplies its own interactive defaults, and only an
// explicit Spec.Env entry may replace them.
func withoutTerminalEnv(parent []string) []string {
	out := make([]string, 0, len(parent))
	for _, entry := range parent {
		if strings.HasPrefix(entry, "TERM=") || strings.HasPrefix(entry, "COLORTERM=") {
			continue
		}
		out = append(out, entry)
	}
	return out
}
