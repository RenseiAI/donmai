// Package afclient kit_types.go — wire types for the daemon's
// /api/daemon/kits* and /api/daemon/kit-sources* operator surfaces. The
// contract is locked in
// donmai-architecture/ADR-2026-05-07-daemon-http-control-api.md (D4) and
// follows the manifest schema in 005-kit-manifest-spec.md.
package afclient

// KitStatus is the activation status of an installed kit.
type KitStatus string

// Kit activation statuses.
const (
	KitStatusActive   KitStatus = "active"
	KitStatusDisabled KitStatus = "disabled"
	KitStatusError    KitStatus = "error"
)

// KitSource identifies where a kit originated, per the federation order
// defined in 005-kit-manifest-spec.md § "Registry sources".
type KitSource string

// Kit registry sources from the federation order in 005-kit-manifest-spec.md.
const (
	KitSourceLocal       KitSource = "local"       // .donmai/kits/*.kit.toml
	KitSourceBundled     KitSource = "bundled"     // shipped with OSS execution layer
	KitSourceTessl       KitSource = "tessl"       // registry.tessl.io
	KitSourceAgentSkills KitSource = "agentskills" // agentskills.io
	KitSourceCommunity   KitSource = "community"   // tenant-declared community/enterprise registries
)

// KitTrustState encodes the three trust states visible in the operator
// surface, mirroring ProviderTrustState for consistency.
type KitTrustState string

// Kit trust states (mirror of ProviderTrustState).
const (
	KitTrustSignedVerified   KitTrustState = "signed-verified"
	KitTrustSignedUnverified KitTrustState = "signed-unverified"
	KitTrustUnsigned         KitTrustState = "unsigned"
)

// KitScope is the activation scope level, mirroring ProviderScope.
type KitScope string

// Kit activation scope levels (mirror of ProviderScope).
const (
	KitScopeProject KitScope = "project"
	KitScopeOrg     KitScope = "org"
	KitScopeTenant  KitScope = "tenant"
	KitScopeGlobal  KitScope = "global"
)

// Kit is the API representation of an installed kit as returned by
// GET /api/daemon/kits and GET /api/daemon/kits/<id>. Maps to the
// manifest schema in 005-kit-manifest-spec.md.
type Kit struct {
	// Identity (from [kit] block in TOML manifest)
	ID          string `json:"id"`
	Name        string `json:"name"`
	Version     string `json:"version"`
	Description string `json:"description,omitempty"`
	Author      string `json:"author,omitempty"`
	AuthorID    string `json:"authorId,omitempty"` // did:web: identity
	License     string `json:"license,omitempty"`
	Homepage    string `json:"homepage,omitempty"`
	Repository  string `json:"repository,omitempty"`
	Priority    int    `json:"priority,omitempty"`

	// Runtime state
	Status KitStatus `json:"status"`
	Source KitSource `json:"source"`
	Scope  KitScope  `json:"scope"`

	// Trust / signature
	Trust    KitTrustState `json:"trust"`
	SignerID string        `json:"signerId,omitempty"`
	SignedAt string        `json:"signedAt,omitempty"`

	// Detect summary — shows what the kit detects without running detect.
	DetectFiles []string `json:"detectFiles,omitempty"`
	DetectExec  string   `json:"detectExec,omitempty"`

	// Contribution summary — concise view of what the kit provides.
	ProvidesCommands   bool `json:"providesCommands"`
	ProvidesPrompts    bool `json:"providesPrompts"`
	ProvidesTools      bool `json:"providesTools"`
	ProvidesMCPServers bool `json:"providesMcpServers"`
	ProvidesSkills     bool `json:"providesSkills"`
	ProvidesAgents     bool `json:"providesAgents"`
	ProvidesA2ASkills  bool `json:"providesA2aSkills"`
	ProvidesExtractors bool `json:"providesExtractors"`
}

// KitManifest is the full manifest detail for a kit, as returned by
// GET /api/daemon/kits/<id>. Richer view used by `kit show`.
type KitManifest struct {
	Kit

	// [supports]
	SupportedOS   []string `json:"supportedOs,omitempty"`
	SupportedArch []string `json:"supportedArch,omitempty"`

	// [requires]
	RequiresRensei       string   `json:"requiresRensei,omitempty"`
	RequiresCapabilities []string `json:"requiresCapabilities,omitempty"`

	// [composition]
	ConflictsWith []string `json:"conflictsWith,omitempty"`
	ComposesWith  []string `json:"composesWith,omitempty"`
	Order         string   `json:"order,omitempty"`

	// [detect] detail
	DetectToolchain map[string]string `json:"detectToolchain,omitempty"`

	// [provide.commands]
	Commands map[string]string `json:"commands,omitempty"`

	// [provide.commands_override.<os>] — OS-keyed command overlays.
	CommandsOverride map[string]map[string]string `json:"commandsOverride,omitempty"`

	// [provide.toolchain_install.<os>] — OS-keyed base-toolchain install
	// scripts (005:196-208). os → {key: shell command}. Surfaced so the
	// daemon HTTP detail (GET /api/daemon/kits/<id>) exposes the demand a
	// platform KitManifestSource can resolve into a ToolchainDemand.
	ToolchainInstall map[string]map[string]string `json:"toolchainInstall,omitempty"`

	// [provide.hooks] — post_acquire / pre_release lifecycle scripts
	// (005:216-223), with an optional OS-keyed overlay. Single command
	// string per hook.
	Hooks *KitHooks `json:"hooks,omitempty"`

	// Provide arrays — names/ids only for summary.
	MCPServerNames []string `json:"mcpServerNames,omitempty"`
	SkillFiles     []string `json:"skillFiles,omitempty"`
	AgentIDs       []string `json:"agentIds,omitempty"`
	A2ASkillIDs    []string `json:"a2aSkillIds,omitempty"`
	ExtractorNames []string `json:"extractorNames,omitempty"`
}

// KitHooks carries the [provide.hooks] lifecycle scripts from a kit
// manifest (005:216-223). PostAcquire/PreRelease are the generic
// single-command forms; OS holds the OS-keyed overlay
// ([provide.hooks.os.<os>]). Most-specific (OS-keyed) wins at compose
// time.
type KitHooks struct {
	PostAcquire string                  `json:"postAcquire,omitempty"`
	PreRelease  string                  `json:"preRelease,omitempty"`
	OS          map[string]KitHookEntry `json:"os,omitempty"`
}

// KitHookEntry is one OS-keyed hook overlay inside KitHooks.OS.
type KitHookEntry struct {
	PostAcquire string `json:"postAcquire,omitempty"`
	PreRelease  string `json:"preRelease,omitempty"`
}

// ListKitsResponse matches GET /api/daemon/kits.
type ListKitsResponse struct {
	Kits []Kit `json:"kits"`
}

// KitManifestEnvelope wraps the full manifest returned by
// GET /api/daemon/kits/<id>.
type KitManifestEnvelope struct {
	Kit KitManifest `json:"kit"`
}

// KitInstallSource identifies where the daemon should fetch a kit from
// at install time. The wire shape is anchored in
// ADR-2026-05-07-daemon-http-control-api.md § D6 (Wave 12 amendment).
//
// Wave 12 ships only `kind: "git"`; the remaining federation kinds
// (`tessl`, `agentskills`) return ErrKitSourceFederationUnimplemented
// in the registry. The descriptor list returned by
// /api/daemon/kit-sources continues to surface them so operators can
// see the federation order, but Install against them is unimplemented.
type KitInstallSource struct {
	// Kind selects the fetcher. Wave 12: "git" only.
	Kind string `json:"kind"`
	// URL is the fetcher-specific source URL (e.g., a git remote).
	URL string `json:"url"`
	// Ref is the optional git ref (branch/tag/commit). Default: HEAD.
	Ref string `json:"ref,omitempty"`
	// ManifestPath is the optional path inside the source to the kit
	// manifest. Default: registry walks the source root for *.kit.toml.
	ManifestPath string `json:"manifestPath,omitempty"`
}

// KitInstallRequest is the request body for POST /api/daemon/kits/<id>/install.
type KitInstallRequest struct {
	Version string            `json:"version,omitempty"`
	Source  *KitInstallSource `json:"source,omitempty"`
	// TrustOverride bypasses the configured trust gate for this single
	// install. The only accepted value is "allowed-this-once" (per
	// 002-provider-base-contract.md § "Signing and trust").
	// When set the daemon emits a structured slog audit log with the
	// kitId, signerId, actor, and timestamp. The override is single-shot:
	// not persisted; subsequent re-installs re-evaluate the gate. Empty
	// string = no override.
	TrustOverride string `json:"trustOverride,omitempty"`
}

// TrustOverrideAllowedThisOnce is the only accepted value of
// KitInstallRequest.TrustOverride. Mirrors the
// 'trustOverride: "allowed-this-once"' contract above.
const TrustOverrideAllowedThisOnce = "allowed-this-once"

// KitInstallResult is returned by POST /api/daemon/kits/<id>/install.
type KitInstallResult struct {
	Kit     Kit    `json:"kit"`
	Message string `json:"message,omitempty"`
}

// KitSignatureResult is returned by GET /api/daemon/kits/<id>/verify-signature.
type KitSignatureResult struct {
	KitID    string        `json:"kitId"`
	Trust    KitTrustState `json:"trust"`
	SignerID string        `json:"signerId,omitempty"`
	SignedAt string        `json:"signedAt,omitempty"`
	OK       bool          `json:"ok"`
	Details  string        `json:"details,omitempty"`
}

// KitRegistrySource is a kit registry source descriptor.
type KitRegistrySource struct {
	Name     string `json:"name"`
	URL      string `json:"url"`
	Enabled  bool   `json:"enabled"`
	Priority int    `json:"priority"` // federation order — lower = higher priority
	Kind     string `json:"kind"`     // "bundled" | "tessl" | "agentskills" | "community"
}

// ListKitSourcesResponse matches GET /api/daemon/kit-sources.
type ListKitSourcesResponse struct {
	Sources []KitRegistrySource `json:"sources"`
}

// KitSourceToggleResult is the response from
// POST /api/daemon/kit-sources/<name>/{enable,disable}.
type KitSourceToggleResult struct {
	Source  KitRegistrySource `json:"source"`
	Message string            `json:"message,omitempty"`
}
