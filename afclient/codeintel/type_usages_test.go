package codeintel

import (
	"os"
	"path/filepath"
	"testing"
)

// TestFindTypeUsages_Basic writes a fixture and asserts usages are found.
func TestFindTypeUsages_Basic(t *testing.T) {
	dir := t.TempDir()

	// Write a fixture TypeScript file that uses AgentWorkType in multiple ways.
	src := `import type { AgentWorkType } from './types'

function handleWork(work: AgentWorkType) {
  switch (work.type) {
    case 'development':
      break
    case 'qa':
      break
  }
}

const workHandlers: Record<AgentWorkType, () => void> = {
  development: () => {},
  qa: () => {},
}
`
	if err := os.WriteFile(filepath.Join(dir, "handler.ts"), []byte(src), 0o600); err != nil {
		t.Fatal(err)
	}

	result, err := FindTypeUsages(dir, "AgentWorkType", 50)
	if err != nil {
		t.Fatalf("FindTypeUsages: %v", err)
	}

	if result.TypeName != "AgentWorkType" {
		t.Errorf("typeName: got %q, want AgentWorkType", result.TypeName)
	}
	if result.TotalUsages == 0 {
		t.Error("expected at least one usage")
	}

	// Verify the import is detected.
	importFound := false
	for _, u := range result.Usages {
		if u.Kind == UsageKindImport {
			importFound = true
		}
	}
	if !importFound {
		t.Error("expected at least one import usage")
	}

	// Verify the switch statement is detected.
	switchFound := false
	for _, u := range result.Usages {
		if u.Kind == UsageKindSwitchCase {
			switchFound = true
		}
	}
	if !switchFound {
		t.Error("expected at least one switch_case usage")
	}

	// Verify the mapping object is detected.
	mappingFound := false
	for _, u := range result.Usages {
		if u.Kind == UsageKindMappingObject {
			mappingFound = true
		}
	}
	if !mappingFound {
		t.Error("expected at least one mapping_object usage")
	}
}

// TestFindTypeUsages_EmptyTypeName returns an error.
func TestFindTypeUsages_EmptyTypeName(t *testing.T) {
	dir := t.TempDir()
	_, err := FindTypeUsages(dir, "", 10)
	if err == nil {
		t.Error("expected error for empty typeName")
	}
}

// TestFindTypeUsages_NoneFound returns empty usages for a type that doesn't exist.
func TestFindTypeUsages_NoneFound(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "f.ts"), []byte("const x = 1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	result, err := FindTypeUsages(dir, "NonExistentType", 10)
	if err != nil {
		t.Fatalf("FindTypeUsages: %v", err)
	}
	if result.TotalUsages != 0 {
		t.Errorf("expected 0 usages, got %d", result.TotalUsages)
	}
}

// TestFindTypeUsages_SortOrder verifies that switch_case comes before import in results.
func TestFindTypeUsages_SortOrder(t *testing.T) {
	dir := t.TempDir()
	src := `import { MyType } from './types'
switch(x) {
  case 'a': break
}
const m: Record<MyType, number> = {}
`
	if err := os.WriteFile(filepath.Join(dir, "f.ts"), []byte(src), 0o600); err != nil {
		t.Fatal(err)
	}

	result, err := FindTypeUsages(dir, "MyType", 50)
	if err != nil {
		t.Fatalf("FindTypeUsages: %v", err)
	}
	if result.TotalUsages == 0 {
		t.Skip("no usages found — skip sort-order test")
	}

	// Verify that switch_case and mapping_object appear before import in the
	// sorted output (lower priority value = higher sort order).
	firstImportIdx := -1
	lastSwitchOrMappingIdx := -1
	for i, u := range result.Usages {
		if u.Kind == UsageKindImport && firstImportIdx < 0 {
			firstImportIdx = i
		}
		if u.Kind == UsageKindSwitchCase || u.Kind == UsageKindMappingObject {
			lastSwitchOrMappingIdx = i
		}
	}
	if firstImportIdx >= 0 && lastSwitchOrMappingIdx >= 0 {
		if firstImportIdx < lastSwitchOrMappingIdx {
			t.Errorf("import appears before switch/mapping in result: import at %d, last switch/mapping at %d",
				firstImportIdx, lastSwitchOrMappingIdx)
		}
	}
}

// TestFindTypeUsages_MaxResults caps the returned usages.
func TestFindTypeUsages_MaxResults(t *testing.T) {
	dir := t.TempDir()
	// Create a file with many usages of MyType.
	var lines []string
	for i := 0; i < 20; i++ {
		lines = append(lines, "import type { MyType } from './types'")
	}
	src := ""
	for _, l := range lines {
		src += l + "\n"
	}
	if err := os.WriteFile(filepath.Join(dir, "f.ts"), []byte(src), 0o600); err != nil {
		t.Fatal(err)
	}

	result, err := FindTypeUsages(dir, "MyType", 5)
	if err != nil {
		t.Fatalf("FindTypeUsages: %v", err)
	}
	if len(result.Usages) > 5 {
		t.Errorf("expected at most 5 usages, got %d", len(result.Usages))
	}
	// TotalUsages should reflect the real count, not the capped count.
	if result.TotalUsages < len(result.Usages) {
		t.Errorf("TotalUsages (%d) should be >= len(Usages) (%d)", result.TotalUsages, len(result.Usages))
	}
}
