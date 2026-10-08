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
// The same file carries a second, separate section: the session's other
// environment bindings (child_env.go). The child's exec environment is
// allowlisted, so a binding outside the allowlist that the agent's tools may
// still need rides here and the policy extension restores it into pi's
// in-process environment at load — never into the exec-time block a listing
// renders.
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
// so an env copy would be redundant exposure: refusing it (childEnvRefused)
// loses the child nothing it cannot read from the file.
const gatewayBearerEnvVar = gateway.TokenEnvVar

// manifestCredentialEnvNames transcribes the endpoint-manifest credential
// names (provider/endpoint/<company>/manifest.go, HostDesc.EnvKeys) that
// are NOT in builtinProviderCredentialEnv's value set and so would
// otherwise bypass the file rail: a bedrock/vertex/azure-style cell's
// binding Env rides applyEndpoint's merge onto Spec.Env. The exec
// environment is allowlisted (child_env.go), so such a name never reaches
// it either way — but a name in NEITHER set would be deferred to the
// credential file's environment section and restored into pi's in-process
// environment for its tools, where pi's own provider resolution reads it
// too. Listing it here keeps cell credentials on the credential rail. Only the
// manifest-declared names are listed: empty-string entries in those
// manifests declare no credential, and region/project/endpoint names
// (AWS_REGION, ANTHROPIC_VERTEX_PROJECT_ID, GOOGLE_VERTEX_PROJECT_ID,
// AZURE_OPENAI_ENDPOINT) are transcribed alongside the key names — they
// are binding identity the child never needs, and a future rotation that
// moves secret material under one of them must not silently re-open the
// listing hole. manifestEnvKeysCovered pins this list against the live
// manifests, so a new manifest EnvKeys entry breaks the build instead of
// leaking.
//
//nolint:gosec // G101: env-var NAMES, never credential bytes.
var manifestCredentialEnvNames = []string{
	"AWS_ACCESS_KEY_ID",
	"AWS_SECRET_ACCESS_KEY",
	"AWS_REGION",
	"GOOGLE_API_KEY",
	"GOOGLE_APPLICATION_CREDENTIALS",
	"GOOGLE_VERTEX_PROJECT_ID",
	"ANTHROPIC_VERTEX_PROJECT_ID",
	"AZURE_OPENAI_API_KEY",
	"AZURE_OPENAI_ENDPOINT",
}

// manifestIdentityEnvNames are the manifestCredentialEnvNames entries that
// name a binding's identity (region, project, endpoint) rather than secret
// material. They stay on the credential rail with the rest, but their values
// are not secrets, so the value refusal (sessionCredentialValues) does not
// reach other bindings that happen to share one: a tool's own region or
// project setting keeps arriving.
var manifestIdentityEnvNames = map[string]struct{}{
	"AWS_REGION":                  {},
	"GOOGLE_VERTEX_PROJECT_ID":    {},
	"ANTHROPIC_VERTEX_PROJECT_ID": {},
	"AZURE_OPENAI_ENDPOINT":       {},
}

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

// sessionCredentialFile is the on-disk shape: a versioned envelope around two
// ordered lists that are never mixed. Credentials are the session's model
// credentials; only the extension reads them, and it reads only the
// injected provider's key. Environment is the session's other bindings,
// kept out of the allowlisted exec environment (child_env.go); the
// extension restores them into pi's in-process environment so the tools pi
// starts inherit them. A reader that predates the environment section
// ignores it.
type sessionCredentialFile struct {
	SchemaVersion int                 `json:"schemaVersion"`
	Credentials   []sessionCredential `json:"credentials"`
	Environment   []sessionCredential `json:"environment,omitempty"`
}

// credentialFilePath returns the absolute path of the session credential
// file for layout.
func credentialFilePath(layout sessionLayout) string {
	return filepath.Join(layout.root, credentialFileName)
}

// sessionCredentialNames is the closed model-credential name set both spawn
// lanes refuse from the child in every form other than the credential
// section of the credential file (childEnvRefused): the injected-provider
// key (PiKeyEnvVar), the gateway binding bearer (gatewayBearerEnvVar), every
// provider-native credential var (builtinProviderCredentialEnv)
// applyEndpoint may mirror onto Spec.Env, plus every endpoint-manifest
// cell-credential name (manifestCredentialEnvNames) a bedrock/vertex/
// azure-style binding Env rides in on through applyEndpoint's merge.
// Deterministic (sorted) order;
// the set is derived once from the same sources sessionCredentialEntries
// fans out, so the two can never disagree about which names are
// credential-carrying.
func sessionCredentialNames() []string {
	names := make([]string, 0, len(builtinProviderCredentialEnv)+2+len(manifestCredentialEnvNames))
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
	for _, name := range manifestCredentialEnvNames {
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
// entries — partitionChildEnv routes them (child_env.go). An empty value
// never becomes an entry: an unset credential must read as absent in the
// child, not as an empty key.
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
// provider key — so it never reaches the pi child in any form on either
// lane.
const workerAuthTokenEnvVar = "WORKER_AUTH_TOKEN" //nolint:gosec // G101: env-var NAME, never credential bytes.

// undeclaredSecretValueNames are Spec.Env names outside the session
// credential rail that are known to carry secret values: the worker runtime
// bearer above, the generic custom-binding key spellings a control plane
// may use for a cell credential that is not one of pi's built-in-provider
// vars, and the provider-native key name of a model provider pi does not
// ship, which a control plane files beside the injected provider's key.
// They are refused from the pi child in every form
// (childEnvRefused): neither the exec environment nor either section of the
// credential file carries them — the credential section holds model-route
// credentials the extension reads, and the environment section holds
// bindings the agent's tools may use, and neither of these is one.
//
//nolint:gosec // G101: env-var NAMES, never credential bytes.
var undeclaredSecretValueNames = []string{
	workerAuthTokenEnvVar,
	"API_KEY",
	"CUSTOM_SECRET_KEY",
	"META_API_KEY",
}

// writeSessionCredentialFile writes entries (the session's model
// credentials) and environment (its deferred non-credential bindings,
// child_env.go) as the session credential file under layout's root, mode
// 0600, through the session-state writer (which refuses a planted link
// instead of following it out of the set). With both lists empty it writes
// no file and returns "": the session has nothing to deliver, and the child
// must read the key as absent rather than as an empty file. The caller
// composes the returned path onto the child env under credentialFileEnvVar;
// the caller owns removal (Stop on both lanes).
func writeSessionCredentialFile(layout sessionLayout, entries, environment []sessionCredential) (string, error) {
	if len(entries) == 0 && len(environment) == 0 {
		return "", nil
	}
	if entries == nil {
		entries = []sessionCredential{}
	}
	payload, err := json.Marshal(sessionCredentialFile{SchemaVersion: 1, Credentials: entries, Environment: environment})
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

// readSessionEnvironmentMap decodes the environment section of
// credential-file bytes into a name-to-value map, for tests.
func readSessionEnvironmentMap(raw []byte) (map[string]string, error) {
	var file sessionCredentialFile
	if err := json.Unmarshal(raw, &file); err != nil {
		return nil, err
	}
	out := make(map[string]string, len(file.Environment))
	for _, e := range file.Environment {
		out[e.Env] = e.Value
	}
	return out, nil
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
	// encoding/json escapes every control byte and orders map keys, so the
	// file is valid JSON for any credential value and byte-stable per spec.
	store := make(map[string]nativeProviderAuthEntry, len(entries))
	for provider, key := range entries {
		store[provider] = nativeProviderAuthEntry{Type: "api_key", Key: key}
	}
	payload, err := json.Marshal(store)
	if err != nil {
		return "", fmt.Errorf("pi: encode native provider auth file: %w", err)
	}
	path := filepath.Join(layout.agentHome, "auth.json")
	if err := writeStateFile(layout, path, payload, 0o600); err != nil {
		return "", fmt.Errorf("pi: write native provider auth file: %w", err)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		_ = os.Remove(path)
		return "", fmt.Errorf("pi: secure native provider auth file: %w", err)
	}
	return path, nil
}

// nativeProviderAuthEntry is one provider's entry in pi's auth.json store
// (docs/providers.md: per-provider api_key entries).
type nativeProviderAuthEntry struct {
	Type string `json:"type"`
	Key  string `json:"key"`
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
// key, the gateway binding bearer, every provider-native credential var
// (builtinProviderCredentialEnv), plus every endpoint-manifest
// cell-credential name (manifestCredentialEnvNames) — wherever they
// arrive: on Spec.Env OR inherited from the parent process (an embedding
// supervisor may re-admit a blocklisted name it injected itself through
// InjectedEnvKeysVar). Tests use it to assert the spawned child's env holds
// no model credential by name.
func isSessionCredentialEnv(entry string) bool {
	key := entry
	if i := strings.IndexByte(entry, '='); i >= 0 {
		key = entry[:i]
	}
	return isSessionCredentialName(key)
}

// isSessionCredentialName reports whether name is one of the
// sessionCredentialNames — a model credential the session reaches only
// through the credential file and the session auth.json.
func isSessionCredentialName(name string) bool {
	_, ok := sessionCredentialNameSet[name]
	return ok
}

// sessionCredentialNameSet is sessionCredentialNames as a set, built once:
// the partition consults it for every entry of every spawn's environment.
var sessionCredentialNameSet = func() map[string]struct{} {
	names := sessionCredentialNames()
	set := make(map[string]struct{}, len(names))
	for _, name := range names {
		set[name] = struct{}{}
	}
	return set
}()

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
