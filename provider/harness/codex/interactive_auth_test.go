package codex

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/RenseiAI/donmai/agent"
)

func clearInteractiveCodexAuthEnv(t *testing.T) {
	t.Helper()
	for _, key := range codexEnvironmentAuthKeys {
		t.Setenv(key, "")
	}
}

func writeHostCodexAuthConfig(t *testing.T, home, mode string) {
	t.Helper()
	body := "cli_auth_credentials_store = \"" + mode + "\"\n"
	if err := os.WriteFile(filepath.Join(home, "config.toml"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

func writeHostCodexAuthFile(t *testing.T, home string) string {
	t.Helper()
	path := filepath.Join(home, codexAuthFileName)
	if err := os.WriteFile(path, []byte(`{"auth_mode":"chatgpt","tokens":{"access_token":"fixture"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestResolveInteractiveCodexAuthDistinguishesEnvironmentFileAndKeyring(t *testing.T) {
	clearInteractiveCodexAuthEnv(t)
	hostHome := t.TempDir()
	t.Setenv("CODEX_HOME", hostHome)

	t.Run("explicit environment auth needs no host credential file", func(t *testing.T) {
		projection, err := resolveInteractiveCodexAuth(map[string]string{"OPENAI_API_KEY": "fixture"})
		if err != nil || projection.kind != interactiveCodexAuthEnvironment || projection.hostAuthFile != "" {
			t.Fatalf("environment projection = %+v err=%v", projection, err)
		}
	})

	t.Run("file mode projects the exact host auth inode", func(t *testing.T) {
		writeHostCodexAuthConfig(t, hostHome, "file")
		hostAuth := writeHostCodexAuthFile(t, hostHome)
		projection, err := resolveInteractiveCodexAuth(nil)
		if err != nil || projection.kind != interactiveCodexAuthFile || projection.storeMode != "file" || projection.hostAuthFile != hostAuth {
			t.Fatalf("file projection = %+v err=%v", projection, err)
		}
	})

	t.Run("keyring without portable environment auth fails before spawn", func(t *testing.T) {
		writeHostCodexAuthConfig(t, hostHome, "keyring")
		_, err := resolveInteractiveCodexAuth(nil)
		if !errors.Is(err, ErrInteractiveCodexAuthProjection) || !strings.Contains(err.Error(), "keyring") {
			t.Fatalf("keyring error = %v", err)
		}
	})

	t.Run("keyring selection is preserved when environment auth is portable", func(t *testing.T) {
		projection, err := resolveInteractiveCodexAuth(map[string]string{"CODEX_API_KEY": "fixture"})
		if err != nil || projection.kind != interactiveCodexAuthEnvironment || projection.storeMode != "file" || projection.envKey != "CODEX_API_KEY" {
			t.Fatalf("keyring + environment projection = %+v err=%v", projection, err)
		}
	})

	t.Run("auto remains ambiguous even when a stale auth file exists", func(t *testing.T) {
		writeHostCodexAuthConfig(t, hostHome, "auto")
		_, err := resolveInteractiveCodexAuth(nil)
		if !errors.Is(err, ErrInteractiveCodexAuthProjection) || !strings.Contains(err.Error(), "does not prove") {
			t.Fatalf("auto error = %v", err)
		}
	})
}

func TestResolveInteractiveCodexAuthRefusesAbsentOrMalformedAuthority(t *testing.T) {
	clearInteractiveCodexAuthEnv(t)

	t.Run("no environment and no auth file", func(t *testing.T) {
		hostHome := t.TempDir()
		t.Setenv("CODEX_HOME", hostHome)
		_, err := resolveInteractiveCodexAuth(nil)
		if !errors.Is(err, ErrInteractiveCodexAuthProjection) || !strings.Contains(err.Error(), "auth.json is absent") {
			t.Fatalf("missing-auth error = %v", err)
		}
	})

	t.Run("malformed host config", func(t *testing.T) {
		hostHome := t.TempDir()
		t.Setenv("CODEX_HOME", hostHome)
		if err := os.WriteFile(filepath.Join(hostHome, "config.toml"), []byte("not = [valid"), 0o600); err != nil {
			t.Fatal(err)
		}
		_, err := resolveInteractiveCodexAuth(nil)
		if !errors.Is(err, ErrInteractiveCodexAuthProjection) || !strings.Contains(err.Error(), "parse host") {
			t.Fatalf("malformed-config error = %v", err)
		}
	})
}

func TestInteractiveCodexEnvironmentAuthUsesEphemeralFileStoreWithoutImportingConfig(t *testing.T) {
	clearInteractiveCodexAuthEnv(t)
	hostHome := t.TempDir()
	boundaryParent := t.TempDir()
	t.Setenv("CODEX_HOME", hostHome)
	writeHostCodexAuthConfig(t, hostHome, "keyring")
	if err := os.WriteFile(
		filepath.Join(hostHome, "config.toml"),
		[]byte("cli_auth_credentials_store = \"keyring\"\n[mcp_servers.poison]\ncommand = \"false\"\n"),
		0o600,
	); err != nil {
		t.Fatal(err)
	}

	boundary, projection, err := newInteractiveCodexConfigBoundary(
		boundaryParent,
		map[string]string{"OPENAI_API_KEY": "fixture"},
	)
	if err != nil {
		t.Fatalf("new boundary: %v", err)
	}
	if projection.kind != interactiveCodexAuthEnvironment {
		t.Fatalf("projection = %+v", projection)
	}
	t.Cleanup(func() { _ = boundary.remove() })
	body, err := os.ReadFile(boundary.configPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), codexFileAuthConfig) || strings.Contains(string(body), "poison") {
		t.Fatalf("private config did not select only the ephemeral file store: %s", body)
	}
	if _, err := os.Lstat(filepath.Join(boundary.home, codexAuthFileName)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("environment-auth boundary unexpectedly contains auth.json: %v", err)
	}
}

func TestSpawnInteractiveKeyringAuthRefusesBeforePTY(t *testing.T) {
	clearInteractiveCodexAuthEnv(t)
	hostHome := t.TempDir()
	workdir := t.TempDir()
	t.Setenv("CODEX_HOME", hostHome)
	writeHostCodexAuthConfig(t, hostHome, "keyring")
	bin := writeFakeCodexScript(t, `touch "$PWD/spawned"`)

	_, err := SpawnInteractive(context.Background(), Options{CodexBin: bin}, agent.Spec{
		Cwd: workdir,
		MCPServers: []agent.MCPServerConfig{{
			Name: "donmai-platform", Type: "http", URL: "https://platform.example/api/mcp/session",
		}},
		Interactive: &agent.InteractiveSpec{},
	})
	if !errors.Is(err, ErrInteractiveCodexAuthProjection) || !errors.Is(err, agent.ErrSpawnFailed) {
		t.Fatalf("keyring spawn error = %v", err)
	}
	if _, statErr := os.Stat(filepath.Join(workdir, "spawned")); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("PTY started after keyring projection refusal: %v", statErr)
	}
}
