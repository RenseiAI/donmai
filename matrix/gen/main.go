// Command gen is the matrix code generator. Run via `go generate ./matrix/...`
// (or `make generate`). It harvests the live provider/endpoint manifests,
// validates the hand-authored cells, and writes the committed artifacts into
// the matrix/ package directory:
//
//	capability-matrix.json, harnesses.json, endpoints.json, matrix.json,
//	capability-realizations.json, registry_gen.go
//
// Output is deterministic (sorted everywhere, no timestamps) so the CI parity
// test (matrix/parity_test.go) can byte-compare a fresh regenerate against the
// committed files. The generator imports only in-module packages, so it runs
// identically with and without the go.work workspace (GOWORK=off parity).
package main

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/RenseiAI/donmai/matrix"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "matrix/gen: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	built, err := matrix.Build()
	if err != nil {
		return fmt.Errorf("build matrix: %w", err)
	}
	arts, err := built.Render()
	if err != nil {
		return fmt.Errorf("render artifacts: %w", err)
	}

	outDir, err := matrixDir()
	if err != nil {
		return err
	}

	for name, content := range arts.Files {
		path := filepath.Join(outDir, name)
		// 0o600: these are committed, generated source artifacts; the
		// generator never needs to make them group/world-writable, and gosec
		// G306 wants <=0600. git tracks the committed mode separately.
		if err := os.WriteFile(path, content, 0o600); err != nil {
			return fmt.Errorf("write %s: %w", name, err)
		}
		fmt.Fprintf(os.Stderr, "matrix/gen: wrote %s (%d bytes)\n", name, len(content))
	}
	return nil
}

// matrixDir resolves the matrix/ package directory from the module root, found
// by walking up from the working directory. go generate sets the CWD to the
// directory of the file holding the //go:generate directive (matrix/), and
// `go run ./matrix/gen` from the repo root has the repo root as CWD; both reach
// the same module root. runtime.Caller is deliberately avoided: under
// `-trimpath` it returns a module-relative path that does not exist on disk.
func matrixDir() (string, error) {
	wd, err := os.Getwd()
	if err != nil {
		return "", fmt.Errorf("getwd: %w", err)
	}
	root, err := moduleRoot(wd)
	if err != nil {
		return "", err
	}
	return filepath.Join(root, "matrix"), nil
}

// moduleRoot walks up from dir to the directory holding go.mod.
func moduleRoot(dir string) (string, error) {
	start := dir
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", fmt.Errorf("no go.mod found walking up from %s", start)
		}
		dir = parent
	}
}
