// Package afclient kit_client.go — DaemonClient methods for the
// /api/daemon/kits* and /api/daemon/kit-sources* surfaces.
// Wave-9 A2 ships the canonical HTTP implementation against the route
// contract locked in ADR-2026-05-07-daemon-http-control-api.md § D1.
package afclient

import "fmt"

// ListKits fetches all installed kits from GET /api/daemon/kits.
func (c *DaemonClient) ListKits() (*ListKitsResponse, error) {
	var resp ListKitsResponse
	if err := c.get("/api/daemon/kits", &resp); err != nil {
		return nil, fmt.Errorf("ListKits: %w", err)
	}
	return &resp, nil
}

// GetKit fetches the full manifest for a single kit from
// GET /api/daemon/kits/<id>. Returns ErrNotFound when the id is not
// registered.
func (c *DaemonClient) GetKit(id string) (*KitManifestEnvelope, error) {
	var resp KitManifestEnvelope
	if err := c.get("/api/daemon/kits/"+id, &resp); err != nil {
		return nil, fmt.Errorf("GetKit %q: %w", id, err)
	}
	return &resp, nil
}

// VerifyKitSignature triggers signature verification for the named kit
// via GET /api/daemon/kits/<id>/verify-signature.
func (c *DaemonClient) VerifyKitSignature(id string) (*KitSignatureResult, error) {
	var resp KitSignatureResult
	if err := c.get("/api/daemon/kits/"+id+"/verify-signature", &resp); err != nil {
		return nil, fmt.Errorf("VerifyKitSignature %q: %w", id, err)
	}
	return &resp, nil
}

// InstallKit installs the named kit via
// POST /api/daemon/kits/<id>/install.
func (c *DaemonClient) InstallKit(id string, req KitInstallRequest) (*KitInstallResult, error) {
	var resp KitInstallResult
	if err := c.post("/api/daemon/kits/"+id+"/install", req, &resp); err != nil {
		return nil, fmt.Errorf("InstallKit %q: %w", id, err)
	}
	return &resp, nil
}

// EnableKit activates a previously-disabled kit via
// POST /api/daemon/kits/<id>/enable.
func (c *DaemonClient) EnableKit(id string) (*Kit, error) {
	var resp Kit
	if err := c.post("/api/daemon/kits/"+id+"/enable", nil, &resp); err != nil {
		return nil, fmt.Errorf("EnableKit %q: %w", id, err)
	}
	return &resp, nil
}

// DisableKit deactivates an active kit via
// POST /api/daemon/kits/<id>/disable.
func (c *DaemonClient) DisableKit(id string) (*Kit, error) {
	var resp Kit
	if err := c.post("/api/daemon/kits/"+id+"/disable", nil, &resp); err != nil {
		return nil, fmt.Errorf("DisableKit %q: %w", id, err)
	}
	return &resp, nil
}

// ListKitSources fetches the registry-source federation order from
// GET /api/daemon/kit-sources.
func (c *DaemonClient) ListKitSources() (*ListKitSourcesResponse, error) {
	var resp ListKitSourcesResponse
	if err := c.get("/api/daemon/kit-sources", &resp); err != nil {
		return nil, fmt.Errorf("ListKitSources: %w", err)
	}
	return &resp, nil
}

// EnableKitSource enables the named registry source via
// POST /api/daemon/kit-sources/<name>/enable.
func (c *DaemonClient) EnableKitSource(name string) (*KitSourceToggleResult, error) {
	var resp KitSourceToggleResult
	if err := c.post("/api/daemon/kit-sources/"+name+"/enable", nil, &resp); err != nil {
		return nil, fmt.Errorf("EnableKitSource %q: %w", name, err)
	}
	return &resp, nil
}

// DisableKitSource disables the named registry source via
// POST /api/daemon/kit-sources/<name>/disable.
func (c *DaemonClient) DisableKitSource(name string) (*KitSourceToggleResult, error) {
	var resp KitSourceToggleResult
	if err := c.post("/api/daemon/kit-sources/"+name+"/disable", nil, &resp); err != nil {
		return nil, fmt.Errorf("DisableKitSource %q: %w", name, err)
	}
	return &resp, nil
}
