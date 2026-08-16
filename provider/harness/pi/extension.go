package pi

import (
	"crypto/sha256"
	"crypto/subtle"
	"embed"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/RenseiAI/donmai/agent"
)

// embeddedExtensions ships the donmai policy extension INSIDE the donmai
// binary. It is never fetched from the network (design §5.1). The materialized
// copy's SHA is verified against these bytes at handshake time.
//
//go:embed extensions/donmai-policy.ts
var embeddedExtensions embed.FS

const (
	// extensionFileName is the extension source file name, both embedded and
	// materialized.
	extensionFileName = "donmai-policy.ts"

	// piStateDir is the per-session pi state directory materialized inside the
	// worktree so the runner's worktree lifecycle owns it (design §2 step 1,
	// §5.3 env hygiene). The policy extension is written at its ROOT (not under
	// an auto-discovered extensions/ subdir): the harness loads it explicitly
	// with `pi --mode rpc -e <path> --no-extensions`, so keeping it out of any
	// auto-scan location prevents a double-load.
	piStateDir = ".pi"

	// donmaiUIMarker is the extension_ui_request placeholder the policy
	// extension stamps on every trust-boundary round-trip. The pump dispatches
	// only requests carrying it (extensions/donmai-policy.ts DONMAI_UI_MARKER).
	donmaiUIMarker = "donmai-policy-v1"

	// handshakeKind / adjudicateKind are the discriminators inside the JSON
	// payload the extension puts in the extension_ui_request `title`
	// (extensions/donmai-policy.ts KIND_*).
	handshakeKind  = "handshake"
	adjudicateKind = "adjudicate"

	// Provider-pin env vars the child extension reads to register the single
	// "donmai" provider (extensions/donmai-policy.ts). The API key rides
	// PiKeyEnvVar; these carry the non-secret routing pin. The context-window
	// pin is conditional: set only when the resolved profile carries a
	// positive ProviderConfig["contextWindow"], so an unpinned session keeps
	// the extension's built-in default.
	piBaseURLEnvVar       = "DONMAI_PI_BASE_URL"
	piAPIEnvVar           = "DONMAI_PI_API"
	piModelEnvVar         = "DONMAI_PI_MODEL"
	piContextWindowEnvVar = "DONMAI_PI_CONTEXT_WINDOW"
	piHandshakeEnvVar     = "DONMAI_PI_HANDSHAKE"

	// injectedExtensionsDir holds materialized agent.ExtensionDelivery
	// (Kind == ExtensionDeliveryInline) artifacts, one level under piStateDir
	// (ADR-2026-08-12 D1). Kept out of piStateDir's root so it never collides
	// with extensionFileName, and out of any auto-discovered location for the
	// same reason the boundary extension is: every entry loads by explicit
	// `-e` path with `--no-extensions` disabling discovery, so nothing here
	// is ever found by scanning.
	injectedExtensionsDir = "extensions-injected"

	// agentHomeDir holds the per-session PI_CODING_AGENT_DIR target, one
	// level under piStateDir and deliberately distinct from it — see
	// sessionLayout.agentHome's doc comment for why collapsing the two
	// breaks pi's own session-resume lookup.
	agentHomeDir = "agent-home"
)

// extensionSource returns the embedded extension bytes.
func extensionSource() []byte {
	b, err := embeddedExtensions.ReadFile("extensions/" + extensionFileName)
	if err != nil {
		// The embed is compiled in; a read error is a build-integrity bug, not
		// a runtime condition. Return nil so extensionSHA() yields a SHA that
		// can never match a real file — fail-closed.
		return nil
	}
	return b
}

// extensionSHA returns the lowercase hex sha256 of the embedded extension.
// This is the value the handshake must match — the extension hashes its own
// on-disk source (import.meta.url) and reports it; the two must agree.
func extensionSHA() string {
	sum := sha256.Sum256(extensionSource())
	return hex.EncodeToString(sum[:])
}

// verifyHandshakeSHA reports whether claimed matches the embedded extension's
// SHA, using a constant-time compare. An empty claim never matches (fail
// closed): a stale/absent extension cannot silently pass.
func verifyHandshakeSHA(claimed string) bool {
	if claimed == "" {
		return false
	}
	want := extensionSHA()
	return subtle.ConstantTimeCompare([]byte(claimed), []byte(want)) == 1
}

// verifyHandshakeToken reports whether claimed matches the per-session token
// the harness set in the child env (piHandshakeEnvVar), using a constant-time
// compare. An empty claim or empty want never matches (fail closed): the token
// proves the request comes from the child the harness actually spawned, with
// the env it set — an unrelated/foreign extension cannot forge it.
func verifyHandshakeToken(claimed, want string) bool {
	if claimed == "" || want == "" {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(claimed), []byte(want)) == 1
}

// sessionLayout describes where a session's pi state lives inside the
// worktree. All paths are under <cwd>/.pi so the runner's worktree lifecycle
// owns cleanup.
type sessionLayout struct {
	root      string // <cwd>/.pi
	extension string // <cwd>/.pi/donmai-policy.ts (loaded via -e; NOT auto-discovered)
	// injected is where INLINE agent.ExtensionDelivery entries are
	// materialized (ADR-2026-08-12 D1). It is a subdirectory of root so
	// cleanup stays workarea-owned exactly like the boundary extension file
	// (D1.3) — the same worktree lifecycle that removes .pi removes this too.
	// PATH-kind deliveries are never written here; the caller already owns
	// that file's lifecycle (D1: "the file already exists at a path the
	// runner can read").
	injected string // <cwd>/.pi/extensions-injected
	// agentHome is the per-session PI_CODING_AGENT_DIR target (config, auth,
	// model catalog — ADR-2026-08-12 D4). It is DELIBERATELY a subdirectory
	// of root rather than root itself: pi's own session lookup (`--session
	// <id>`) resolves local sessions by consulting BOTH the config/agent
	// directory and the session-storage directory, and pointing both env
	// vars at the exact same path breaks that lookup — a session written
	// under root fails to resolve on --session <id> when
	// PI_CODING_AGENT_DIR == PI_CODING_AGENT_SESSION_DIR == root (verified
	// against the real pinned binary: the collision reproduces "No session
	// found matching '<id>'" on resume even though the session file exists
	// on disk at the expected path — see the resume real-binary fixtures).
	// Kept apart, both isolation goals still hold: agentHome never leaks
	// into the invoking user's ~/.pi/agent, and root still carries session
	// storage exactly as it always did.
	agentHome string // <cwd>/.pi/agent-home
}

func newSessionLayout(cwd string) sessionLayout {
	root := filepath.Join(cwd, piStateDir)
	return sessionLayout{
		root:      root,
		extension: filepath.Join(root, extensionFileName),
		injected:  filepath.Join(root, injectedExtensionsDir),
		agentHome: filepath.Join(root, agentHomeDir),
	}
}

// materializeExtension writes the embedded policy extension into the session
// worktree so the harness can load it with `-e`. Returns the layout so the
// caller can point pi at it and later verify the handshake.
//
// Fail-closed: any write error is returned; the caller must NOT spawn a
// prompt when materialization fails. The provider pin (design §6) is delivered
// to the extension via env (piBaseURLEnvVar/piAPIEnvVar/piModelEnvVar +
// PiKeyEnvVar), never written to disk — so the extension's on-disk source
// stays byte-identical to the embedded payload and its SHA verifies.
func materializeExtension(cwd string) (sessionLayout, error) {
	layout := newSessionLayout(cwd)
	if err := os.MkdirAll(layout.root, 0o700); err != nil {
		return layout, fmt.Errorf("pi: create state dir: %w", err)
	}
	// The boundary extension's bytes are BYTE-IDENTICAL across every session
	// this process spawns (it is compiled into the binary — extensionSource()
	// never varies), so every concurrent spawn is writing the same content to
	// a different session-local path. writeViaCache reuses a shared,
	// content-addressed blob via hardlink instead of re-encoding+writing the
	// same bytes per spawn (a cold-start optimization — see writeViaCache's
	// doc comment for why this changes nothing about correctness): the digest
	// verification below reads back whatever actually landed at
	// layout.extension and is unaffected by whether the write took the cache
	// path or the fresh-write fallback.
	if err := writeViaCache(extensionSHA(), extensionSource(), layout.extension, 0o600); err != nil {
		return layout, fmt.Errorf("pi: write policy extension: %w", err)
	}
	return layout, nil
}

// materializeAdditionalExtensions realizes spec.AdditionalExtensions
// (ADR-2026-08-12 D1) into an ordered list of load-ready paths, one per
// delivery, in the caller's declared order. It is the seam half of D1/D2:
//
//   - structural validation first (agent.ValidateExtensionDeliveries) — a
//     malformed entry denies before any filesystem work;
//   - PATH deliveries are read from the caller-supplied absolute path;
//     INLINE deliveries are written under layout.injected;
//   - every delivery — path or inline — is then READ BACK from wherever it
//     actually landed and its sha256 compared against the required Digest.
//     This is the TOCTOU rule (D2(b)): verification happens AFTER
//     materialization, against what is actually loadable, never against
//     Source or the caller's claim. Any failure — missing file, unreadable
//     path, digest mismatch — fails closed with a typed error and returns no
//     usable paths; there is no warn-and-strip path (D1.2).
//
// The caller is responsible for loading the boundary extension (the pi
// policy extension) FIRST and prepending it ahead of this list's output —
// this function only ever handles the additional deliveries themselves, so
// it cannot accidentally reorder or displace the boundary (D1: "the policy
// extension is always first and cannot be displaced, reordered, or disabled
// by a delivery").
func materializeAdditionalExtensions(layout sessionLayout, deliveries []agent.ExtensionDelivery) ([]string, error) {
	if len(deliveries) == 0 {
		return nil, nil
	}
	if err := agent.ValidateExtensionDeliveries(deliveries); err != nil {
		return nil, fmt.Errorf("pi: additional extension delivery: %w", err)
	}

	paths := make([]string, 0, len(deliveries))
	for _, d := range deliveries {
		var loadPath string
		switch d.Kind {
		case agent.ExtensionDeliveryPath:
			loadPath = d.Path
		case agent.ExtensionDeliveryInline:
			if err := os.MkdirAll(layout.injected, 0o700); err != nil {
				return nil, fmt.Errorf("pi: additional extension %q: create injected-extensions dir: %w", d.ID, err)
			}
			loadPath = filepath.Join(layout.injected, sanitizeInjectedBasename(d.ID, d.Basename))
			// A capability pack admitted for one cell is typically byte-
			// identical across every session the fleet spawns under that
			// admission — the exact fan-out shape this cache targets. Reuse
			// the shared content-addressed cache the same way the boundary
			// extension does (writeViaCache), keyed on the ALREADY-VALIDATED
			// digest (agent.ValidateExtensionDeliveries ran above and
			// guarantees d.Digest is a well-formed lowercase sha256 hex
			// string, so it is safe to use directly as a cache filename).
			if err := writeViaCache(d.Digest, d.Source, loadPath, 0o600); err != nil {
				return nil, fmt.Errorf("pi: additional extension %q: materialize: %w", d.ID, err)
			}
		}

		// Verify AFTER materialization, against what is actually on disk —
		// the TOCTOU rule (D2(b)). A path delivery is read here for the
		// first time; an inline delivery is read back rather than trusted
		// from the bytes this process just wrote, so a concurrent rewrite of
		// either lands the same fail-closed outcome.
		loaded, err := os.ReadFile(loadPath) //nolint:gosec // G304: loadPath is either the caller's declared absolute Path (an operator-injected delivery, per D2(a) never workarea/network content) or a path this function just wrote under layout.injected; both are intentional, and the read is immediately digest-verified below rather than trusted.
		if err != nil {
			return nil, fmt.Errorf("pi: additional extension %q: read for verification: %w", d.ID, err)
		}
		if !agent.VerifyExtensionDigest(loaded, d.Digest) {
			return nil, fmt.Errorf("pi: additional extension %q: digest mismatch — refusing to load unverified content", d.ID)
		}
		paths = append(paths, loadPath)
	}
	return paths, nil
}

// sanitizeInjectedBasename returns a filesystem-safe filename for an inline
// delivery's materialized artifact. agent.ValidateExtensionDelivery already
// rejects a Basename containing path separators or "."/".." before this is
// called; the id prefix additionally guarantees uniqueness across deliveries
// that happen to share a basename (agent.ValidateExtensionDeliveries already
// requires unique ids).
func sanitizeInjectedBasename(id, basename string) string {
	return id + "-" + filepath.Base(basename)
}

// --- Content-addressed materialization cache (cold-start hardening) ---
//
// A fleet spawns many sessions whose extension bytes are digest-IDENTICAL:
// the boundary extension always is (one embedded file, one binary), and an
// admitted capability pack usually is (the same grant produces the same
// tool list across every session it is admitted for). Rewriting the same
// bytes to a fresh per-session path on every single spawn is pure overhead
// at N-instance scale. The workarea cleanup rules (ADR-2026-08-12 D1.3) let a
// shared, content-addressed cache sit outside any one session's workarea —
// nothing about D1.3 requires the SOURCE of a session's materialized bytes to
// be a fresh write, only that the session-owned copy exists, is cleaned up by
// the session's own lifecycle, and is digest-verified before load. This cache
// changes ONLY the first of those (a hardlink from a shared blob instead of a
// fresh encode+write); the session-local file, its cleanup, and the
// mandatory post-materialization digest read-back (D2(b)'s TOCTOU rule) are
// completely unchanged — a cache hit and a cache miss produce an
// indistinguishable session-local file from the verifier's point of view.
//
// Digest, not content, is the cache key: agent.ExtensionDelivery.Digest is
// validated to be a well-formed lowercase sha256 hex string before this ever
// runs (agent.ValidateExtensionDeliveries), and the boundary extension's key
// is extensionSHA() — a value this package computes, never caller input. A
// hash collision producing a false cache hit is not a threat this cache
// introduces: even a poisoned or corrupted cache entry is caught by the
// unchanged post-write digest verification in the caller, because that
// verification reads back the session-local file, not the cache blob.

// piExtCacheDirEnvVar overrides the shared materialization cache directory —
// primarily for tests; operators who want it elsewhere (e.g. a tmpfs) may set
// it too. Unset defaults to a fixed subdirectory of os.TempDir(), deliberately
// OUTSIDE every session's workarea so no single session's cleanup removes
// bytes another live session may still be linking against.
const piExtCacheDirEnvVar = "DONMAI_PI_EXT_CACHE_DIR"

// extensionCache is a content-addressed store of materialized extension
// bytes, keyed by sha256 hex digest. Safe for concurrent use by any number of
// goroutines spawning sessions in parallel. It deliberately does NOT memoize
// its directory (no sync.Once): the env override is re-read and MkdirAll
// re-run on every call, both idempotent and cheap relative to a process
// spawn, which keeps DONMAI_PI_EXT_CACHE_DIR honest for a caller that sets it
// per-test rather than at process start (a memoized directory would make the
// override race the first caller instead of always winning).
type extensionCache struct{}

var globalExtensionCache = &extensionCache{}

// dirPath resolves (and ensures) the cache directory.
func (c *extensionCache) dirPath() (string, error) {
	dir := os.Getenv(piExtCacheDirEnvVar)
	if dir == "" {
		dir = filepath.Join(os.TempDir(), "donmai-pi-ext-cache")
	}
	//nolint:gosec // G703: dir is an operator/test-configured cache-directory
	// override (DONMAI_PI_EXT_CACHE_DIR), not request- or session-derived
	// input; unset defaults to a fixed os.TempDir() subpath this package
	// controls.
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	return dir, nil
}

// blobPath returns the cache blob path for digest, and whether a regular
// file already exists there.
func (c *extensionCache) blobPath(digest string) (path string, hit bool) {
	dir, err := c.dirPath()
	if err != nil || digest == "" {
		return "", false
	}
	// digest is always caller-validated (sha256 hex, fixed charset/length)
	// before reaching here; filepath.Base is cheap defense-in-depth, not the
	// only guard.
	path = filepath.Join(dir, filepath.Base(digest))
	fi, statErr := os.Stat(path)
	return path, statErr == nil && fi.Mode().IsRegular()
}

// populate best-effort publishes destPath's just-written bytes into the
// cache under digest, via hardlink-then-atomic-rename so a concurrent reader
// never observes a partially-written blob. Every failure is swallowed: the
// per-session materialization this backs up has ALREADY succeeded by the
// time populate runs, so a cache-population failure can only cost a future
// spawn's fast path, never today's correctness.
func (c *extensionCache) populate(digest, destPath string) {
	dir, err := c.dirPath()
	if err != nil || digest == "" {
		return
	}
	blob := filepath.Join(dir, filepath.Base(digest))
	if fi, statErr := os.Stat(blob); statErr == nil && fi.Mode().IsRegular() {
		return // already populated (by this call or a concurrent one)
	}
	tmp := blob + ".tmp-" + strconv.FormatInt(time.Now().UnixNano(), 36) + "-" + strconv.Itoa(os.Getpid())
	if err := os.Link(destPath, tmp); err != nil {
		return // best-effort: e.g. cross-device; the fresh write already succeeded
	}
	if err := os.Rename(tmp, blob); err != nil {
		_ = os.Remove(tmp)
	}
}

// writeViaCache materializes content at destPath, reusing the shared cache
// when a byte-identical blob already exists under digest: a hardlink
// replaces a full write. On any cache miss or cache-path failure it falls
// back to a normal write and best-effort populates the cache for the next
// caller. Correctness NEVER depends on the cache — only the caller's
// mandatory post-write digest verification (unchanged by this function) does
// — so a hardlink failure, a missing/corrupt blob, or a disabled cache
// directory degrade to "no caching", never to a wrong result.
//
// destPath must not exist, or must be safe to overwrite (matches
// os.WriteFile's overwrite semantics): any pre-existing file at destPath is
// removed first so os.Link (which fails on an existing target) behaves the
// same as os.WriteFile would have.
func writeViaCache(digest string, content []byte, destPath string, perm os.FileMode) error {
	if err := os.Remove(destPath); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("pi: replace %s: %w", destPath, err)
	}
	if blob, hit := globalExtensionCache.blobPath(digest); hit {
		if err := os.Link(blob, destPath); err == nil {
			return nil
		}
		// Fall through to a fresh write on any link failure (cross-device,
		// blob vanished between Stat and Link, permission, …).
	}
	if err := os.WriteFile(destPath, content, perm); err != nil {
		return err
	}
	globalExtensionCache.populate(digest, destPath)
	return nil
}

// providerPinEnv builds the non-secret routing-pin env the child extension
// reads to register the single "donmai" provider (design §6). The API key is
// NOT here — it rides PiKeyEnvVar (applyEndpoint mirrors the resolved cell key
// onto it). Under a gateway cell the baseURL is the local gateway binding, so
// pi inherits the whole mesh through one pin.
//
// The context-window pin (piContextWindowEnvVar) is appended only when the
// resolved profile carries a positive ProviderConfig["contextWindow"] — the
// key the dispatch bridge produces for both the resolvedProfile and
// modelProfile wire shapes. Without it the extension keeps its built-in
// default, so a control plane that resolved a 1M-context model is no longer
// silently clamped to that default.
func providerPinEnv(spec agent.Spec) []string {
	ep := spec.Endpoint
	model := spec.Model
	baseURL := ""
	api := "openai-completions"
	if ep != nil {
		baseURL = ep.BaseURL
		api = piAPIForProtocol(ep.Protocol)
		if ep.Model != "" {
			model = ep.Model
		}
	}
	out := []string{
		piBaseURLEnvVar + "=" + baseURL,
		piAPIEnvVar + "=" + api,
		piModelEnvVar + "=" + model,
	}
	if cw := contextWindowFromSpec(spec); cw > 0 {
		out = append(out, piContextWindowEnvVar+"="+strconv.Itoa(cw))
	}
	return out
}

// contextWindowFromSpec reads the resolved context-window size (tokens) from
// Spec.ProviderConfig["contextWindow"]. JSON decoding yields float64 for
// numbers, so int/int64/float64 are all accepted (the gemini harness's
// intFromProviderConfig idiom). Missing or non-positive returns 0: the caller
// omits the pin and the extension falls back to its default.
func contextWindowFromSpec(spec agent.Spec) int {
	switch v := spec.ProviderConfig["contextWindow"].(type) {
	case int:
		return v
	case int64:
		return int(v)
	case float64:
		return int(v)
	default:
		return 0
	}
}

// piAPIForProtocol maps a donmai WireProtocol to pi's pi-ai api name.
func piAPIForProtocol(p agent.WireProtocol) string {
	switch p {
	case agent.ProtoAnthropicMessages:
		return "anthropic-messages"
	case agent.ProtoOpenAIChat:
		return "openai-completions"
	case agent.ProtoOpenAIResponses:
		return "openai-responses"
	case agent.ProtoGeminiGenerate:
		return "google-generative-ai"
	default:
		return "openai-completions"
	}
}
