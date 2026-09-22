package main

import (
	"os"
	"path/filepath"
	"testing"

	runtimeenv "github.com/RenseiAI/donmai/runtime/env"
)

// TestLoadDotenv_RefusesRunnerOnlyNames pins the one thing a dotenv file must
// never be able to do.
//
// The file is read from the process CWD, i.e. whatever repository the operator
// is standing in. The injection declaration re-admits blocklisted names from
// the INHERITED environment, so one committed line would otherwise hand every
// harness and PTY child of this invocation the key the operator exported in
// their login shell.
func TestLoadDotenv_RefusesRunnerOnlyNames(t *testing.T) {
	// Not parallel: it changes the process CWD and environment.
	dir := t.TempDir()
	path := filepath.Join(dir, ".env")
	contents := runtimeenv.InjectedEnvKeysVar + "=ANTHROPIC_API_KEY\n" +
		runtimeenv.GatewayUpstreamEnvKeysVar + "=OPENAI_API_KEY\n" +
		"ATTACH_TOKEN=repo-supplied\n" +
		"DONMAI_SESSION_SHIM_REGISTRY_DIR=/repo/registry\n" +
		"ORDINARY_SETTING=applied\n"
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatalf("write .env: %v", err)
	}

	for _, key := range []string{
		runtimeenv.InjectedEnvKeysVar,
		runtimeenv.GatewayUpstreamEnvKeysVar,
		"ATTACH_TOKEN",
		"DONMAI_SESSION_SHIM_REGISTRY_DIR",
		"ORDINARY_SETTING",
	} {
		t.Setenv(key, "")
		if err := os.Unsetenv(key); err != nil {
			t.Fatalf("unset %s: %v", key, err)
		}
	}

	loadDotenv(path)

	for _, key := range []string{
		runtimeenv.InjectedEnvKeysVar,
		runtimeenv.GatewayUpstreamEnvKeysVar,
		"ATTACH_TOKEN",
		"DONMAI_SESSION_SHIM_REGISTRY_DIR",
	} {
		if value, set := os.LookupEnv(key); set {
			t.Errorf("a dotenv file set the runner-only control %s=%q", key, value)
		}
	}
	if got := os.Getenv("ORDINARY_SETTING"); got != "applied" {
		t.Errorf("ORDINARY_SETTING = %q, want \"applied\" — ordinary dotenv entries must still apply", got)
	}
}

// TestLoadDotenv_ExistingEnvWins preserves godotenv.Load's documented
// precedence, which the manual application must not silently change.
func TestLoadDotenv_ExistingEnvWins(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ".env")
	if err := os.WriteFile(path, []byte("ALREADY_SET=from-file\nNOT_SET=from-file\n"), 0o600); err != nil {
		t.Fatalf("write .env: %v", err)
	}
	t.Setenv("ALREADY_SET", "from-process")
	t.Setenv("NOT_SET", "")
	if err := os.Unsetenv("NOT_SET"); err != nil {
		t.Fatalf("unset NOT_SET: %v", err)
	}

	loadDotenv(path)

	if got := os.Getenv("ALREADY_SET"); got != "from-process" {
		t.Errorf("ALREADY_SET = %q, want \"from-process\"", got)
	}
	if got := os.Getenv("NOT_SET"); got != "from-file" {
		t.Errorf("NOT_SET = %q, want \"from-file\"", got)
	}
}

// TestLoadDotenv_MissingFileIsNonFatal keeps dotenv files optional.
func TestLoadDotenv_MissingFileIsNonFatal(t *testing.T) {
	t.Parallel()
	loadDotenv(filepath.Join(t.TempDir(), "no-such-file"))
}
