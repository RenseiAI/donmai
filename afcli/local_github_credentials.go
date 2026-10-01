package afcli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

const (
	localGitHubTokenOutputLimit = 8 << 10
	localGitHubTokenTimeout     = 5 * time.Second
)

// localGitHubSourceToken is only for the configured GitHub intake/publication
// adapter. The selected value never enters the worker's BaseEnv. The ordinary
// standalone child-env source keeps its process then git-root .env.local order.
func localGitHubSourceToken(baseEnv map[string]string) (string, error) {
	githubToken := strings.TrimSpace(baseEnv["GITHUB_TOKEN"])
	if githubToken == "" {
		githubToken = strings.TrimSpace(os.Getenv("GITHUB_TOKEN"))
	}
	ghToken := strings.TrimSpace(baseEnv["GH_TOKEN"])
	if ghToken == "" {
		ghToken = strings.TrimSpace(os.Getenv("GH_TOKEN"))
	}
	if githubToken != "" && ghToken != "" && githubToken != ghToken {
		return "", errors.New("local GitHub source has conflicting GITHUB_TOKEN and GH_TOKEN credentials")
	}
	if githubToken != "" {
		return validateLocalGitHubToken(githubToken)
	}
	if ghToken != "" {
		return validateLocalGitHubToken(ghToken)
	}
	return storedGitHubCLIToken(context.Background())
}

func validateLocalGitHubToken(token string) (string, error) {
	if token == "" || len(token) > localGitHubTokenOutputLimit || strings.TrimSpace(token) != token {
		return "", errors.New("local GitHub credential is absent or invalid")
	}
	for _, char := range token {
		if char < 0x21 || char == 0x7f {
			return "", errors.New("local GitHub credential is absent or invalid")
		}
	}
	return token, nil
}

// boundedGitHubTokenOutput never exposes captured bearer bytes through an
// error. Keeping the bytes.Buffer named prevents io.Copy from bypassing Write.
type boundedGitHubTokenOutput struct{ buffer bytes.Buffer }

func (b *boundedGitHubTokenOutput) Write(data []byte) (int, error) {
	if len(data) > localGitHubTokenOutputLimit-b.buffer.Len() {
		return 0, errors.New("local GitHub credential output exceeds its bound")
	}
	return b.buffer.Write(data)
}

func localGitHubLocator(name string) (string, error) {
	value := os.Getenv(name)
	if value == "" {
		return "", nil
	}
	if !filepath.IsAbs(value) || strings.ContainsAny(value, "\r\n\x00") {
		return "", fmt.Errorf("local GitHub CLI %s locator is invalid", name)
	}
	return filepath.Clean(value), nil
}

// gh uses a user-owned config or OS credential store. Pass only locators needed
// to reach that store; in particular, ambient bearer and preload variables
// cannot turn this stored-login fallback into a different credential source.
func localGitHubCLIEnvironment() ([]string, error) {
	home, err := localGitHubLocator("HOME")
	if err != nil || home == "" {
		return nil, errors.New("local GitHub CLI requires an absolute service HOME")
	}
	env := []string{"HOME=" + home, "PATH=/usr/bin:/bin", "GH_PROMPT_DISABLED=1", "NO_COLOR=1", "LC_ALL=C"}
	for _, name := range []string{"GH_CONFIG_DIR", "XDG_CONFIG_HOME", "XDG_RUNTIME_DIR"} {
		value, locatorErr := localGitHubLocator(name)
		if locatorErr != nil {
			return nil, locatorErr
		}
		if value != "" {
			env = append(env, name+"="+value)
		}
	}
	if bus := os.Getenv("DBUS_SESSION_BUS_ADDRESS"); bus != "" {
		if strings.ContainsAny(bus, ";\r\n\x00") ||
			(!strings.HasPrefix(bus, "unix:path=/") && !strings.HasPrefix(bus, "unix:abstract=")) {
			return nil, errors.New("local GitHub CLI session bus locator is invalid")
		}
		env = append(env, "DBUS_SESSION_BUS_ADDRESS="+bus)
	}
	return env, nil
}

func storedGitHubCLIToken(ctx context.Context) (string, error) {
	if ctx == nil || ctx.Err() != nil {
		return "", errors.New("local GitHub CLI credential lookup was cancelled")
	}
	env, err := localGitHubCLIEnvironment()
	if err != nil {
		return "", err
	}
	binary, err := exec.LookPath("gh")
	if err != nil || !filepath.IsAbs(binary) {
		return "", errors.New("local GitHub source needs GITHUB_TOKEN, GH_TOKEN, or an installed GitHub CLI login")
	}
	binary, err = filepath.EvalSymlinks(binary)
	if err != nil || !filepath.IsAbs(binary) {
		return "", errors.New("local GitHub CLI executable is unavailable")
	}
	info, err := os.Stat(binary)
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0o111 == 0 {
		return "", errors.New("local GitHub CLI executable is unavailable")
	}
	probeCtx, cancel := context.WithTimeout(ctx, localGitHubTokenTimeout)
	defer cancel()
	// Construct the fixed GitHub CLI verb, then bind the command to the
	// absolute, resolved, regular executable already checked above. The
	// second PATH lookup in CommandContext grants no execution authority.
	command := exec.CommandContext(probeCtx, "gh", "auth", "token", "--hostname", "github.com")
	command.Path = binary
	command.Args[0] = binary
	command.Dir = "/"
	command.Env = env
	command.Stdin = nil
	command.Stderr = io.Discard
	command.WaitDelay = 200 * time.Millisecond
	var output boundedGitHubTokenOutput
	defer func() { clear(output.buffer.Bytes()) }()
	command.Stdout = &output
	quiescent, runErr := runOwnedGitHubTokenProbe(probeCtx, command)
	if !quiescent {
		return "", errors.New("local GitHub CLI credential process did not prove quiescence")
	}
	if runErr != nil {
		if probeCtx.Err() != nil {
			return "", errors.New("local GitHub CLI credential lookup timed out or was cancelled")
		}
		return "", errors.New("local GitHub CLI login is unavailable")
	}
	return validateLocalGitHubToken(strings.TrimSpace(output.buffer.String()))
}
