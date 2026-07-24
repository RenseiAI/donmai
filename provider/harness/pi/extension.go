package pi

import (
	"crypto/sha256"
	"crypto/subtle"
	"embed"
	"encoding/hex"
	"encoding/json"
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
	// §5.3 env hygiene).
	piStateDir = ".pi"

	// handshakeType / adjudicateType mirror the extension's request types.
	handshakeType  = "donmai.handshake"
	adjudicateType = "donmai.adjudicate"
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
// This is the value the handshake must match.
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

// sessionLayout describes where a session's pi state lives inside the
// worktree. All paths are under <cwd>/.pi so the runner's worktree lifecycle
// owns cleanup.
type sessionLayout struct {
	root       string // <cwd>/.pi
	extension  string // <cwd>/.pi/extensions/donmai-policy.ts
	settings   string // <cwd>/.pi/settings.json
	modelsJSON string // <cwd>/.pi/models.json
}

func newSessionLayout(cwd string) sessionLayout {
	root := filepath.Join(cwd, piStateDir)
	return sessionLayout{
		root:       root,
		extension:  filepath.Join(root, "extensions", extensionFileName),
		settings:   filepath.Join(root, "settings.json"),
		modelsJSON: filepath.Join(root, "models.json"),
	}
}

// materializeExtension writes the embedded policy extension, a provider-pin
// models.json (design §6), and a settings.json that disables everything not
// whitelisted, into the session worktree. Returns the layout so the caller can
// point pi at it and later verify the handshake.
//
// Fail-closed: any write error is returned; the caller must NOT spawn a
// prompt when materialization fails.
func materializeExtension(cwd string, ep *agent.EndpointBinding, model string) (sessionLayout, error) {
	layout := newSessionLayout(cwd)
	if err := os.MkdirAll(filepath.Dir(layout.extension), 0o700); err != nil {
		return layout, fmt.Errorf("pi: create extension dir: %w", err)
	}
	if err := os.WriteFile(layout.extension, extensionSource(), 0o600); err != nil {
		return layout, fmt.Errorf("pi: write policy extension: %w", err)
	}
	if err := os.WriteFile(layout.settings, sessionSettings(), 0o600); err != nil {
		return layout, fmt.Errorf("pi: write settings: %w", err)
	}
	if err := os.WriteFile(layout.modelsJSON, providerPinConfig(ep, model), 0o600); err != nil {
		return layout, fmt.Errorf("pi: write provider pin: %w", err)
	}
	return layout, nil
}

// sessionSettings is the settings payload that locks the session down: the
// donmai policy extension is the only extension loaded, model cycling is
// disabled (the session cannot wander off the resolved cell — design §6), and
// pi auth state is redirected into the session state dir so a fleet box's
// personal ~/.pi credentials are never visible (§5.3).
func sessionSettings() []byte {
	b, _ := json.MarshalIndent(map[string]any{
		"extensions": []map[string]any{
			{"path": "extensions/" + extensionFileName, "trusted": true},
		},
		// Deny model cycling so the session stays pinned to the resolved cell.
		"permissions": map[string]any{
			"cycle_model": "deny",
		},
		// Redirect auth/state into the session dir (env hygiene, §5.3).
		"stateDir":         ".",
		"disableTelemetry": true,
	}, "", "  ")
	return b
}

// providerPinConfig materializes the routing pin (design §6):
// register a "donmai" provider whose baseUrl is the resolved cell endpoint
// (the local gateway binding under a gateway cell) and whose api matches the
// cell protocol; the API KEY IS NEVER INLINED — it rides an env var reference
// so the key never lands on disk (mirrors opencode config.go's {env:...}
// indirection). set_model donmai/<model> is issued as a runtime command in
// handle.start.
func providerPinConfig(ep *agent.EndpointBinding, model string) []byte {
	baseURL := ""
	api := "openai-completions"
	if ep != nil {
		baseURL = ep.BaseURL
		api = piAPIForProtocol(ep.Protocol)
		if ep.Model != "" {
			model = ep.Model
		}
	}
	b, _ := json.MarshalIndent(map[string]any{
		"providers": map[string]any{
			"donmai": map[string]any{
				"baseUrl":   baseURL,
				"api":       api,
				"apiKeyEnv": PiKeyEnvVar, // reference, not the secret
			},
		},
		"model": "donmai/" + model,
	}, "", "  ")
	return b
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
