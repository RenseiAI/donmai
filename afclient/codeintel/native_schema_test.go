package codeintel

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// TestLoadIndex_VersionMismatchDiscards proves the schema-version gate: a
// persisted index.json written under the legacy v1 shape (no "version" field)
// is discarded on load and reported as empty, forcing a clean full rebuild.
//
// RED (before the version gate in loadIndex):
//
//	native_schema_test.go: loaded 1 files from a v1 index; want 0 (discard+rebuild)
func TestLoadIndex_VersionMismatchDiscards(t *testing.T) {
	dir := t.TempDir()
	idxDir := filepath.Join(dir, ".donmai/code-index")
	if err := os.MkdirAll(idxDir, 0o750); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	// Legacy v1 index: no "version" key, one file entry.
	v1 := `{"files":{"a.go":{"filePath":"a.go","gitHash":"abc","symbols":[],"lastIndexed":1}},"rootHash":"deadbeef"}`
	if err := os.WriteFile(filepath.Join(idxDir, "index.json"), []byte(v1), 0o640); err != nil { //nolint:gosec // G306 test fixture
		t.Fatalf("write: %v", err)
	}

	nr := NewNativeRunner(dir)
	loaded := nr.loadIndex()
	if len(loaded.Files) != 0 {
		t.Errorf("loaded %d files from a v1 index; want 0 (version gate must discard+rebuild)", len(loaded.Files))
	}
	if loaded.Version != IndexSchemaVersion {
		t.Errorf("discarded index Version = %d; want %d", loaded.Version, IndexSchemaVersion)
	}
}

// TestLoadIndex_CurrentVersionKept verifies a v2 index round-trips through
// save+load with all fields intact.
func TestLoadIndex_CurrentVersionKept(t *testing.T) {
	dir := t.TempDir()
	nr := NewNativeRunner(dir)

	orig := IndexFile{
		Version:  IndexSchemaVersion,
		RootHash: "root123",
		Files: map[string]FileIndex{
			"pkg/a.ts": {
				FilePath:    "pkg/a.ts",
				GitHash:     "gitblobsha1",
				ContentHash: "0011223344556677",
				SimHash:     0xDEADBEEFCAFEBABE,
				Symbols:     []CodeSymbol{{Name: "Foo", Kind: KindClass, FilePath: "pkg/a.ts", Exported: true}},
				Imports:     []string{"./b", "react"},
				Exports:     []string{"Foo"},
				LastIndexed: 42,
			},
		},
	}
	if err := nr.saveIndex(orig); err != nil {
		t.Fatalf("saveIndex: %v", err)
	}

	// Confirm the persisted JSON actually carries the version tag.
	data, err := os.ReadFile(filepath.Join(dir, ".donmai/code-index/index.json")) //nolint:gosec
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("unmarshal raw: %v", err)
	}
	if v, _ := raw["version"].(float64); int(v) != IndexSchemaVersion {
		t.Errorf("persisted version = %v; want %d", raw["version"], IndexSchemaVersion)
	}

	loaded := nr.loadIndex()
	if len(loaded.Files) != 1 {
		t.Fatalf("loaded %d files; want 1", len(loaded.Files))
	}
	fi := loaded.Files["pkg/a.ts"]
	if fi.ContentHash != "0011223344556677" {
		t.Errorf("ContentHash = %q; want persisted value", fi.ContentHash)
	}
	if fi.SimHash != 0xDEADBEEFCAFEBABE {
		t.Errorf("SimHash = %#x; want persisted value", fi.SimHash)
	}
	if len(fi.Imports) != 2 || fi.Imports[0] != "./b" {
		t.Errorf("Imports = %v; want [./b react]", fi.Imports)
	}
	if len(fi.Exports) != 1 || fi.Exports[0] != "Foo" {
		t.Errorf("Exports = %v; want [Foo]", fi.Exports)
	}
}
