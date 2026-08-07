// Package testregistration guards against build-tag-gated test files that no
// target ever compiles.
//
// The defect class: a `_test.go` file behind `//go:build sometag` is invisible
// to `go test ./...` and `go build ./...`. It is not run, not compiled, and not
// even syntax-checked — the toolchain silently excludes it. The suite is green
// and the file might not have compiled for a year. This is the Go spelling of
// the "unregistered suite" shape in the unfailable-test gate
// (donmai-architecture/agents/PROTOCOL.md § V, V16-V21): coverage is reported,
// nothing is proven, and only reverting the production change would reveal it.
//
// "Registered" here means a target or CI step supplies the tag via `-tags`, so
// the file is at minimum COMPILED. Compilation is the honest bar: a tagged
// suite usually needs live services CI cannot provide, but bit-rot — a renamed
// symbol, a changed signature — is what actually kills these files, and that is
// caught the moment they are type-checked. Executing them is better; compiling
// them is the floor.
//
// Both sides are derived, never hand-maintained: the EXPECTED set is every tag
// found on a `_test.go` file on disk, and the ACTUAL set is parsed out of the
// real Makefile and workflow text. There is no list to forget to update.
package testregistration

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// ToolchainControlled holds build-constraint identifiers the go tool sets
// itself from GOOS/GOARCH/flags. These are never supplied via `-tags`, so
// requiring registration for them would be a guaranteed false failure.
var ToolchainControlled = map[string]bool{
	// Race/cgo/compiler selection.
	"race": true, "cgo": true, "gc": true, "gccgo": true, "msan": true, "asan": true,
	// GOOS values and the meta-tag.
	"aix": true, "android": true, "darwin": true, "dragonfly": true, "freebsd": true,
	"hurd": true, "illumos": true, "ios": true, "js": true, "linux": true, "nacl": true,
	"netbsd": true, "openbsd": true, "plan9": true, "solaris": true, "wasip1": true,
	"windows": true, "zos": true, "unix": true,
	// GOARCH values.
	"386": true, "amd64": true, "arm": true, "arm64": true, "loong64": true,
	"mips": true, "mips64": true, "mips64le": true, "mipsle": true, "ppc64": true,
	"ppc64le": true, "riscv64": true, "s390x": true, "wasm": true,
}

var (
	buildLineRE = regexp.MustCompile(`(?m)^//go:build\s+(.*)$`)
	identRE     = regexp.MustCompile(`[A-Za-z_][A-Za-z0-9_.]*`)
	// `-tags a,b` / `-tags="a b"` / `--tags 'a'` — every spelling the go tool accepts.
	tagsFlagRE = regexp.MustCompile(`-{1,2}tags[=\s]+["']?([A-Za-z0-9_.,\s]+)["']?`)
	goVersion  = regexp.MustCompile(`^go1\.\d+`)
)

// CustomTags returns the build-constraint identifiers in a `_test.go` source
// that a human must opt into with `-tags`. Toolchain-controlled identifiers,
// `go1.x` version constraints, and the boolean operators are excluded.
func CustomTags(source string) []string {
	match := buildLineRE.FindStringSubmatch(source)
	if match == nil {
		return nil
	}
	seen := map[string]bool{}
	var out []string
	for _, ident := range identRE.FindAllString(match[1], -1) {
		if ToolchainControlled[ident] || goVersion.MatchString(ident) || seen[ident] {
			continue
		}
		seen[ident] = true
		out = append(out, ident)
	}
	return out
}

// SuppliedTags returns every tag any `-tags` flag in the given build text
// supplies. Callers pass the concatenated Makefile and workflow sources.
func SuppliedTags(buildText string) map[string]bool {
	supplied := map[string]bool{}
	for _, match := range tagsFlagRE.FindAllStringSubmatch(buildText, -1) {
		for _, field := range strings.FieldsFunc(match[1], func(r rune) bool {
			return r == ',' || r == ' ' || r == '\t' || r == '\n'
		}) {
			if field != "" {
				supplied[field] = true
			}
		}
	}
	return supplied
}

// Unregistered describes one test file gated behind a tag nothing supplies.
type Unregistered struct {
	File string
	Tag  string
}

// Check pairs every custom tag on disk against the supplied set.
// Pure over its inputs so the unit test can drive it with synthetic sources.
func Check(testFiles map[string]string, buildText string) []Unregistered {
	supplied := SuppliedTags(buildText)
	var out []Unregistered
	for _, file := range sortedKeys(testFiles) {
		for _, tag := range CustomTags(testFiles[file]) {
			if !supplied[tag] {
				out = append(out, Unregistered{File: file, Tag: tag})
			}
		}
	}
	return out
}

func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for key := range m {
		keys = append(keys, key)
	}
	for i := 1; i < len(keys); i++ {
		for j := i; j > 0 && keys[j] < keys[j-1]; j-- {
			keys[j], keys[j-1] = keys[j-1], keys[j]
		}
	}
	return keys
}

var skipDirs = map[string]bool{
	".git": true, "node_modules": true, "bin": true, "dist": true, "vendor": true,
}

// CollectTestFiles reads every `_test.go` under root, keyed by repo-relative
// path. Paths are gathered first and read afterwards, so no filesystem
// operation happens inside the WalkDir callback (gosec G122: a read there races
// the walk and is symlink-TOCTOU prone).
func CollectTestFiles(root string) (map[string]string, error) {
	var paths []string
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			if skipDirs[entry.Name()] {
				return filepath.SkipDir
			}
			return nil
		}
		if strings.HasSuffix(entry.Name(), "_test.go") {
			paths = append(paths, path)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}

	files := make(map[string]string, len(paths))
	for _, path := range paths {
		// #nosec G304 -- path was produced by walking the repo's own tree.
		data, readErr := os.ReadFile(path)
		if readErr != nil {
			return nil, readErr
		}
		rel, relErr := filepath.Rel(root, path)
		if relErr != nil {
			return nil, relErr
		}
		files[filepath.ToSlash(rel)] = string(data)
	}
	return files, nil
}

// CollectBuildText concatenates the Makefile and every workflow file — the
// places a `-tags` flag can legitimately live.
func CollectBuildText(root string) (string, error) {
	var builder strings.Builder
	// #nosec G304 -- fixed filename under the repo root this guard is inspecting.
	if data, err := os.ReadFile(filepath.Join(root, "Makefile")); err == nil {
		builder.Write(data)
	} else if !os.IsNotExist(err) {
		return "", err
	}
	entries, err := os.ReadDir(filepath.Join(root, ".github/workflows"))
	if err != nil {
		if os.IsNotExist(err) {
			return builder.String(), nil
		}
		return "", err
	}
	for _, entry := range entries {
		if entry.IsDir() || !(strings.HasSuffix(entry.Name(), ".yml") || strings.HasSuffix(entry.Name(), ".yaml")) {
			continue
		}
		// #nosec G304 -- entry comes from ReadDir of the repo's own workflow dir.
		data, readErr := os.ReadFile(filepath.Join(root, ".github/workflows", entry.Name()))
		if readErr != nil {
			return "", readErr
		}
		builder.Write(data)
		builder.WriteString("\n")
	}
	return builder.String(), nil
}

// RepoRoot walks up from dir until it finds the directory holding go.mod.
func RepoRoot(dir string) (string, error) {
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", fmt.Errorf("no go.mod found walking up from %s", dir)
		}
		dir = parent
	}
}
