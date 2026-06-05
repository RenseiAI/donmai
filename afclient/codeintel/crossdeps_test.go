package codeintel

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// cdWriteFile is a test helper that writes content to a file, creating parent dirs.
func cdWriteFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		t.Fatalf("mkdir %s: %v", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil { //nolint:gosec
		t.Fatalf("cdWriteFile %s: %v", path, err)
	}
}

// cdWriteJSON marshals v to JSON and writes it to path.
func cdWriteJSON(t *testing.T, path string, v any) {
	t.Helper()
	data, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal %s: %v", path, err)
	}
	cdWriteFile(t, path, string(data))
}

// TestValidateCrossDeps_Clean verifies that a monorepo with correct declarations
// returns Valid=true and no missing deps.
func TestValidateCrossDeps_Clean(t *testing.T) {
	dir := t.TempDir()

	// Package A depends on package B — correctly declared.
	cdWriteJSON(t, filepath.Join(dir, "packages/a/package.json"), map[string]any{
		"name":         "@myorg/a",
		"dependencies": map[string]string{"@myorg/b": "^1.0.0"},
	})
	cdWriteFile(t, filepath.Join(dir, "packages/a/src/index.ts"),
		`import { helper } from '@myorg/b'
export function doA() { return helper() }
`)

	cdWriteJSON(t, filepath.Join(dir, "packages/b/package.json"), map[string]any{
		"name": "@myorg/b",
	})
	cdWriteFile(t, filepath.Join(dir, "packages/b/src/index.ts"),
		`export function helper() { return 42 }
`)

	result, err := ValidateCrossDeps(dir, "")
	if err != nil {
		t.Fatalf("ValidateCrossDeps: %v", err)
	}
	if !result.Valid {
		t.Errorf("expected Valid=true, got missingDeps: %+v", result.MissingDeps)
	}
	if result.PackagesChecked < 2 {
		t.Errorf("expected >= 2 packages checked, got %d", result.PackagesChecked)
	}
}

// TestValidateCrossDeps_MissingDep reports a missing dependency.
func TestValidateCrossDeps_MissingDep(t *testing.T) {
	dir := t.TempDir()

	// Package A imports from B but does NOT declare it.
	cdWriteJSON(t, filepath.Join(dir, "packages/a/package.json"), map[string]any{
		"name": "@myorg/a",
		// No dependency on @myorg/b
	})
	cdWriteFile(t, filepath.Join(dir, "packages/a/src/index.ts"),
		`import { helper } from '@myorg/b'
export function doA() { return helper() }
`)

	cdWriteJSON(t, filepath.Join(dir, "packages/b/package.json"), map[string]any{
		"name": "@myorg/b",
	})
	cdWriteFile(t, filepath.Join(dir, "packages/b/src/index.ts"),
		`export function helper() { return 42 }
`)

	result, err := ValidateCrossDeps(dir, "")
	if err != nil {
		t.Fatalf("ValidateCrossDeps: %v", err)
	}
	if result.Valid {
		t.Error("expected Valid=false when dep is missing")
	}
	if len(result.MissingDeps) == 0 {
		t.Error("expected at least one missing dep")
	}
	found := false
	for _, d := range result.MissingDeps {
		if d.ImportedPackage == "@myorg/b" {
			found = true
		}
	}
	if !found {
		t.Errorf("expected @myorg/b in missing deps, got: %+v", result.MissingDeps)
	}
}

// TestValidateCrossDeps_Dedup verifies that multiple imports of the same package
// from the same importing package appear only once.
func TestValidateCrossDeps_Dedup(t *testing.T) {
	dir := t.TempDir()

	cdWriteJSON(t, filepath.Join(dir, "packages/a/package.json"), map[string]any{
		"name": "@myorg/a",
		// No dep on @myorg/b
	})
	// Two files each import @myorg/b.
	cdWriteFile(t, filepath.Join(dir, "packages/a/src/file1.ts"),
		`import { x } from '@myorg/b'
`)
	cdWriteFile(t, filepath.Join(dir, "packages/a/src/file2.ts"),
		`import { y } from '@myorg/b'
`)

	cdWriteJSON(t, filepath.Join(dir, "packages/b/package.json"), map[string]any{
		"name": "@myorg/b",
	})
	cdWriteFile(t, filepath.Join(dir, "packages/b/src/index.ts"), `export const x = 1; export const y = 2`)

	result, err := ValidateCrossDeps(dir, "")
	if err != nil {
		t.Fatalf("ValidateCrossDeps: %v", err)
	}
	// Should be deduplicated to 1 violation per (packageJson, importedPackage).
	count := 0
	for _, d := range result.MissingDeps {
		if d.ImportedPackage == "@myorg/b" {
			count++
		}
	}
	if count != 1 {
		t.Errorf("expected 1 deduplicated violation for @myorg/b, got %d", count)
	}
}

// TestValidateCrossDeps_TargetPath scopes the check to a subdirectory.
func TestValidateCrossDeps_TargetPath(t *testing.T) {
	dir := t.TempDir()

	cdWriteJSON(t, filepath.Join(dir, "packages/a/package.json"), map[string]any{
		"name": "@myorg/a",
	})
	cdWriteFile(t, filepath.Join(dir, "packages/a/src/index.ts"),
		`import { x } from '@myorg/b'
`)

	cdWriteJSON(t, filepath.Join(dir, "packages/b/package.json"), map[string]any{
		"name": "@myorg/b",
	})
	cdWriteFile(t, filepath.Join(dir, "packages/b/src/index.ts"),
		`import { y } from '@myorg/a'
`)

	// Scope to packages/b only — packages/a violations should not appear.
	result, err := ValidateCrossDeps(dir, "packages/b")
	if err != nil {
		t.Fatalf("ValidateCrossDeps: %v", err)
	}
	for _, d := range result.MissingDeps {
		if !strings.HasPrefix(d.ImportingFile, "packages/b") {
			t.Errorf("violation from outside targetPath: %+v", d)
		}
	}
}

// TestImportClassifier verifies the import line classifier.
func TestImportClassifier(t *testing.T) {
	tests := []struct {
		line   string
		isReal bool
	}{
		{`import { x } from '@myorg/b'`, true},
		{`export { y } from '@myorg/b'`, true},
		{`const x = require('@myorg/b')`, true},
		{`// import { x } from '@myorg/b'`, false},
		{`/* import { x } from '@myorg/b' */`, false},
		{`const s = 'import not real'`, false},
		{``, false},
	}

	for _, tc := range tests {
		t.Run(tc.line, func(t *testing.T) {
			isReal, _ := classifyImportLine(tc.line, importParseState{})
			if isReal != tc.isReal {
				t.Errorf("classifyImportLine(%q) = %v, want %v", tc.line, isReal, tc.isReal)
			}
		})
	}
}
