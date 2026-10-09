// Package daemon — kit_dependency_stores.go: parse, gate, and project
// [[provide.dependency_store]] entries.
//
// The manifest api revision gate: v1 manifests must not declare the new
// section (a v1 consumer would half-apply it — read the manifest while
// ignoring the store authority metadata), and unknown revisions are
// rejected outright. Only the v2 revision may carry dependency stores.
package daemon

import (
	"fmt"

	"github.com/RenseiAI/donmai/internal/kit"
)

// validateManifestAPIRevision rejects manifests this binary cannot
// faithfully apply: unknown api revisions, and v1 manifests carrying the
// dependency-store section a v1 consumer would silently ignore.
func validateManifestAPIRevision(m kitManifestTOML) error {
	if !kitManifestSupportedAPI(m.API) {
		return fmt.Errorf("kit manifest: unsupported api %q", m.API)
	}
	if m.API != kitManifestAPIV2 && len(m.Provide.DependencyStores) > 0 {
		return fmt.Errorf("kit manifest: dependency_store requires api %q, manifest declares %q",
			kitManifestAPIV2, m.API)
	}
	return nil
}

// dependencyStoreViews projects parsed entries into the
// daemon-independent view type, validating each entry. A v2 manifest
// with an invalid entry is rejected; validation errors name the kit.
func dependencyStoreViews(m kitManifestTOML) ([]kit.DependencyStoreView, error) {
	if len(m.Provide.DependencyStores) == 0 {
		return nil, nil
	}
	views := make([]kit.DependencyStoreView, 0, len(m.Provide.DependencyStores))
	for i := range m.Provide.DependencyStores {
		views = append(views, projectDependencyStore(m.Provide.DependencyStores[i]))
	}
	if err := kit.ValidateDependencyStores(views); err != nil {
		return nil, fmt.Errorf("kit %s: %w", m.Kit.ID, err)
	}
	return views, nil
}

// projectDependencyStore maps one TOML entry onto the engine view type.
func projectDependencyStore(entry kitDependencyStoreTOML) kit.DependencyStoreView {
	return kit.DependencyStoreView{
		Manager:   entry.Manager,
		Ecosystem: entry.Ecosystem,
		Lockfiles: append([]string{}, entry.Lockfiles...),
		Inputs:    append([]string{}, entry.Inputs...),
		Version:   entry.Version,
		Store: kit.DependencyStoreLayout{
			Env:         append([]string{}, entry.Store.Env...),
			Default:     copyStringMap(entry.Store.Default),
			Sharing:     entry.Store.Sharing,
			Integrity:   entry.Store.Integrity,
			Content:     append([]string{}, entry.Store.Content...),
			Bookkeeping: append([]string{}, entry.Store.Bookkeeping...),
			Secrets:     append([]string{}, entry.Store.Secrets...),
			NeverShare:  append([]string{}, entry.Store.NeverShare...),
			RecordsPath: entry.Store.RecordsPath,
		},
		ImportEnv:        copyStringMap(entry.Import.Env),
		Commands:         copyStringMap(entry.Commands),
		CommandsOverride: copyStringMapMap(entry.CommandsOverride),
		Snapshot: kit.DependencyStoreSnapshot{
			Installed:       append([]string{}, entry.Snapshot.Installed...),
			ABI:             append([]string{}, entry.Snapshot.ABI...),
			Relocation:      entry.Snapshot.Relocation,
			RunsPackageCode: entry.Snapshot.RunsPackageCode,
		},
		ProxyEnv: copyStringMap(entry.Proxy.Env),
	}
}
