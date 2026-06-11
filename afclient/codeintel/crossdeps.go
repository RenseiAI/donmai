package codeintel

// crossdeps.go — Native Go port of validate-cross-deps (S3).
//
// Design matches the TS validateCrossDepsInProcess function from
// donmai-libraries/packages/code-intelligence/src/plugin/code-intelligence-plugin.ts.
//
// Algorithm:
//  1. Walk the cwd tree to discover workspace packages (by finding package.json
//     files that have a "name" field). Collect each package's declared
//     dependencies (dependencies + devDependencies + peerDependencies).
//  2. For each source file, find its owning package (longest directory prefix).
//  3. Parse import statements (TS/JS/MJS/CJS). For Go / Python / Rust files the
//     import statements are also scanned but Go module paths and Python packages
//     rarely have package.json entries, so false-positives are minimal.
//  4. For each import of a workspace package name, check that the importing
//     package's package.json declares it as a dependency.
//  5. Return the list of missing declarations, deduplicated by
//     (packageJsonPath, importedPackage).
//
// The JS/TS import parser replicates the TS isRealImportLine logic:
//   - skip block comments
//   - skip template-literal contents
//   - recognise static import, export { from }, dynamic require()
//
// Intentional deviation from TS: the Go port processes files synchronously
// (no async I/O). Performance is acceptable for repository-scale trees.

import (
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// CrossDepViolation is a single missing cross-package dependency declaration.
type CrossDepViolation struct {
	ImportingFile   string `json:"importingFile"`
	ImportedPackage string `json:"importedPackage"`
	PackageJSONPath string `json:"packageJsonPath"`
	Line            int    `json:"line"`
}

// CrossDepsResult is the output of ValidateCrossDeps.
type CrossDepsResult struct {
	Valid           bool                `json:"valid"`
	MissingDeps     []CrossDepViolation `json:"missingDeps"`
	PackagesChecked int                 `json:"packagesChecked"`
	FilesChecked    int                 `json:"filesChecked"`
}

type workspacePkg struct {
	name string
	dir  string
	deps map[string]bool
}

// ValidateCrossDeps checks that cross-package imports in a monorepo have
// corresponding package.json dependency declarations.
// targetPath optionally scopes the check to a specific directory prefix.
func ValidateCrossDeps(cwd, targetPath string) (CrossDepsResult, error) {
	// 1. Discover workspace packages.
	pkgs, err := discoverWorkspacePackages(cwd)
	if err != nil {
		return CrossDepsResult{}, fmt.Errorf("discover packages: %w", err)
	}
	pkgNames := make(map[string]bool, len(pkgs))
	for _, p := range pkgs {
		pkgNames[p.name] = true
	}

	var violations []CrossDepViolation
	filesChecked := 0

	// 2. Walk source files.
	err = filepath.WalkDir(cwd, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return nil
		}
		name := d.Name()
		if d.IsDir() {
			if skipDirs[name] || strings.HasPrefix(name, ".") {
				return filepath.SkipDir
			}
			return nil
		}
		ext := strings.ToLower(filepath.Ext(name))
		if !supportedExt[ext] {
			return nil
		}
		rel, _ := filepath.Rel(cwd, path)
		if targetPath != "" && !strings.HasPrefix(rel, targetPath) {
			return nil
		}

		owning := findOwningPkg(rel, pkgs)
		if owning == nil {
			return nil
		}

		data, err := os.ReadFile(path) //nolint:gosec
		if err != nil {
			return nil
		}
		filesChecked++

		lines := strings.Split(string(data), "\n")
		state := importParseState{}
		for i, line := range lines {
			isReal, nextState := classifyImportLine(line, state)
			state = nextState
			if !isReal {
				continue
			}
			imported := extractImportedPackage(line)
			if imported == "" || !pkgNames[imported] {
				continue
			}
			if !owning.deps[imported] {
				violations = append(violations, CrossDepViolation{
					ImportingFile:   rel,
					ImportedPackage: imported,
					PackageJSONPath: filepath.Join(owning.dir, "package.json"),
					Line:            i + 1,
				})
			}
		}
		return nil
	})
	if err != nil {
		return CrossDepsResult{}, fmt.Errorf("walk: %w", err)
	}

	// 3. Deduplicate violations by (packageJsonPath, importedPackage).
	seen := make(map[string]bool)
	unique := violations[:0]
	for _, v := range violations {
		key := v.PackageJSONPath + ":" + v.ImportedPackage
		if !seen[key] {
			seen[key] = true
			unique = append(unique, v)
		}
	}

	return CrossDepsResult{
		Valid:           len(unique) == 0,
		MissingDeps:     unique,
		PackagesChecked: len(pkgs),
		FilesChecked:    filesChecked,
	}, nil
}

// discoverWorkspacePackages walks cwd up to 5 levels deep to find package.json
// files that declare a package name.
func discoverWorkspacePackages(cwd string) ([]*workspacePkg, error) {
	var pkgs []*workspacePkg
	type entry struct {
		path  string
		depth int
	}
	stack := []entry{{cwd, 0}}
	for len(stack) > 0 {
		cur := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if cur.depth > 5 {
			continue
		}
		entries, err := os.ReadDir(cur.path)
		if err != nil {
			continue
		}
		for _, e := range entries {
			if skipDirs[e.Name()] {
				continue
			}
			fullPath := filepath.Join(cur.path, e.Name())
			if e.IsDir() {
				stack = append(stack, entry{fullPath, cur.depth + 1})
				continue
			}
			if e.Name() == "package.json" {
				data, err := os.ReadFile(fullPath) //nolint:gosec
				if err != nil {
					continue
				}
				var pkg struct {
					Name             string            `json:"name"`
					Dependencies     map[string]string `json:"dependencies"`
					DevDependencies  map[string]string `json:"devDependencies"`
					PeerDependencies map[string]string `json:"peerDependencies"`
				}
				if err := json.Unmarshal(data, &pkg); err != nil || pkg.Name == "" {
					continue
				}
				deps := make(map[string]bool)
				for k := range pkg.Dependencies {
					deps[k] = true
				}
				for k := range pkg.DevDependencies {
					deps[k] = true
				}
				for k := range pkg.PeerDependencies {
					deps[k] = true
				}
				rel, _ := filepath.Rel(cwd, cur.path)
				pkgs = append(pkgs, &workspacePkg{
					name: pkg.Name,
					dir:  rel,
					deps: deps,
				})
			}
		}
	}
	return pkgs, nil
}

// findOwningPkg returns the package with the longest directory-prefix match
// for the given file path, or nil if none found.
func findOwningPkg(filePath string, pkgs []*workspacePkg) *workspacePkg {
	var best *workspacePkg
	bestLen := -1
	for _, p := range pkgs {
		dir := p.dir
		if dir == "." {
			dir = ""
		}
		if dir == "" || strings.HasPrefix(filePath, dir+"/") || filePath == dir {
			if len(dir) > bestLen {
				bestLen = len(dir)
				best = p
			}
		}
	}
	return best
}

// ── Import-line parser ────────────────────────────────────────────────────────

type importParseState struct {
	inBlockComment    bool
	inTemplateLiteral bool
}

// classifyImportLine determines whether a line contains a real import/require
// statement, tracking comment and template-literal state.
// Matches the TS isRealImportLine function.
func classifyImportLine(line string, state importParseState) (isReal bool, next importParseState) {
	trimmed := strings.TrimSpace(line)
	next = state

	if state.inBlockComment {
		if strings.Contains(trimmed, "*/") {
			next.inBlockComment = false
		}
		return false, next
	}
	if strings.HasPrefix(trimmed, "/*") {
		if !strings.Contains(trimmed[2:], "*/") {
			next.inBlockComment = true
		}
		return false, next
	}
	if state.inTemplateLiteral {
		if countBackticks(line)%2 == 1 {
			next.inTemplateLiteral = false
		}
		return false, next
	}
	if strings.HasPrefix(trimmed, "//") || strings.HasPrefix(trimmed, "*") {
		return false, next
	}

	backs := countBackticks(line)
	if backs%2 == 1 {
		// Odd number of backticks means we're entering a template literal.
		importIdx := reImportOrExport.FindStringIndex(line)
		firstBacktick := strings.Index(line, "`")
		if importIdx != nil && firstBacktick >= 0 && importIdx[0] > firstBacktick {
			return false, importParseState{inTemplateLiteral: true}
		}
		if importIdx != nil && (firstBacktick < 0 || importIdx[0] < firstBacktick) {
			return true, importParseState{inTemplateLiteral: true}
		}
		return false, importParseState{inTemplateLiteral: true}
	}

	if reStaticImport.MatchString(line) {
		return true, next
	}
	if reRequire.MatchString(line) {
		reqIdx := strings.Index(line, "require")
		if reqIdx < 0 {
			return false, next
		}
		before := line[:reqIdx]
		if strings.Contains(before, "`") || strings.Contains(before, "'require") || strings.Contains(before, `"require`) {
			return false, next
		}
		return true, next
	}
	return false, next
}

func countBackticks(line string) int {
	count := 0
	for i := 0; i < len(line); i++ {
		if line[i] == '`' && (i == 0 || line[i-1] != '\\') {
			count++
		}
	}
	return count
}

// extractImportedPackage extracts the package name from an import/require line.
// Returns empty string if not a cross-package import.
func extractImportedPackage(line string) string {
	m := reImportPkg.FindStringSubmatch(line)
	if m == nil {
		return ""
	}
	return m[1]
}

// ── Compiled regexps ──────────────────────────────────────────────────────────

var (
	// Detect import/export keyword.
	reImportOrExport = regexp.MustCompile(`\b(import|export)\s`)
	// Static import or export: import ... / export ...
	reStaticImport = regexp.MustCompile(`^\s*(import|export)\s`)
	// require(...)
	reRequire = regexp.MustCompile(`\brequire\s*\(`)
	// Extract package name from: from 'pkg' / require('pkg') / import 'pkg'
	// Matches scoped (@org/pkg) and unscoped packages, not relative paths.
	reImportPkg = regexp.MustCompile(`(?:from\s+['"]|require\s*\(\s*['"]|import\s+['"])(@[^'"\/]+\/[^'"\/]+|[^.'"\/\\@][^'"\/]*)`)
)
