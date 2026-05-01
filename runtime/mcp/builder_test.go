package mcp_test

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/RenseiAI/agentfactory-tui/agent"
	"github.com/RenseiAI/agentfactory-tui/runtime/mcp"
)

func TestBuildEmptyReturnsEmptyPath(t *testing.T) {
	t.Parallel()

	b := mcp.NewBuilder()
	path, cleanup, err := b.Build(nil)
	if err != nil {
		t.Fatalf("Build(nil): %v", err)
	}
	if path != "" {
		t.Fatalf("expected empty path for nil servers, got %q", path)
	}
	if cleanup == nil {
		t.Fatal("cleanup should never be nil")
	}
	cleanup() // must not panic
}

func TestBuildRoundtrip(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	b := &mcp.Builder{TempDir: dir, Prefix: "rt-"}
	servers := []agent.MCPServerConfig{
		{
			Name:    "af-linear",
			Command: "/usr/local/bin/af",
			Args:    []string{"linear-mcp"},
			Env:     map[string]string{"LINEAR_API_KEY": "sk-test"},
		},
		{
			Name:    "af-code",
			Command: "/usr/local/bin/af",
			Args:    []string{"code-mcp"},
		},
	}
	path, cleanup, err := b.Build(servers)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	t.Cleanup(cleanup)

	if !filepath.IsAbs(path) {
		t.Fatalf("expected absolute path, got %q", path)
	}
	if !strings.HasPrefix(filepath.Base(path), "rt-") {
		t.Fatalf("expected prefix rt-, got %q", filepath.Base(path))
	}
	if !strings.HasSuffix(path, ".json") {
		t.Fatalf("expected .json suffix, got %q", path)
	}

	cfg, err := mcp.LoadConfigFile(path)
	if err != nil {
		t.Fatalf("LoadConfigFile: %v", err)
	}
	if len(cfg.MCPServers) != 2 {
		t.Fatalf("expected 2 servers, got %d", len(cfg.MCPServers))
	}
	if got := cfg.MCPServers["af-linear"]; got.Type != "stdio" || got.Command != "/usr/local/bin/af" ||
		!reflect.DeepEqual(got.Args, []string{"linear-mcp"}) ||
		got.Env["LINEAR_API_KEY"] != "sk-test" {
		t.Fatalf("af-linear roundtrip mismatch: %+v", got)
	}
	if got := cfg.MCPServers["af-code"]; got.Type != "stdio" || got.Command != "/usr/local/bin/af" ||
		!reflect.DeepEqual(got.Args, []string{"code-mcp"}) || got.Env != nil {
		t.Fatalf("af-code roundtrip mismatch: %+v", got)
	}
}

func TestBuildCleanupActuallyDeletes(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	b := &mcp.Builder{TempDir: dir}
	path, cleanup, err := b.Build([]agent.MCPServerConfig{
		{Name: "n", Command: "/bin/true", Args: []string{}},
	})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("expected tmpfile to exist after Build, got: %v", err)
	}
	cleanup()
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("expected tmpfile gone after cleanup, got stat err: %v", err)
	}
	// idempotent
	cleanup()
}

func TestBuildMultipleSessionsDoNotCollide(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	b := &mcp.Builder{TempDir: dir}
	const n = 16

	var (
		wg    sync.WaitGroup
		mu    sync.Mutex
		paths = make(map[string]struct{}, n)
	)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			p, cleanup, err := b.Build([]agent.MCPServerConfig{
				{Name: "n", Command: "/bin/true", Args: []string{}},
			})
			if err != nil {
				t.Errorf("Build: %v", err)
				return
			}
			defer cleanup()
			mu.Lock()
			paths[p] = struct{}{}
			mu.Unlock()
		}()
	}
	wg.Wait()
	if len(paths) != n {
		t.Fatalf("expected %d unique tmpfile paths, got %d (collisions)", n, len(paths))
	}
}

func TestBuildRejectsInvalidServers(t *testing.T) {
	t.Parallel()

	b := mcp.NewBuilder()
	cases := []struct {
		name    string
		servers []agent.MCPServerConfig
		needle  string
	}{
		{
			name:    "empty Name",
			servers: []agent.MCPServerConfig{{Name: "", Command: "/bin/true"}},
			needle:  "empty Name",
		},
		{
			name:    "empty Command",
			servers: []agent.MCPServerConfig{{Name: "n", Command: ""}},
			needle:  "empty Command",
		},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, _, err := b.Build(tc.servers)
			if err == nil {
				t.Fatal("expected error, got nil")
			}
			if !strings.Contains(err.Error(), tc.needle) {
				t.Fatalf("error %q missing %q", err.Error(), tc.needle)
			}
		})
	}
}

func TestBuildConfigFilePreservesOrderInKeys(t *testing.T) {
	t.Parallel()

	cfg, err := mcp.BuildConfigFile([]agent.MCPServerConfig{
		{Name: "z", Command: "/bin/true"},
		{Name: "a", Command: "/bin/true"},
	})
	if err != nil {
		t.Fatalf("BuildConfigFile: %v", err)
	}
	if _, ok := cfg.MCPServers["z"]; !ok {
		t.Fatal("missing key z")
	}
	if _, ok := cfg.MCPServers["a"]; !ok {
		t.Fatal("missing key a")
	}
}

func TestBuildDefensivelyCopiesArgsAndEnv(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	args := []string{"a", "b"}
	envMap := map[string]string{"K": "v"}
	in := []agent.MCPServerConfig{
		{Name: "n", Command: "/bin/true", Args: args, Env: envMap},
	}

	b := &mcp.Builder{TempDir: dir}
	path, cleanup, err := b.Build(in)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	defer cleanup()

	args[0] = "MUTATED"
	envMap["K"] = "MUTATED"

	cfg, err := mcp.LoadConfigFile(path)
	if err != nil {
		t.Fatalf("LoadConfigFile: %v", err)
	}
	if got := cfg.MCPServers["n"]; got.Args[0] != "a" || got.Env["K"] != "v" {
		t.Fatalf("input mutation leaked into config file: %+v", got)
	}
}
