package pi

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/RenseiAI/donmai/agent"
	"github.com/RenseiAI/donmai/gateway"
)

// This file moves session credentials off the pi harness child's environment
// and into a session-scoped credential file the child reads at load.
//
// Why: pi renames its own process (node's process.title) to a short name at
// startup, and on some platforms a same-user process listing then renders the
// child's ENVIRONMENT as if it were its command line — every secret composed
// into the child env is readable host-wide, and leaks into terminals, logs
// and transcripts. Argv is already clean (the fleet token and the gateway
// helper moved off it); the child env is the remaining surface. So no secret
// value is ever composed into the pi child env: credential bytes ride an
// owner-only file inside the session state root, whose PATH rides the child
// env instead. pi's own auth.json file mechanism covers the native-provider
// route (docs/providers.md: per-provider api_key entries under the
// PI_CODING_AGENT_DIR home); the injected "donmai" provider's key rides the
// same file through the command-execution key form, so the parent never
// re-serializes the key into env at any layer.
//
// The file is written before spawn and removed at session end (Stop on both
// lanes, and the PTY cleanup chain on the interactive lane), through the
// session-state writer so a planted link refuses the spawn instead of
// redirecting the bytes.

// credentialFileName is the session credential file's basename inside the
// session state root. It sits beside the materialized boundary extension
// (never under an auto-discovered location) so the worktree lifecycle that
// removes the state root removes it with it.
const credentialFileName = "session-credentials.json"

// credentialFileEnvVar carries the credential file's absolute path onto the
// child. It names a PATH, never a secret, so it is safe to compose into the
// child env on both spawn lanes.
const credentialFileEnvVar = "DONMAI_PI_CREDENTIALS_FILE" //nolint:gosec // G101: env-var NAME, never credential bytes.

// gatewayBearerEnvVar is the env-var NAME the worker-local translating
// gateway's per-session bearer rides under when it arrives on the spec.
// It aliases gateway.TokenEnvVar (same string, one compiler-checked
// definition): a gateway rename that moves the constant breaks this
// package's build instead of silently re-opening the child-env hole this
// rail exists to close. The bearer is a per-session secret — a same-user
// process listing of the renamed pi child renders it exactly like a
// provider key — so it rides the session credential file like every other
// session credential, never the child env. The file rail carries the same
// bearer under PiKeyEnvVar (applyEndpoint mirrors the binding key there),
// so the env copy is redundant exposure: stripping it loses the child
// nothing it cannot read from the file.
const gatewayBearerEnvVar = gateway.TokenEnvVar

// sessionCredential is one named credential the file carries: the env-var
// name the value would have ridden under, and the value itself.
type sessionCredential struct {
	// Env is the variable name the credential rides under in the extension's
	// process environment (PiKeyEnvVar for the injected provider, or the
	// provider-native credential var for a native route).
	Env string `json:"env"`
	// Value is the credential itself.
	Value string `json:"value"`
}

// sessionCredentialFile is the on-disk shape: a versioned envelope around an
// ordered credential list, so a future entry kind can be told apart from
// today's without guessing.
type sessionCredentialFile struct {
	SchemaVersion int                 `json:"schemaVersion"`
	Credentials   []sessionCredential `json:"credentials"`
}

// stripCredentialNamedEnv drops every child-env entry whose NAME is a
// session credential name (sessionCredentialNames) or an undeclared
// secret-valued name (undeclaredSecretValueNames), wherever it came from —
// Spec.Env or the inherited parent. The session's own rail credential values
// ride the credential file instead; an inherited copy is never the session's
// credential, so dropping it loses nothing the child is entitled to read.
// Undeclared secret values are dropped outright (they ride neither the file
// nor the env on this harness).
func stripCredentialNamedEnv(childEnv []string) []string {
	out := childEnv[:0]
	for _, entry := range childEnv {
		if !isSessionCredentialEnv(entry) && !isUndeclaredSecretValueEnv(entry) {
			out = append(out, entry)
		}
	}
	// Zero the tail so dropped credential values do not linger in the
	// backing array (the slice is freshly composed per spawn, but the
	// array is still addressable memory until GC).
	for i := len(out); i < len(childEnv); i++ {
		childEnv[i] = ""
	}
	return out
}

// credentialFilePath returns the absolute path of the session credential
// file for layout.
func credentialFilePath(layout sessionLayout) string {
	return filepath.Join(layout.root, credentialFileName)
}

// sessionCredentialNames is the closed credential-name set both spawn lanes
// strip from the child env: the injected-provider key (PiKeyEnvVar), the
// gateway binding bearer (gatewayBearerEnvVar), plus every provider-native
// credential var (builtinProviderCredentialEnv) applyEndpoint may mirror
// onto Spec.Env. Deterministic (sorted) order; the set is derived once
// from the same sources sessionCredentialEntries fans out, so the two can
// never disagree about which names are credential-carrying.
func sessionCredentialNames() []string {
	names := make([]string, 0, len(builtinProviderCredentialEnv)+2)
	names = append(names, PiKeyEnvVar, gatewayBearerEnvVar)
	for _, name := range builtinProviderCredentialEnv {
		duplicate := false
		for _, have := range names {
			if have == name {
				duplicate = true
				break
			}
		}
		if !duplicate {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	return names
}

// sessionCredentialEntries fans the session's credential values out of the
// spec: every sessionCredentialNames entry present with a non-empty value,
// in deterministic (sorted) order. Non-credential Spec.Env bindings are NOT
// entries — they keep riding the child env unchanged through
// composeChildEnv. An empty value never becomes an entry: an unset
// credential must read as absent in the child, not as an empty key that
// shadows nothing.
func sessionCredentialEntries(spec agent.Spec) []sessionCredential {
	names := sessionCredentialNames()
	var out []sessionCredential
	for _, name := range names {
		if value, ok := spec.Env[name]; ok && value != "" {
			out = append(out, sessionCredential{Env: name, Value: value})
		}
	}
	return out
}

// workerAuthTokenEnvVar is the worker's platform runtime bearer
// (heartbeat, result post, session preflight). It is supervisor authority
// consumed by in-process runner code, never by the harness child — and a
// same-user listing of the renamed pi child renders it exactly like a
// provider key — so it never rides the pi child env on either lane.
const workerAuthTokenEnvVar = "WORKER_AUTH_TOKEN" //nolint:gosec // G101: env-var NAME, never credential bytes.

// undeclaredSecretValueNames are Spec.Env names outside the session
// credential rail that are known to carry secret values: the worker runtime
// bearer above, plus the generic custom-binding key spellings a control
// plane may use for a cell credential that is not one of pi's
// built-in-provider vars. They are refused from the pi child env on both
// lanes (never fanned out to the credential file: the file rail carries
// model-route credentials the extension reads, and neither of these is
// one). Documented non-secret bindings pass through untouched.
//
//nolint:gosec // G101: env-var NAMES, never credential bytes.
var undeclaredSecretValueNames = []string{
	workerAuthTokenEnvVar,
	"API_KEY",
	"CUSTOM_SECRET_KEY",
}

// stripSessionCredentialEnv returns a copy of spec whose Env no longer
// carries credential values: every sessionCredentialEntries name plus every
// undeclaredSecretValueNames entry is dropped. composeChildEnv and
// interactiveChildEnv build the child env from the stripped spec, so no
// secret value reaches the child env on either lane; the dropped rail
// values ride the credential file instead. Non-credential bindings survive
// untouched, and a spec with no credentials keeps a nil Env rather than
// gaining an empty map.
func stripSessionCredentialEnv(spec agent.Spec) agent.Spec {
	entries := sessionCredentialEntries(spec)
	if len(entries) == 0 && !hasUndeclaredSecretValue(spec) {
		return spec
	}
	drop := make(map[string]struct{}, len(entries)+len(undeclaredSecretValueNames))
	for _, e := range entries {
		drop[e.Env] = struct{}{}
	}
	for _, name := range undeclaredSecretValueNames {
		drop[name] = struct{}{}
	}
	out := spec
	env := make(map[string]string, len(spec.Env))
	for k, v := range spec.Env {
		if _, isCredential := drop[k]; !isCredential {
			env[k] = v
		}
	}
	out.Env = env
	return out
}

// hasUndeclaredSecretValue reports whether spec carries any
// undeclaredSecretValueNames entry, so the strip above can stay a no-op
// (nil Env preserved) for specs that carry neither rail credentials nor
// undeclared secrets.
func hasUndeclaredSecretValue(spec agent.Spec) bool {
	for _, name := range undeclaredSecretValueNames {
		if _, ok := spec.Env[name]; ok {
			return true
		}
	}
	return false
}

// isUndeclaredSecretValueEnv reports whether a child-env entry carries one
// of the undeclared secret-valued names. Tests use it alongside
// isSessionCredentialEnv to assert the spawned child's env holds no secret
// under any known-secret name.
func isUndeclaredSecretValueEnv(entry string) bool {
	key := entry
	if i := strings.IndexByte(entry, '='); i >= 0 {
		key = entry[:i]
	}
	for _, name := range undeclaredSecretValueNames {
		if key == name {
			return true
		}
	}
	return false
}

// writeSessionCredentialFile writes entries as the session credential file
// under layout's root, mode 0600, through the session-state writer (which
// refuses a planted link instead of following it out of the set). A nil
// entry list writes no file and returns "": a keyless session has no
// credential to deliver, and the child must read the key as absent rather
// than as an empty file. The caller composes the returned path onto the
// child env under credentialFileEnvVar; the caller owns removal (Stop on
// both lanes).
func writeSessionCredentialFile(layout sessionLayout, entries []sessionCredential) (string, error) {
	if len(entries) == 0 {
		return "", nil
	}
	payload, err := json.Marshal(sessionCredentialFile{SchemaVersion: 1, Credentials: entries})
	if err != nil {
		return "", fmt.Errorf("pi: encode session credential file: %w", err)
	}
	path := credentialFilePath(layout)
	if err := writeStateFile(layout, path, payload, 0o600); err != nil {
		return "", fmt.Errorf("pi: write session credential file: %w", err)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		_ = os.Remove(path)
		return "", fmt.Errorf("pi: secure session credential file: %w", err)
	}
	return path, nil
}

// removeSessionCredentialFile deletes the session credential file. It is
// idempotent: a missing file (keyless session, or already removed) is not
// an error. Only the exact session-owned path is ever removed — the join is
// recomputed from layout, never taken from caller input — so a malicious or
// mistaken path cannot redirect the delete.
func removeSessionCredentialFile(layout sessionLayout) {
	path := credentialFilePath(layout)
	if filepath.Clean(path) != path || layout.root == "" {
		return
	}
	_ = os.Remove(path)
}

// readSessionCredentialFile reads back the credential file at path for
// tests: it maps each entry's env name to its value. Production never reads
// the file back — only the pi child does, through the extension.
func readSessionCredentialFile(path string) (map[string]string, error) {
	raw, err := os.ReadFile(path) //nolint:gosec // G304: path is the session-owned credential file under test, not session input.
	if err != nil {
		return nil, err
	}
	return readSessionCredentialMap(raw)
}

// readSessionCredentialMap decodes credential-file bytes into the env-name
// map. Tests use it for both the on-disk file and the content a fake child
// captured while the file still existed (the PTY cleanup removes it on
// exit, so the path alone is unreadable after the child ends).
func readSessionCredentialMap(raw []byte) (map[string]string, error) {
	var file sessionCredentialFile
	if err := json.Unmarshal(raw, &file); err != nil {
		return nil, err
	}
	out := make(map[string]string, len(file.Credentials))
	for _, e := range file.Credentials {
		out[e.Env] = e.Value
	}
	return out, nil
}

// strconvQuote renders path as a double-quoted JavaScript string literal.
// The path is parent-owned (the session state root), never session content,
// and it is quoted rather than interpolated raw so a quote or backslash in
// it cannot break out of the literal.
func strconvQuote(path string) string {
	var b strings.Builder
	b.WriteByte('"')
	for _, r := range path {
		switch r {
		case '"', '\\':
			b.WriteByte('\\')
			b.WriteRune(r)
		case '\n':
			b.WriteString("\\n")
		case '\r':
			b.WriteString("\\r")
		case '\t':
			b.WriteString("\\t")
		default:
			b.WriteRune(r)
		}
	}
	b.WriteByte('"')
	return b.String()
}

// writeNativeProviderAuthFile writes the session agent home's auth.json so
// the NATIVE provider route (modelPinArgs' --provider <name> branch, whose
// credential pi resolves from its own store, not from the injected
// provider) authenticates without any secret in the child env. Every
// provider-native credential on the spec becomes one api_key entry under
// that provider's own slug; the injected-provider key is NOT an entry —
// the extension reads it from the credential file directly, so one key
// never lands in two stores. Missing providers, and
// providers without a spec credential, get no entry: pi falls back to its
// own resolution for those, exactly as it did when the key rode env.
//
// The file is written through the session-state writer (link-safe) at mode
// 0600, matching the permissions pi itself uses when it creates auth.json.
// It returns "" when there is nothing to write, so a session with no
// native-route credential keeps no auth.json at all and pi's own
// resolution is untouched.
func writeNativeProviderAuthFile(layout sessionLayout, spec agent.Spec) (string, error) {
	entries := make(map[string]string)
	for provider, envVar := range builtinProviderCredentialEnv {
		if value, ok := spec.Env[envVar]; ok && strings.TrimSpace(value) != "" {
			entries[provider] = value
		}
	}
	if len(entries) == 0 {
		return "", nil
	}
	names := make([]string, 0, len(entries))
	for provider := range entries {
		names = append(names, provider)
	}
	sort.Strings(names)
	var b strings.Builder
	b.WriteString("{")
	for i, provider := range names {
		if i > 0 {
			b.WriteString(",")
		}
		b.WriteString(strconvQuote(provider))
		b.WriteString(":{\"type\":\"api_key\",\"key\":")
		b.WriteString(strconvQuote(entries[provider]))
		b.WriteString("}")
	}
	b.WriteString("}")
	path := filepath.Join(layout.agentHome, "auth.json")
	if err := writeStateFile(layout, path, []byte(b.String()), 0o600); err != nil {
		return "", fmt.Errorf("pi: write native provider auth file: %w", err)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		_ = os.Remove(path)
		return "", fmt.Errorf("pi: secure native provider auth file: %w", err)
	}
	return path, nil
}

// removeNativeProviderAuthFile deletes the session agent home's auth.json.
// Idempotent: a session with no native-route credential wrote none.
func removeNativeProviderAuthFile(layout sessionLayout) {
	path := filepath.Join(layout.agentHome, "auth.json")
	if layout.agentHome == "" || filepath.Clean(path) != path {
		return
	}
	_ = os.Remove(path)
}

// sessionCredentialEnv composes the credential-file path entries onto a
// KEY=VALUE child env: credentialFileEnvVar names the file for the
// extension to read. Empty path appends nothing, so a keyless session's
// child env carries no credential-file pointer at all.
func sessionCredentialEnv(childEnv []string, path string) []string {
	if strings.TrimSpace(path) == "" {
		return childEnv
	}
	return append(childEnv, credentialFileEnvVar+"="+path)
}

// isSessionCredentialEnv reports whether a child-env entry carries a
// session credential VALUE (as opposed to the credential file's path).
// It covers exactly the sessionCredentialNames set — the injected-provider
// key, the gateway binding bearer, plus every provider-native credential
// var (builtinProviderCredentialEnv) — wherever they arrive: on Spec.Env OR
// inherited from the parent process (an embedding supervisor may re-admit a
// blocklisted name it injected itself through InjectedEnvKeysVar, and the
// gateway's own upstream refusal does not cover every snapshot key). Tests
// use it to assert the spawned child's env holds no secret.
func isSessionCredentialEnv(entry string) bool {
	key := entry
	if i := strings.IndexByte(entry, '='); i >= 0 {
		key = entry[:i]
	}
	for _, name := range sessionCredentialNames() {
		if key == name {
			return true
		}
	}
	return false
}

// sessionCredentialValuePresent reports whether any of values appears in the
// child env's VALUES (not keys). Tests use it with the sentinel credential
// to assert the spawn delivered the secret through the file, never env.
func sessionCredentialValuePresent(childEnv []string, values ...string) bool {
	for _, entry := range childEnv {
		i := strings.IndexByte(entry, '=')
		if i < 0 {
			continue
		}
		value := entry[i+1:]
		for _, want := range values {
			if want != "" && strings.Contains(value, want) {
				return true
			}
		}
	}
	return false
}
