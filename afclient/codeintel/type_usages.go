package codeintel

// type_usages.go — Native Go port of find-type-usages (S3).
//
// Design matches the TS findTypeUsagesInProcess function from
// donmai-libraries/packages/code-intelligence/src/plugin/code-intelligence-plugin.ts.
//
// Supported usage kinds (same as TS):
//   - "import"           — import statement containing the type name
//   - "switch_case"      — switch(...) block that references the type
//   - "mapping_object"   — Record<TypeName...> or satisfies Record<TypeName...>
//   - "type_reference"   — type/interface declaration or annotation using the type
//   - "exhaustive_check" — assertNever / exhaustive check referencing the type
//
// Kind priority (lower = higher priority in sort): switch_case(0), mapping_object(1),
// exhaustive_check(2), type_reference(3), import(4).  Matches the TS sort order.

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// TypeUsageKind classifies how a type is used in a file.
type TypeUsageKind string

// Usage-kind constants ordered by reporting priority (lowest = highest priority).
const (
	UsageKindSwitchCase      TypeUsageKind = "switch_case"
	UsageKindMappingObject   TypeUsageKind = "mapping_object"
	UsageKindImport          TypeUsageKind = "import"
	UsageKindTypeReference   TypeUsageKind = "type_reference"
	UsageKindExhaustiveCheck TypeUsageKind = "exhaustive_check"
)

var usageKindPriority = map[TypeUsageKind]int{
	UsageKindSwitchCase:      0,
	UsageKindMappingObject:   1,
	UsageKindExhaustiveCheck: 2,
	UsageKindTypeReference:   3,
	UsageKindImport:          4,
}

// TypeUsage represents a single usage site of a type.
type TypeUsage struct {
	FilePath string        `json:"filePath"`
	Line     int           `json:"line"`
	Context  string        `json:"context"`
	Kind     TypeUsageKind `json:"kind"`
}

// TypeUsagesResult is the output of FindTypeUsages.
type TypeUsagesResult struct {
	TypeName         string      `json:"typeName"`
	TotalUsages      int         `json:"totalUsages"`
	Usages           []TypeUsage `json:"usages"`
	SwitchStatements int         `json:"switchStatements"`
	MappingObjects   int         `json:"mappingObjects"`
}

// FindTypeUsages searches the file tree under cwd for usages of typeName.
// Matches the TS findTypeUsagesInProcess function.
func FindTypeUsages(cwd, typeName string, maxResults int) (TypeUsagesResult, error) {
	if typeName == "" {
		return TypeUsagesResult{}, fmt.Errorf("typeName is required")
	}
	if maxResults <= 0 {
		maxResults = 50
	}

	escaped := regexp.QuoteMeta(typeName)
	reMapping := regexp.MustCompile(`Record<\s*` + escaped + `|satisfies\s+Record<\s*` + escaped)
	reTypeRef := regexp.MustCompile(`:\s*` + escaped + `\b`)
	reImportLine := regexp.MustCompile(`\bimport\b`)
	reSwitch := regexp.MustCompile(`switch\s*\(`)

	var usages []TypeUsage

	err := filepath.WalkDir(cwd, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return nil
		}
		name := d.Name()
		// Never follow symlinks. A symlinked *file* dirent has IsDir()==false and
		// would otherwise pass the extension filter and be os.ReadFile'd,
		// transparently following the link — a repo could plant pkg/innocent.go ->
		// ../outside-secret/x.go and exfiltrate arbitrary host file lines through
		// the agent-facing find-type-usages tool. Skipping the symlink dirent
		// excludes both symlinked files and symlinked directories (WalkDir already
		// never descends into a symlinked directory). Mirrors discoverFiles in
		// native.go.
		if d.Type()&fs.ModeSymlink != 0 {
			return nil
		}
		if d.IsDir() {
			if skipDirs[name] || strings.HasPrefix(name, ".") {
				return filepath.SkipDir
			}
			return nil
		}
		// Only search supported source files.
		ext := strings.ToLower(filepath.Ext(name))
		if !supportedExt[ext] {
			return nil
		}

		data, err := os.ReadFile(path) //nolint:gosec
		if err != nil {
			return nil
		}
		content := string(data)
		if !strings.Contains(content, typeName) {
			return nil
		}

		rel, _ := filepath.Rel(cwd, path)
		lines := strings.Split(content, "\n")

		for i, line := range lines {
			// 1. Import lines.
			if reImportLine.MatchString(line) && strings.Contains(line, typeName) {
				usages = append(usages, TypeUsage{
					FilePath: rel,
					Line:     i + 1,
					Context:  strings.TrimSpace(line),
					Kind:     UsageKindImport,
				})
				continue
			}

			// 2. Switch statements: look ahead up to 50 lines for the type.
			if reSwitch.MatchString(line) {
				windowEnd := i + 50
				if windowEnd >= len(lines) {
					windowEnd = len(lines) - 1
				}
				window := strings.Join(lines[i:windowEnd+1], "\n")
				// Context before: check type is referenced near the switch.
				before := strings.Join(lines[maxI(0, i-5):i+1], "\n")
				if strings.Contains(window, typeName) || strings.Contains(before, typeName) {
					usages = append(usages, TypeUsage{
						FilePath: rel,
						Line:     i + 1,
						Context:  strings.TrimSpace(line),
						Kind:     UsageKindSwitchCase,
					})
				}
				continue
			}

			// 3. Mapping objects: Record<TypeName...> or satisfies Record<TypeName...>
			if reMapping.MatchString(line) {
				usages = append(usages, TypeUsage{
					FilePath: rel,
					Line:     i + 1,
					Context:  strings.TrimSpace(line),
					Kind:     UsageKindMappingObject,
				})
				continue
			}

			// 4. Exhaustive checks.
			if (strings.Contains(line, "assertNever") || strings.Contains(line, "exhaustive")) &&
				strings.Contains(content, typeName) {
				usages = append(usages, TypeUsage{
					FilePath: rel,
					Line:     i + 1,
					Context:  strings.TrimSpace(line),
					Kind:     UsageKindExhaustiveCheck,
				})
				continue
			}

			// 5. Type references: "type TypeName" / "interface TypeName" / ": TypeName"
			if !reImportLine.MatchString(line) &&
				(strings.Contains(line, "type "+typeName) ||
					strings.Contains(line, "interface "+typeName) ||
					reTypeRef.MatchString(line)) {
				usages = append(usages, TypeUsage{
					FilePath: rel,
					Line:     i + 1,
					Context:  strings.TrimSpace(line),
					Kind:     UsageKindTypeReference,
				})
			}
		}
		return nil
	})
	if err != nil {
		return TypeUsagesResult{}, fmt.Errorf("walk: %w", err)
	}

	// Sort by kind priority (matching TS sort order).
	sort.SliceStable(usages, func(i, j int) bool {
		pi := usageKindPriority[usages[i].Kind]
		pj := usageKindPriority[usages[j].Kind]
		return pi < pj
	})

	switchCount := 0
	mappingCount := 0
	for _, u := range usages {
		if u.Kind == UsageKindSwitchCase {
			switchCount++
		} else if u.Kind == UsageKindMappingObject {
			mappingCount++
		}
	}

	total := len(usages)
	if maxResults < len(usages) {
		usages = usages[:maxResults]
	}

	return TypeUsagesResult{
		TypeName:         typeName,
		TotalUsages:      total,
		Usages:           usages,
		SwitchStatements: switchCount,
		MappingObjects:   mappingCount,
	}, nil
}

func maxI(a, b int) int {
	if a > b {
		return a
	}
	return b
}

// supportedExt is the set of file extensions scanned by FindTypeUsages and
// ValidateCrossDeps. Matches the SUPPORTED_EXTENSIONS set in the TS plugin.
var supportedExt = map[string]bool{
	".ts":  true,
	".tsx": true,
	".js":  true,
	".jsx": true,
	".mjs": true,
	".cjs": true,
	".py":  true,
	".go":  true,
	".rs":  true,
}
