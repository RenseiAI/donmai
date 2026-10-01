package codex

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

const (
	hostLoginStatus          = "Logged in using ChatGPT\n"
	maxHostAuthMetadataBytes = 1 << 20
)

var errHostSessionLogin = errors.New("codex host-session ChatGPT login is unavailable")

type hostStatusOwner interface {
	wait() error
	stop(time.Duration) error
}

type hostStatusOwnerFactory func(*exec.Cmd) hostStatusOwner

// statusCapture drains all child output while retaining only a small prefix.
// Overflow is a refusal, never a reason to inspect or report diagnostics.
// A named buffer prevents io.Copy from bypassing Write via promoted ReadFrom.
type statusCapture struct {
	buffer   bytes.Buffer
	overflow bool
}

func (c *statusCapture) Len() int      { return c.buffer.Len() }
func (c *statusCapture) Bytes() []byte { return c.buffer.Bytes() }

// validProjectedChatGPTAuth performs a bounded structural read of the same
// private hard link the native status command receives. Native v0.157.1 treats
// an empty auth.json object as ChatGPT mode, so native status alone cannot
// establish that login material exists. Values stay transient in this call
// and are never logged, returned, persisted, or used as auth by Donmai.
func validProjectedChatGPTAuth(boundary *codexConfigBoundary) bool {
	root, err := os.OpenRoot(boundary.home)
	if err != nil {
		return false
	}
	defer func() { _ = root.Close() }()
	file, err := root.Open(codexAuthFileName)
	if err != nil {
		return false
	}
	defer func() { _ = file.Close() }()
	body, err := io.ReadAll(io.LimitReader(file, maxHostAuthMetadataBytes+1))
	if err != nil || len(body) == 0 || len(body) > maxHostAuthMetadataBytes {
		return false
	}
	var auth struct {
		Mode                *string         `json:"auth_mode"`
		APIKey              *string         `json:"OPENAI_API_KEY"`
		AgentIdentity       json.RawMessage `json:"agent_identity"`
		PersonalAccessToken json.RawMessage `json:"personal_access_token"`
		BedrockAPIKey       json.RawMessage `json:"bedrock_api_key"`
		BedrockAccessKeys   json.RawMessage `json:"bedrock_access_keys"`
		Tokens              *struct {
			IDToken      string `json:"id_token"`
			AccessToken  string `json:"access_token"`
			RefreshToken string `json:"refresh_token"`
		} `json:"tokens"`
	}
	if json.Unmarshal(body, &auth) != nil || auth.Tokens == nil {
		return false
	}
	if auth.Mode != nil && *auth.Mode != "chatgpt" {
		return false
	}
	if auth.APIKey != nil || len(auth.AgentIdentity) > 0 && string(auth.AgentIdentity) != "null" || len(auth.PersonalAccessToken) > 0 && string(auth.PersonalAccessToken) != "null" || len(auth.BedrockAPIKey) > 0 && string(auth.BedrockAPIKey) != "null" || len(auth.BedrockAccessKeys) > 0 && string(auth.BedrockAccessKeys) != "null" {
		return false
	}
	return strings.TrimSpace(auth.Tokens.IDToken) != "" && strings.TrimSpace(auth.Tokens.AccessToken) != "" && strings.TrimSpace(auth.Tokens.RefreshToken) != ""
}

func (c *statusCapture) Write(p []byte) (int, error) {
	const limit = 4096
	if c.Len()+len(p) > limit {
		c.overflow = true
	}
	if remaining := limit - c.Len(); remaining > 0 {
		_, _ = c.buffer.Write(p[:min(len(p), remaining)])
	}
	return len(p), nil
}

// CheckHostSessionLogin verifies the file-backed ChatGPT login that a
// HostSessionAuth Spawn will project. It neither starts app-server nor performs
// a model turn. The native v0.157.1 `login status` path uses load_auth(false)
// and FileAuthStorage.load; it does not refresh or write the credential. The
// exact status output is an observed local login, not online entitlement.
// No credential bytes or native diagnostics are returned to callers.
func CheckHostSessionLogin(ctx context.Context, binary string) error {
	return checkHostSessionLogin(ctx, binary, func(command *exec.Cmd) hostStatusOwner { return newOwnedProcess(command) })
}

func checkHostSessionLogin(ctx context.Context, binary string, ownerFactory hostStatusOwnerFactory) (result error) {
	if !filepath.IsAbs(binary) {
		return errHostSessionLogin
	}
	if ctx == nil || ownerFactory == nil {
		return errHostSessionLogin
	}
	if ctx.Err() != nil {
		return errHostSessionLogin
	}
	hostAuthFile, err := resolveHostSessionAuthFile()
	if err != nil {
		return errHostSessionLogin
	}
	boundary, err := newCodexConfigBoundary("", true)
	if err != nil {
		return errHostSessionLogin
	}
	retainBoundary := false
	defer func() {
		if retainBoundary {
			return
		}
		if err := boundary.remove(); err != nil {
			result = errHostSessionLogin
		}
	}()
	if err := boundary.linkHostSessionAuth(hostAuthFile); err != nil {
		return errHostSessionLogin
	}
	if err := boundary.validate(); err != nil {
		return errHostSessionLogin
	}
	if !validProjectedChatGPTAuth(boundary) {
		return errHostSessionLogin
	}
	before, err := os.Lstat(hostAuthFile)
	if err != nil {
		return errHostSessionLogin
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	command := exec.Command(binary, "-c", `cli_auth_credentials_store="file"`, "login", "status")
	// A wrapper may detach a child that keeps stdout/stderr open after the
	// direct process exits. WaitDelay bounds Go's pipe-drain wait; an unproved
	// descendant makes the check fail without signalling an unowned PID.
	command.WaitDelay = 200 * time.Millisecond
	command.Dir = boundary.home
	command.Env = []string{"CODEX_HOME=" + boundary.home, "HOME=" + boundary.home, "PATH=/usr/bin:/bin", "NO_COLOR=1"}
	command.Stdin = nil
	var stdout, stderr statusCapture
	command.Stdout, command.Stderr = &stdout, &stderr
	configureOwnedProcessGroup(command)
	owned := ownerFactory(command)
	if owned == nil {
		return errHostSessionLogin
	}
	if err := command.Start(); err != nil {
		return errHostSessionLogin
	}
	done := make(chan error, 1)
	go func() { done <- owned.wait() }()
	var waitErr error
	select {
	case waitErr = <-done:
	case <-ctx.Done():
		if err := owned.stop(0); err != nil {
			retainBoundary = true
			return errHostSessionLogin
		}
		waitErr = <-done
		if errors.Is(waitErr, exec.ErrWaitDelay) {
			retainBoundary = true
		}
		return errHostSessionLogin
	}
	if err := owned.stop(0); err != nil {
		retainBoundary = true
		return errHostSessionLogin
	}
	if errors.Is(waitErr, exec.ErrWaitDelay) {
		retainBoundary = true
		return errHostSessionLogin
	}
	if ctx.Err() != nil || waitErr != nil || stdout.overflow || stderr.overflow || stdout.Len() != 0 || !bytes.Equal(stderr.Bytes(), []byte(hostLoginStatus)) {
		return errHostSessionLogin
	}
	if err := boundary.validate(); err != nil {
		return errHostSessionLogin
	}
	after, err := os.Lstat(hostAuthFile)
	if err != nil || !os.SameFile(before, after) || before.Size() != after.Size() || !before.ModTime().Equal(after.ModTime()) {
		return errHostSessionLogin
	}
	return nil
}

var _ io.Writer = (*statusCapture)(nil)
