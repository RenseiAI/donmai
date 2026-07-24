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
}

func newSessionLayout(cwd string) sessionLayout {
	root := filepath.Join(cwd, piStateDir)
	return sessionLayout{
		root:      root,
		extension: filepath.Join(root, extensionFileName),
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
