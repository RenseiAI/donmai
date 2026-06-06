// Package matrix is the single source of truth + code generator for the
// two-axis capability matrix (harness × model-endpoint × serving-host).
//
// The hand-authored inputs live in cells.go (valid-cell list + denylist) and
// harvest.go (which providers/endpoints to harvest Manifest() from). build.go
// harvests the live manifests, validates every hand-authored cell against them
// (protocol intersection, authMode subset, caps narrowing) and assembles the
// deterministic matrix; render.go marshals the byte-stable artifacts.
//
// The generator (matrix/gen) writes the committed artifacts:
//
//	capability-matrix.json   — the full document
//	harnesses.json           — the harness rows
//	endpoints.json           — the model-endpoint rows
//	matrix.json              — cells + denylist + legacy aliases
//	registry_gen.go          — the legacy ProviderName → cell alias map (Go)
//
// These are committed artifacts gated by the CI parity test (parity_test.go),
// which regenerates into buffers and byte-compares against the committed files.
// platform and rensei-tui read the JSON; keep it at the package root (NOT under
// testdata/, which go build excludes).
//
//go:generate go run ./gen
package matrix
