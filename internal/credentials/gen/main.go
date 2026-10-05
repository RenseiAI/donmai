// Command gen renders the canonical agent-env blocklist artifact
// (internal/credentials/blocklist.json) from the Go source of truth
// (internal/credentials/blocklist.go, var AgentEnvBlocklist).
//
// Why a JSON artifact: the Go var is the single source of truth, but
// cross-repo consumers that cannot import Go — notably platform (Node/
// TypeScript) — need a language-neutral, vendorable rendering to check
// their own copy against. rensei-tui already renders a Go baseline from
// the same var (scripts/gen-blocklist.go); this JSON is the equivalent
// neutral interface, and its digest is computed with the identical
// algorithm so every repo ties to one hash.
//
// Digest: sha256 over the newline-terminated name sequence, in source
// order — byte-for-byte the same normalization rensei-tui uses, so a
// shared digest proves a shared list without comparing element-by-element.
//
// Determinism: no timestamps, stable key order, source order preserved.
// Identical input names produce byte-identical output.
//
// Freshness is enforced by TestBlocklistJSONParity in package credentials
// (it re-renders and compares against the committed file), which runs under
// `make test` and `make verify-generated` — mirroring how matrix/gen pairs
// a write-only generator with a parity test.
//
// Usage (output path resolves from the module root, so any CWD inside the
// module works):
//
//	go run ./internal/credentials/gen     # write blocklist.json
package main

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/RenseiAI/donmai/internal/credentials"
)

func main() {
	out, digest, err := credentials.RenderBlocklistJSON(credentials.AgentEnvBlocklist)
	if err != nil {
		fatalf("render: %v", err)
	}
	outPath := artifactPath()
	// 0o600: this is a committed, generated source artifact; the generator
	// never needs it group/world-writable, and gosec (G306) flags 0o644.
	if err := os.WriteFile(outPath, out, 0o600); err != nil {
		fatalf("write %s: %v", outPath, err)
	}
	fmt.Printf("gen: wrote %s (%d names, digest %s)\n", filepath.Base(outPath), len(credentials.AgentEnvBlocklist), digest)
}

// artifactPath resolves internal/credentials/blocklist.json from the module
// root, found by walking up from the working directory (repo root under
// `make` or `go run`, the package dir under `go generate`). It never uses
// runtime.Caller: under `-trimpath` that returns a module-relative path that
// does not exist on disk.
func artifactPath() string {
	wd, err := os.Getwd()
	if err != nil {
		fatalf("getwd: %v", err)
	}
	root, err := moduleRoot(wd)
	if err != nil {
		fatalf("%v", err)
	}
	return filepath.Join(root, "internal", "credentials", "blocklist.json")
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

func fatalf(format string, a ...any) {
	fmt.Fprintf(os.Stderr, "gen: error: "+format+"\n", a...)
	os.Exit(1)
}
