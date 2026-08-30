package codex

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/BurntSushi/toml"
)

// ErrInteractiveCodexAuthProjection identifies a platform-session spawn whose
// host authentication cannot be carried into the private CODEX_HOME without
// importing caller-global configuration. Callers can distinguish this posture
// from an ordinary process failure with errors.Is.
var ErrInteractiveCodexAuthProjection = errors.New("codex interactive authentication cannot be projected into the isolated session")

var codexEnvironmentAuthKeys = [...]string{
	"OPENAI_API_KEY",
	"CODEX_API_KEY",
	"CODEX_ACCESS_TOKEN",
}

type interactiveCodexAuthKind string

const (
	interactiveCodexAuthEnvironment interactiveCodexAuthKind = "environment"
	interactiveCodexAuthFile        interactiveCodexAuthKind = "file"
)

type interactiveCodexAuthProjection struct {
	kind         interactiveCodexAuthKind
	storeMode    string
	hostAuthFile string
	envKey       string
	envValue     string
}

type interactiveCodexAuthSeeder func(
	ctx context.Context,
	binary string,
	ownedHome string,
	projection interactiveCodexAuthProjection,
) error

type hostCodexAuthConfig struct {
	StoreMode string `toml:"cli_auth_credentials_store"`
}

func interactiveCodexAuthError(detail string) error {
	return fmt.Errorf("%w: %s", ErrInteractiveCodexAuthProjection, detail)
}

// resolveInteractiveCodexAuth selects a credential source using evidence the
// isolated child can actually consume. Environment auth is seeded through
// Codex's own login command into the ephemeral private file store. File auth is
// projected by inode. Keyring and explicit auto are refused: OS credential
// entries are scoped to the canonical host CODEX_HOME and Codex exposes no
// secret-free projection API for a different home.
func resolveInteractiveCodexAuth(specEnv map[string]string) (interactiveCodexAuthProjection, error) {
	hostAuthFile, err := resolveHostSessionAuthFile()
	if err != nil {
		return interactiveCodexAuthProjection{}, interactiveCodexAuthError(err.Error())
	}
	hostHome := filepath.Dir(hostAuthFile)
	storeMode, explicitMode, err := readHostCodexAuthStoreMode(hostHome)
	if err != nil {
		return interactiveCodexAuthProjection{}, interactiveCodexAuthError(err.Error())
	}

	if key, value, ok := resolveEnvironmentCodexAuth(specEnv); ok {
		return interactiveCodexAuthProjection{
			kind:      interactiveCodexAuthEnvironment,
			storeMode: "file",
			envKey:    key,
			envValue:  value,
		}, nil
	}

	switch storeMode {
	case "keyring":
		return interactiveCodexAuthProjection{}, interactiveCodexAuthError(
			"host cli_auth_credentials_store=keyring is bound to the canonical CODEX_HOME; " +
				"set OPENAI_API_KEY, CODEX_API_KEY, or CODEX_ACCESS_TOKEN for this session, " +
				"or select file storage and run codex login before launching",
		)
	case "auto":
		return interactiveCodexAuthProjection{}, interactiveCodexAuthError(
			"host cli_auth_credentials_store=auto does not prove whether the active credential is in the OS keyring or auth.json; " +
				"set OPENAI_API_KEY, CODEX_API_KEY, or CODEX_ACCESS_TOKEN for this session, " +
				"or select file storage and run codex login before launching",
		)
	case "file", "":
		if _, err := os.Lstat(hostAuthFile); err != nil {
			if errors.Is(err, os.ErrNotExist) {
				modeDetail := "no explicit credential-store mode"
				if explicitMode {
					modeDetail = "cli_auth_credentials_store=file"
				}
				return interactiveCodexAuthProjection{}, interactiveCodexAuthError(
					modeDetail + " but host auth.json is absent; set environment auth or run codex login before launching",
				)
			}
			return interactiveCodexAuthProjection{}, interactiveCodexAuthError(
				"inspect host auth.json: " + err.Error(),
			)
		}
		return interactiveCodexAuthProjection{
			kind:         interactiveCodexAuthFile,
			storeMode:    "file",
			hostAuthFile: hostAuthFile,
		}, nil
	default:
		return interactiveCodexAuthProjection{}, interactiveCodexAuthError(
			fmt.Sprintf("host cli_auth_credentials_store=%q is unsupported", storeMode),
		)
	}
}

func resolveEnvironmentCodexAuth(specEnv map[string]string) (key, value string, ok bool) {
	for _, key := range codexEnvironmentAuthKeys {
		if value, ok := specEnv[key]; ok {
			if strings.TrimSpace(value) != "" {
				return key, value, true
			}
			continue
		}
		if value := os.Getenv(key); strings.TrimSpace(value) != "" {
			return key, value, true
		}
	}
	return "", "", false
}

func seedInteractiveCodexEnvironmentAuth(
	ctx context.Context,
	binary string,
	ownedHome string,
	projection interactiveCodexAuthProjection,
) error {
	if projection.kind != interactiveCodexAuthEnvironment {
		return nil
	}
	flag := "--with-api-key"
	if projection.envKey == "CODEX_ACCESS_TOKEN" {
		flag = "--with-access-token"
	}
	cmd := exec.CommandContext(ctx, binary, "login", flag)
	cmd.Dir = ownedHome
	cmd.Env = mergeEnv(nil, map[string]string{projection.envKey: projection.envValue}, ownedHome)
	cmd.Stdin = strings.NewReader(projection.envValue)
	cmd.Stdout = io.Discard
	cmd.Stderr = io.Discard
	if err := cmd.Run(); err != nil {
		return interactiveCodexAuthError("Codex rejected the environment credential while seeding the private session store")
	}
	return nil
}

func readHostCodexAuthStoreMode(hostHome string) (mode string, explicit bool, err error) {
	root, err := os.OpenRoot(hostHome)
	if err != nil {
		return "", false, fmt.Errorf("open host Codex home: %w", err)
	}
	defer func() { _ = root.Close() }()
	file, err := root.Open("config.toml")
	if errors.Is(err, os.ErrNotExist) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("read host Codex credential-store selection: %w", err)
	}
	defer func() { _ = file.Close() }()
	body, err := io.ReadAll(file)
	if err != nil {
		return "", false, fmt.Errorf("read host Codex credential-store selection: %w", err)
	}
	var config hostCodexAuthConfig
	if _, err := toml.Decode(string(body), &config); err != nil {
		return "", false, fmt.Errorf("parse host Codex credential-store selection: %w", err)
	}
	mode = strings.ToLower(strings.TrimSpace(config.StoreMode))
	if mode == "" {
		return "", false, nil
	}
	switch mode {
	case "file", "keyring", "auto":
		return mode, true, nil
	default:
		return "", true, fmt.Errorf("host cli_auth_credentials_store=%q is not file, keyring, or auto", mode)
	}
}
