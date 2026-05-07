// Package daemon kit_registry.go — minimal in-process Kit registry that
// scans the filesystem for installed kit manifests and exposes them via
// the operator control API.
//
// This is the OSS-execution-layer's "Local manifests" registry source
// from the federation list in 005-kit-manifest-spec.md § "Registry
// sources" (item 1). Other registry sources (bundled, rensei, tessl,
// agentskills, community) are not implemented in this wave; the
// /api/daemon/kit-sources endpoint returns a static descriptor list
// surfacing the federation order.
//
// Scan path defaults to ~/.rensei/kits/*.kit.toml. Multiple paths may be
// declared via daemon.yaml's optional `kit.scanPaths` override.
//
// Behaviour:
//   - Empty registry (no scan path entries, no .kit.toml files) → empty
//     list, HTTP 200.
//   - Malformed manifests log a warning via slog and are excluded from the
//     listing rather than failing the whole request.
//   - Enable/disable state is persisted to a sidecar file at
//     ~/.rensei/kits/.state.json so toggle outcomes survive daemon
//     restarts. The file is created on first toggle.
//   - Install is currently a stub returning ErrKitInstallUnimplemented;
//     fetching kits from a remote registry is deferred until the
//     federation sources land.
//   - Verify-signature returns KitTrustUnsigned for all kits in this
//     wave (signing is partially implemented per the ADR caveat).
package daemon

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"github.com/BurntSushi/toml"

	"github.com/RenseiAI/agentfactory-tui/afclient"
)

// ErrKitInstallUnimplemented is returned by KitRegistry.Install while the
// remote-registry fetch path is unimplemented. See the wave-9 deferral note
// in the package doc.
var ErrKitInstallUnimplemented = errors.New("kit install: remote registry fetch not implemented in this wave")

// ErrKitNotFound is returned when a kit id is not present in the registry.
var ErrKitNotFound = errors.New("kit not found")

// ErrKitSourceNotFound is returned when a kit-source name is not known.
var ErrKitSourceNotFound = errors.New("kit source not found")

// DefaultKitScanPath returns the canonical scan path for installed kits
// (~/.rensei/kits). Used when daemon.yaml does not declare kit.scanPaths.
func DefaultKitScanPath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return "/tmp/.rensei/kits"
	}
	return filepath.Join(home, ".rensei", "kits")
}

// kitStatePath returns the path to the sidecar state file used to persist
// enable/disable toggles across daemon restarts. The file lives next to
// the first scanPath since toggles are scan-path-agnostic.
func kitStatePath(firstScanPath string) string {
	if firstScanPath == "" {
		firstScanPath = DefaultKitScanPath()
	}
	return filepath.Join(firstScanPath, ".state.json")
}

// kitState is the persisted shape for the .state.json sidecar.
type kitState struct {
	// DisabledIDs tracks kits the operator has explicitly disabled.
	// Kits not present are considered active.
	DisabledIDs []string `json:"disabledIds,omitempty"`
	// DisabledSources tracks registry sources the operator has disabled.
	DisabledSources []string `json:"disabledSources,omitempty"`
}

// kitManifestTOML is the on-disk TOML shape used to decode kit manifests.
// It mirrors the schema in 005-kit-manifest-spec.md but is intentionally
// permissive: unknown fields are ignored so future schema additions don't
// break parsing.
type kitManifestTOML struct {
	API string `toml:"api"`

	Kit struct {
		ID             string `toml:"id"`
		Version        string `toml:"version"`
		Name           string `toml:"name"`
		Description    string `toml:"description"`
		Author         string `toml:"author"`
		AuthorIdentity string `toml:"authorIdentity"`
		License        string `toml:"license"`
		Homepage       string `toml:"homepage"`
		Repository     string `toml:"repository"`
		Priority       int    `toml:"priority"`
	} `toml:"kit"`

	Supports struct {
		OS   []string `toml:"os"`
		Arch []string `toml:"arch"`
	} `toml:"supports"`

	Requires struct {
		Rensei       string   `toml:"rensei"`
		Capabilities []string `toml:"capabilities"`
	} `toml:"requires"`

	Detect struct {
		Files     []string          `toml:"files"`
		FilesAll  []string          `toml:"files_all"`
		Exec      string            `toml:"exec"`
		Toolchain map[string]string `toml:"toolchain"`
	} `toml:"detect"`

	Provide struct {
		Commands        map[string]string `toml:"commands"`
		ToolPermissions []struct {
			Shell string `toml:"shell"`
		} `toml:"tool_permissions"`
		PromptFragments []struct {
			Partial string   `toml:"partial"`
			When    []string `toml:"when"`
			File    string   `toml:"file"`
		} `toml:"prompt_fragments"`
		MCPServers []struct {
			Name        string `toml:"name"`
			Command     string `toml:"command"`
			Description string `toml:"description"`
		} `toml:"mcp_servers"`
		Skills []struct {
			File string `toml:"file"`
		} `toml:"skills"`
		Agents []struct {
			ID       string `toml:"id"`
			Template string `toml:"template"`
		} `toml:"agents"`
		A2ASkills []struct {
			ID          string `toml:"id"`
			Description string `toml:"description"`
		} `toml:"a2a_skills"`
		IntelligenceExtractors []struct {
			Name     string `toml:"name"`
			Language string `toml:"language"`
		} `toml:"intelligence_extractors"`
	} `toml:"provide"`

	Composition struct {
		ConflictsWith []string `toml:"conflicts_with"`
		ComposesWith  []string `toml:"composes_with"`
		Order         string   `toml:"order"`
	} `toml:"composition"`
}

// KitRegistry is a minimal in-process Kit registry.
//
// Methods are safe for concurrent use. The registry rescans on every List
// call so newly-installed manifests appear without a daemon restart; this
// is acceptable for an operator-facing surface where call volume is low.
type KitRegistry struct {
	scanPaths []string
	mu        sync.Mutex
}

// NewKitRegistry constructs a KitRegistry.
//
// scanPaths defaults to []string{DefaultKitScanPath()} when nil or empty.
// The first scan path is also where the .state.json sidecar lives.
func NewKitRegistry(scanPaths []string) *KitRegistry {
	if len(scanPaths) == 0 {
		scanPaths = []string{DefaultKitScanPath()}
	}
	expanded := make([]string, 0, len(scanPaths))
	for _, p := range scanPaths {
		expanded = append(expanded, expandKitHomePath(p))
	}
	return &KitRegistry{scanPaths: expanded}
}

// expandKitHomePath replaces a leading ~ with the user's home directory.
// Kept local to avoid coupling to afcli helpers.
func expandKitHomePath(path string) string {
	if !strings.HasPrefix(path, "~/") && path != "~" {
		return path
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return path
	}
	if path == "~" {
		return home
	}
	return filepath.Join(home, path[2:])
}

// ScanPaths returns the registry's scan paths in declaration order.
func (r *KitRegistry) ScanPaths() []string {
	out := make([]string, len(r.scanPaths))
	copy(out, r.scanPaths)
	return out
}

// List returns all installed kits across all scan paths. Malformed
// manifests log a warning and are excluded. Empty scan paths return an
// empty slice with no error.
func (r *KitRegistry) List() []afclient.Kit {
	manifests := r.scan()
	state := r.loadState()
	disabled := make(map[string]struct{}, len(state.DisabledIDs))
	for _, id := range state.DisabledIDs {
		disabled[id] = struct{}{}
	}
	out := make([]afclient.Kit, 0, len(manifests))
	for _, m := range manifests {
		k := manifestToKit(m)
		if _, ok := disabled[k.ID]; ok {
			k.Status = afclient.KitStatusDisabled
		} else {
			k.Status = afclient.KitStatusActive
		}
		out = append(out, k)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// Get returns the full manifest for a single kit id. Returns ErrKitNotFound
// when the id is not registered.
func (r *KitRegistry) Get(id string) (afclient.KitManifest, error) {
	manifests := r.scan()
	state := r.loadState()
	disabled := make(map[string]struct{}, len(state.DisabledIDs))
	for _, did := range state.DisabledIDs {
		disabled[did] = struct{}{}
	}
	for _, m := range manifests {
		if m.Kit.ID != id {
			continue
		}
		k := manifestToKit(m)
		if _, ok := disabled[k.ID]; ok {
			k.Status = afclient.KitStatusDisabled
		} else {
			k.Status = afclient.KitStatusActive
		}
		return manifestToKitManifest(m, k), nil
	}
	return afclient.KitManifest{}, fmt.Errorf("%s: %w", id, ErrKitNotFound)
}

// Enable marks the kit active in the persisted state. Returns the updated
// Kit summary or ErrKitNotFound when the id is unknown.
func (r *KitRegistry) Enable(id string) (afclient.Kit, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	manifests := r.scan()
	var match *kitManifestTOML
	for i := range manifests {
		if manifests[i].Kit.ID == id {
			match = &manifests[i]
			break
		}
	}
	if match == nil {
		return afclient.Kit{}, fmt.Errorf("%s: %w", id, ErrKitNotFound)
	}
	state := r.loadStateLocked()
	state.DisabledIDs = removeString(state.DisabledIDs, id)
	if err := r.saveStateLocked(state); err != nil {
		return afclient.Kit{}, fmt.Errorf("save kit state: %w", err)
	}
	k := manifestToKit(*match)
	k.Status = afclient.KitStatusActive
	return k, nil
}

// Disable marks the kit disabled in the persisted state. Returns the
// updated Kit summary or ErrKitNotFound when the id is unknown.
func (r *KitRegistry) Disable(id string) (afclient.Kit, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	manifests := r.scan()
	var match *kitManifestTOML
	for i := range manifests {
		if manifests[i].Kit.ID == id {
			match = &manifests[i]
			break
		}
	}
	if match == nil {
		return afclient.Kit{}, fmt.Errorf("%s: %w", id, ErrKitNotFound)
	}
	state := r.loadStateLocked()
	if !containsString(state.DisabledIDs, id) {
		state.DisabledIDs = append(state.DisabledIDs, id)
	}
	if err := r.saveStateLocked(state); err != nil {
		return afclient.Kit{}, fmt.Errorf("save kit state: %w", err)
	}
	k := manifestToKit(*match)
	k.Status = afclient.KitStatusDisabled
	return k, nil
}

// VerifySignature returns a KitSignatureResult for the kit. In this wave
// the signing model is partially implemented: signed-verified is never
// returned even for manifests carrying authorIdentity. See
// 005-kit-manifest-spec.md § "Trust verification".
func (r *KitRegistry) VerifySignature(id string) (afclient.KitSignatureResult, error) {
	manifests := r.scan()
	for _, m := range manifests {
		if m.Kit.ID != id {
			continue
		}
		return afclient.KitSignatureResult{
			KitID:    id,
			Trust:    afclient.KitTrustUnsigned,
			SignerID: m.Kit.AuthorIdentity,
			OK:       true,
			Details:  "Signature verification is partially implemented (Wave 9 caveat); manifest reported as unsigned. See 005-kit-manifest-spec.md § Trust verification.",
		}, nil
	}
	return afclient.KitSignatureResult{}, fmt.Errorf("%s: %w", id, ErrKitNotFound)
}

// Install is currently a no-op stub. The remote-registry fetch path is
// scheduled for a follow-up wave; this method exists so the HTTP route
// has a wired call site.
func (r *KitRegistry) Install(_ string, _ afclient.KitInstallRequest) (afclient.KitInstallResult, error) {
	return afclient.KitInstallResult{}, ErrKitInstallUnimplemented
}

// ListSources returns the federation order's registry source descriptors.
// Persisted disable state from .state.json is applied to the Enabled flag.
func (r *KitRegistry) ListSources() []afclient.KitRegistrySource {
	state := r.loadState()
	disabled := make(map[string]struct{}, len(state.DisabledSources))
	for _, n := range state.DisabledSources {
		disabled[n] = struct{}{}
	}
	sources := defaultKitSources()
	for i := range sources {
		_, off := disabled[sources[i].Name]
		sources[i].Enabled = !off
	}
	return sources
}

// EnableSource toggles a registry source on. Returns ErrKitSourceNotFound
// if the name is not in the federation list.
func (r *KitRegistry) EnableSource(name string) (afclient.KitRegistrySource, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !isKnownKitSource(name) {
		return afclient.KitRegistrySource{}, fmt.Errorf("%s: %w", name, ErrKitSourceNotFound)
	}
	state := r.loadStateLocked()
	state.DisabledSources = removeString(state.DisabledSources, name)
	if err := r.saveStateLocked(state); err != nil {
		return afclient.KitRegistrySource{}, fmt.Errorf("save kit state: %w", err)
	}
	src := lookupKitSource(name)
	src.Enabled = true
	return src, nil
}

// DisableSource toggles a registry source off.
func (r *KitRegistry) DisableSource(name string) (afclient.KitRegistrySource, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !isKnownKitSource(name) {
		return afclient.KitRegistrySource{}, fmt.Errorf("%s: %w", name, ErrKitSourceNotFound)
	}
	state := r.loadStateLocked()
	if !containsString(state.DisabledSources, name) {
		state.DisabledSources = append(state.DisabledSources, name)
	}
	if err := r.saveStateLocked(state); err != nil {
		return afclient.KitRegistrySource{}, fmt.Errorf("save kit state: %w", err)
	}
	src := lookupKitSource(name)
	src.Enabled = false
	return src, nil
}

// scan walks each scan path and returns the parsed manifests.
// Malformed manifests are skipped with a warning.
//
// Per 005-kit-manifest-spec.md § "Daemon kit registry": "Multiple paths
// are scanned in declaration order; later paths override earlier ones on
// kit.id collision."
func (r *KitRegistry) scan() []kitManifestTOML {
	var (
		seen = map[string]int{}
		out  []kitManifestTOML
	)
	for _, dir := range r.scanPaths {
		entries, err := os.ReadDir(dir)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			slog.Warn("kit registry: read scan path", //nolint:gosec // structured slog handler escapes values
				"path", dir,
				"err", err.Error(),
			)
			continue
		}
		for _, e := range entries {
			if e.IsDir() {
				continue
			}
			name := e.Name()
			if !strings.HasSuffix(name, ".kit.toml") {
				continue
			}
			full := filepath.Join(dir, name)
			m, err := loadKitManifestFile(full)
			if err != nil {
				slog.Warn("kit registry: malformed manifest", //nolint:gosec // structured slog handler escapes values
					"path", full,
					"err", err.Error(),
				)
				continue
			}
			if m.Kit.ID == "" {
				slog.Warn("kit registry: manifest missing kit.id", //nolint:gosec // structured slog handler escapes values
					"path", full,
				)
				continue
			}
			if idx, ok := seen[m.Kit.ID]; ok {
				out[idx] = m
				continue
			}
			seen[m.Kit.ID] = len(out)
			out = append(out, m)
		}
	}
	return out
}

// loadKitManifestFile decodes a single .kit.toml file.
func loadKitManifestFile(path string) (kitManifestTOML, error) {
	var m kitManifestTOML
	data, err := os.ReadFile(path) //nolint:gosec // operator-installed manifests
	if err != nil {
		return m, fmt.Errorf("read manifest: %w", err)
	}
	if err := toml.Unmarshal(data, &m); err != nil {
		return m, fmt.Errorf("parse manifest: %w", err)
	}
	return m, nil
}

// loadState reads the persisted .state.json sidecar.
// Missing file returns an empty zero-value state without error.
func (r *KitRegistry) loadState() kitState {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.loadStateLocked()
}

// loadStateLocked is the unsynchronised variant used internally when the
// caller already holds r.mu.
func (r *KitRegistry) loadStateLocked() kitState {
	if len(r.scanPaths) == 0 {
		return kitState{}
	}
	path := kitStatePath(r.scanPaths[0])
	data, err := os.ReadFile(path) //nolint:gosec // operator-controlled path
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			slog.Warn("kit registry: read state", "path", path, "err", err.Error())
		}
		return kitState{}
	}
	var st kitState
	if err := json.Unmarshal(data, &st); err != nil {
		slog.Warn("kit registry: parse state", "path", path, "err", err.Error())
		return kitState{}
	}
	return st
}

// saveStateLocked persists state to .state.json. Caller must hold r.mu.
func (r *KitRegistry) saveStateLocked(st kitState) error {
	if len(r.scanPaths) == 0 {
		return errors.New("no scan paths configured")
	}
	dir := r.scanPaths[0]
	if err := os.MkdirAll(dir, 0o700); err != nil { //nolint:gosec // operator-controlled scan path
		return fmt.Errorf("create state dir %q: %w", dir, err)
	}
	path := kitStatePath(dir)
	data, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal state: %w", err)
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil { //nolint:gosec // operator-controlled scan path
		return fmt.Errorf("write temp state: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("rename state: %w", err)
	}
	return nil
}

// manifestToKit converts a parsed TOML manifest to the wire Kit summary.
func manifestToKit(m kitManifestTOML) afclient.Kit {
	return afclient.Kit{
		ID:                 m.Kit.ID,
		Name:               m.Kit.Name,
		Version:            m.Kit.Version,
		Description:        m.Kit.Description,
		Author:             m.Kit.Author,
		AuthorID:           m.Kit.AuthorIdentity,
		License:            m.Kit.License,
		Homepage:           m.Kit.Homepage,
		Repository:         m.Kit.Repository,
		Priority:           m.Kit.Priority,
		Source:             afclient.KitSourceLocal,
		Scope:              afclient.KitScopeProject,
		Trust:              afclient.KitTrustUnsigned,
		DetectFiles:        copyStrings(m.Detect.Files),
		DetectExec:         m.Detect.Exec,
		ProvidesCommands:   len(m.Provide.Commands) > 0,
		ProvidesPrompts:    len(m.Provide.PromptFragments) > 0,
		ProvidesTools:      len(m.Provide.ToolPermissions) > 0,
		ProvidesMCPServers: len(m.Provide.MCPServers) > 0,
		ProvidesSkills:     len(m.Provide.Skills) > 0,
		ProvidesAgents:     len(m.Provide.Agents) > 0,
		ProvidesA2ASkills:  len(m.Provide.A2ASkills) > 0,
		ProvidesExtractors: len(m.Provide.IntelligenceExtractors) > 0,
	}
}

// manifestToKitManifest builds the full envelope view used by GET .../<id>.
func manifestToKitManifest(m kitManifestTOML, k afclient.Kit) afclient.KitManifest {
	out := afclient.KitManifest{
		Kit:                  k,
		SupportedOS:          copyStrings(m.Supports.OS),
		SupportedArch:        copyStrings(m.Supports.Arch),
		RequiresRensei:       m.Requires.Rensei,
		RequiresCapabilities: copyStrings(m.Requires.Capabilities),
		ConflictsWith:        copyStrings(m.Composition.ConflictsWith),
		ComposesWith:         copyStrings(m.Composition.ComposesWith),
		Order:                m.Composition.Order,
		DetectToolchain:      copyStringMap(m.Detect.Toolchain),
		Commands:             copyStringMap(m.Provide.Commands),
	}
	for _, s := range m.Provide.MCPServers {
		out.MCPServerNames = append(out.MCPServerNames, s.Name)
	}
	for _, s := range m.Provide.Skills {
		out.SkillFiles = append(out.SkillFiles, s.File)
	}
	for _, a := range m.Provide.Agents {
		out.AgentIDs = append(out.AgentIDs, a.ID)
	}
	for _, a := range m.Provide.A2ASkills {
		out.A2ASkillIDs = append(out.A2ASkillIDs, a.ID)
	}
	for _, x := range m.Provide.IntelligenceExtractors {
		out.ExtractorNames = append(out.ExtractorNames, x.Name)
	}
	return out
}

// defaultKitSources returns the federation-order registry sources from
// 005-kit-manifest-spec.md § "Registry sources". Only the local source
// has a working implementation in this wave — the remaining four are
// surfaced so operators can see the federation order, but Install
// against them returns ErrKitInstallUnimplemented.
func defaultKitSources() []afclient.KitRegistrySource {
	return []afclient.KitRegistrySource{
		{Name: "local", Kind: "local", URL: DefaultKitScanPath(), Enabled: true, Priority: 1},
		{Name: "bundled", Kind: "bundled", URL: "", Enabled: true, Priority: 2},
		{Name: "rensei", Kind: "rensei", URL: "https://registry.rensei.dev", Enabled: true, Priority: 3},
		{Name: "tessl", Kind: "tessl", URL: "https://registry.tessl.io", Enabled: true, Priority: 4},
		{Name: "agentskills", Kind: "agentskills", URL: "https://agentskills.io", Enabled: true, Priority: 5},
		{Name: "community", Kind: "community", URL: "", Enabled: true, Priority: 6},
	}
}

func isKnownKitSource(name string) bool {
	for _, s := range defaultKitSources() {
		if s.Name == name {
			return true
		}
	}
	return false
}

func lookupKitSource(name string) afclient.KitRegistrySource {
	for _, s := range defaultKitSources() {
		if s.Name == name {
			return s
		}
	}
	return afclient.KitRegistrySource{}
}

func copyStrings(in []string) []string {
	if len(in) == 0 {
		return nil
	}
	out := make([]string, len(in))
	copy(out, in)
	return out
}

func copyStringMap(in map[string]string) map[string]string {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]string, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

func containsString(haystack []string, needle string) bool {
	for _, h := range haystack {
		if h == needle {
			return true
		}
	}
	return false
}

func removeString(in []string, target string) []string {
	out := in[:0]
	for _, s := range in {
		if s != target {
			out = append(out, s)
		}
	}
	return out
}
