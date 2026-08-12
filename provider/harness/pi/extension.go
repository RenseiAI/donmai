package pi

import (
	"crypto/sha256"
	"crypto/subtle"
	"embed"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"

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
	// PiKeyEnvVar; these carry the non-secret routing pin.
	piBaseURLEnvVar   = "DONMAI_PI_BASE_URL"
	piAPIEnvVar       = "DONMAI_PI_API"
	piModelEnvVar     = "DONMAI_PI_MODEL"
	piHandshakeEnvVar = "DONMAI_PI_HANDSHAKE"

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
	if err := os.WriteFile(layout.extension, extensionSource(), 0o600); err != nil {
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
			if err := os.WriteFile(loadPath, d.Source, 0o600); err != nil {
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

// providerPinEnv builds the non-secret routing-pin env the child extension
// reads to register the single "donmai" provider (design §6). The API key is
// NOT here — it rides PiKeyEnvVar (applyEndpoint mirrors the resolved cell key
// onto it). Under a gateway cell the baseURL is the local gateway binding, so
// pi inherits the whole mesh through one pin.
func providerPinEnv(ep *agent.EndpointBinding, model string) []string {
	baseURL := ""
	api := "openai-completions"
	if ep != nil {
		baseURL = ep.BaseURL
		api = piAPIForProtocol(ep.Protocol)
		if ep.Model != "" {
			model = ep.Model
		}
	}
	return []string{
		piBaseURLEnvVar + "=" + baseURL,
		piAPIEnvVar + "=" + api,
		piModelEnvVar + "=" + model,
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
