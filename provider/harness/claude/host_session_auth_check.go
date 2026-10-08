package claude

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"github.com/RenseiAI/donmai/agent"
)

const (
	hostLoginTimeout   = 5 * time.Second
	hostLoginOutputMax = 16 << 10
)

type boundedHostLoginOutput struct{ buffer bytes.Buffer }

func (b *boundedHostLoginOutput) Write(data []byte) (int, error) {
	if len(data) > hostLoginOutputMax-b.buffer.Len() {
		return 0, errors.New("claude host login status exceeded its output bound")
	}
	return b.buffer.Write(data)
}

// CheckHostSessionLogin observes the selected native CLI's first-party host
// login. It carries only the operator's intentional home/config locators to
// the status child; unrelated process credentials are never inherited.
// This is local login evidence, not online model entitlement.
func CheckHostSessionLogin(ctx context.Context, binary string) error {
	if ctx == nil || !filepath.IsAbs(binary) {
		return fmt.Errorf("%w: Claude host login needs an absolute CLI path and context", agent.ErrProviderUnavailable)
	}
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("%w: Claude host login: %w", agent.ErrProviderUnavailable, err)
	}
	home, err := os.UserHomeDir()
	if err != nil || !filepath.IsAbs(home) {
		return fmt.Errorf("%w: Claude host login home is unavailable", agent.ErrProviderUnavailable)
	}
	env := []string{"HOME=" + home, "PATH=" + os.Getenv("PATH"), "NO_COLOR=1"}
	// The login status child reads the keychain-backed login on macOS,
	// which needs USER to locate the operator's login item. Without it
	// a signed-in host reports logged-out, and a quota probe built on
	// this check would record ok:false every interval. USER carries no
	// credential material, only the account name the login already names.
	if user, present := os.LookupEnv("USER"); present && user != "" {
		env = append(env, "USER="+user)
	}
	for _, name := range []string{"CLAUDE_CONFIG_DIR", "XDG_CONFIG_HOME"} {
		if value, present := os.LookupEnv(name); present && value != "" {
			if !filepath.IsAbs(value) {
				value, err = filepath.Abs(value)
				if err != nil {
					return fmt.Errorf("%w: resolve Claude host login config locator: %w", agent.ErrProviderUnavailable, err)
				}
			}
			env = append(env, name+"="+value)
		}
	}
	probeDir, err := os.MkdirTemp("", "donmai-claude-login-")
	if err != nil {
		return fmt.Errorf("%w: create private Claude login probe: %w", agent.ErrProviderUnavailable, err)
	}
	quiescent := true
	defer func() {
		if quiescent {
			_ = os.RemoveAll(probeDir)
		}
	}()
	probeCtx, cancel := context.WithTimeout(ctx, hostLoginTimeout)
	defer cancel()
	command := exec.CommandContext(probeCtx, binary, "auth", "status", "--json")
	command.Dir = probeDir
	env = append(env, "TMPDIR="+probeDir, "XDG_CACHE_HOME="+probeDir)
	command.Env = env
	command.WaitDelay = 200 * time.Millisecond
	var stdout, stderr boundedHostLoginOutput
	command.Stdout, command.Stderr = &stdout, &stderr
	quiescent, err = runOwnedModelVersionProbe(probeCtx, command)
	if !quiescent {
		return fmt.Errorf("%w: Claude host login could not prove child quiescence: %v", agent.ErrProviderUnavailable, err)
	}
	if err != nil || stderr.buffer.Len() != 0 {
		if probeCtx.Err() != nil {
			return fmt.Errorf("%w: Claude host login: %w", agent.ErrProviderUnavailable, probeCtx.Err())
		}
		return fmt.Errorf("%w: Claude host login status unavailable", agent.ErrProviderUnavailable)
	}
	var status struct {
		LoggedIn    bool   `json:"loggedIn"`
		AuthMethod  string `json:"authMethod"`
		APIProvider string `json:"apiProvider"`
	}
	if err := json.Unmarshal(stdout.buffer.Bytes(), &status); err != nil || !status.LoggedIn || status.AuthMethod != "claude.ai" || status.APIProvider != "firstParty" {
		return fmt.Errorf("%w: Claude runtime requires an authenticated first-party host login", agent.ErrProviderUnavailable)
	}
	return nil
}
