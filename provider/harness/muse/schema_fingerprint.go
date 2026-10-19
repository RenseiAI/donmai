package muse

// schemaFingerprint pins the exact `muse schema` export the wire types in
// types.go were generated from. The fingerprint comes from the export's
// own manifest.json (msp.schema.json's stable surface); a mismatch
// between this pin and the host binary's reported schema.fingerprint at
// handshake time is surfaced in the handshake error, not silently
// ignored.
const (
	// schemaExportVersion is the envelope schema version (SchemaInfo.
	// Version) the generated types target. The handshake requires exactly
	// this version.
	schemaExportVersion = 1

	// schemaExportFingerprint is the manifest.json fingerprint of the
	// export the types were generated from.
	schemaExportFingerprint = "sha256:eae467ab60cb9a5d5af4de616830f065e59375c987fd59269c8cc33ed2a1137f"

	// schemaExportExperimental is the surface the export was generated
	// from. Referenced by the handshake check so the pin stays honest.
	schemaExportExperimental = false
)

// schemaSurfaceStable reports whether the pinned export is the stable
// surface (not experimental).
func schemaSurfaceStable() bool { return !schemaExportExperimental }
