package muse

// schemaFingerprint pins the exact `muse schema` export the wire types in
// types.go were generated from. The fingerprint comes from the export's
// own manifest.json (msp.schema.json's stable surface); a mismatch
// between this pin and the host binary's reported schema.fingerprint at
// handshake time is surfaced as an advisory warning; the envelope version is enforced.
const (
	// schemaExportVersion is the envelope schema version (SchemaInfo.
	// Version) the generated types target. The handshake requires exactly
	// this version.
	schemaExportVersion = 1

	// schemaExportFingerprint is the manifest.json fingerprint of the
	// export the types were generated from.
	schemaExportFingerprint = "sha256:c94c3d1f2f51e6424074b9ec43eac3eedcf532d3dcd77351ec1383a87d3049c6"

	// schemaExportExperimental is the surface the export was generated
	// from. Referenced by the handshake check so the pin stays honest.
	schemaExportExperimental = false
)

// schemaSurfaceStable reports whether the pinned export is the stable
// surface (not experimental).
func schemaSurfaceStable() bool { return !schemaExportExperimental }
