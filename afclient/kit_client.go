// Package afclient kit_client.go — DaemonClient methods for the
// /api/daemon/kits* and /api/daemon/kit-sources* surfaces. Wave-9 Phase 2
// ships canonical method signatures stubbed with ErrUnimplemented; Track
// A2 fills in the HTTP implementation. Sub-agents must replace
// ErrUnimplemented with the real c.get/c.post call to the route
// specified in each method's doc comment.
package afclient

import "fmt"

// ListKits fetches all installed kits from GET /api/daemon/kits.
func (c *DaemonClient) ListKits() (*ListKitsResponse, error) {
	return nil, fmt.Errorf("ListKits: %w", ErrUnimplemented)
}

// GetKit fetches the full manifest for a single kit from
// GET /api/daemon/kits/<id>. Returns ErrNotFound when the id is not
// registered.
func (c *DaemonClient) GetKit(_ string) (*KitManifestEnvelope, error) {
	return nil, fmt.Errorf("GetKit: %w", ErrUnimplemented)
}

// VerifyKitSignature triggers signature verification for the named kit
// via GET /api/daemon/kits/<id>/verify-signature.
func (c *DaemonClient) VerifyKitSignature(_ string) (*KitSignatureResult, error) {
	return nil, fmt.Errorf("VerifyKitSignature: %w", ErrUnimplemented)
}

// InstallKit installs the named kit via
// POST /api/daemon/kits/<id>/install.
func (c *DaemonClient) InstallKit(_ string, _ KitInstallRequest) (*KitInstallResult, error) {
	return nil, fmt.Errorf("InstallKit: %w", ErrUnimplemented)
}

// EnableKit activates a previously-disabled kit via
// POST /api/daemon/kits/<id>/enable.
func (c *DaemonClient) EnableKit(_ string) (*Kit, error) {
	return nil, fmt.Errorf("EnableKit: %w", ErrUnimplemented)
}

// DisableKit deactivates an active kit via
// POST /api/daemon/kits/<id>/disable.
func (c *DaemonClient) DisableKit(_ string) (*Kit, error) {
	return nil, fmt.Errorf("DisableKit: %w", ErrUnimplemented)
}

// ListKitSources fetches the registry-source federation order from
// GET /api/daemon/kit-sources.
func (c *DaemonClient) ListKitSources() (*ListKitSourcesResponse, error) {
	return nil, fmt.Errorf("ListKitSources: %w", ErrUnimplemented)
}

// EnableKitSource enables the named registry source via
// POST /api/daemon/kit-sources/<name>/enable.
func (c *DaemonClient) EnableKitSource(_ string) (*KitSourceToggleResult, error) {
	return nil, fmt.Errorf("EnableKitSource: %w", ErrUnimplemented)
}

// DisableKitSource disables the named registry source via
// POST /api/daemon/kit-sources/<name>/disable.
func (c *DaemonClient) DisableKitSource(_ string) (*KitSourceToggleResult, error) {
	return nil, fmt.Errorf("DisableKitSource: %w", ErrUnimplemented)
}
