package muse

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/RenseiAI/donmai/agent"
)

// hostSessionLoginTimeout bounds the `<binary> --version` probe the
// host login check runs. A package var, not a const, so tests can
// shorten the bound for hanging-CLI cases; production always runs
// the 5 s default.
var hostSessionLoginTimeout = 5 * time.Second

// hostSessionLoginOutputMax bounds the bytes retained from the
// version probe's stdout and stderr. The version line is one short
// line; anything past this is a runaway child, not a version read.
const hostSessionLoginOutputMax = 16 << 10

// boundedHostLoginOutput retains at most hostSessionLoginOutputMax
// bytes of probe output. Overflow is swallowed, never reported: a
// bound failure must not become a verdict.
type boundedHostLoginOutput struct{ buffer bytes.Buffer }

func (b *boundedHostLoginOutput) Write(data []byte) (int, error) {
	n := len(data)
	remaining := hostSessionLoginOutputMax - b.buffer.Len()
	if remaining > 0 {
		if len(data) > remaining {
			data = data[:remaining]
		}
		b.buffer.Write(data)
	}
	return n, nil
}

// CheckHostSessionLogin proves a supported host CLI is installed: one
// bounded `<binary> --version` run, parsed with the same parser and
// compared against the same MinVersion the harness constructor
// enforces, so the check and a Spawn through New never disagree. No
// network, no serve child, no model turn, no credential file reads.
//
// The CLI exposes no login-status command, so this is install
// evidence, not online login entitlement: a host without a valid
// login is classified at the first turn instead. binary must be an
// absolute path (callers resolve it through ResolveMuseBinary).
func CheckHostSessionLogin(ctx context.Context, binary string) error {
	if ctx == nil || !filepath.IsAbs(binary) {
		return fmt.Errorf("%w: host login check needs an absolute CLI path and context", agent.ErrProviderUnavailable)
	}
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("%w: host login check: %w", agent.ErrProviderUnavailable, err)
	}
	probeCtx, cancel := context.WithTimeout(ctx, hostSessionLoginTimeout)
	defer cancel()
	// nolint:gosec // binary is the resolved CLI path the caller probed at construction.
	command := exec.CommandContext(probeCtx, binary, "--version")
	// A wrapper may detach a child that keeps stdout/stderr open after
	// the direct process exits. WaitDelay bounds the pipe-drain wait;
	// an unproved descendant makes the check fail without signalling
	// an unowned PID.
	command.WaitDelay = 200 * time.Millisecond
	command.Env = []string{"PATH=/usr/bin:/bin", "NO_COLOR=1", "MUSE_NO_AUTO_UPDATE=1"}
	command.Stdin = nil
	var stdout, stderr boundedHostLoginOutput
	command.Stdout, command.Stderr = &stdout, &stderr
	configureProcessGroup(command)
	if err := command.Start(); err != nil {
		return fmt.Errorf("%w: host login check: %v", agent.ErrProviderUnavailable, err)
	}
	done := make(chan error, 1)
	go func() { done <- command.Wait() }()
	var waitErr error
	select {
	case waitErr = <-done:
	case <-probeCtx.Done():
		// The probe owns this child in its own process group: kill
		// the group, never just the leader, so a forked helper cannot
		// outlive the timeout. The failure below reports the probe as
		// failed either way.
		signalProcessGroup(command, sigKILL())
		<-done
		if ctxErr := ctx.Err(); ctxErr != nil {
			return fmt.Errorf("%w: host login check: %w", agent.ErrProviderUnavailable, ctxErr)
		}
		return fmt.Errorf("%w: host login check timed out", agent.ErrProviderUnavailable)
	}
	if waitErr != nil {
		if errors.Is(waitErr, exec.ErrWaitDelay) {
			signalProcessGroup(command, sigKILL())
		}
		return fmt.Errorf("%w: host login check: %v", agent.ErrProviderUnavailable, waitErr)
	}
	if stderr.buffer.Len() != 0 {
		return fmt.Errorf("%w: host login check printed unexpected diagnostics", agent.ErrProviderUnavailable)
	}
	ver := parseMuseVersion(strings.TrimSpace(stdout.buffer.String()))
	if ver == "" {
		return fmt.Errorf("%w: host login check could not parse a version", agent.ErrProviderUnavailable)
	}
	if compareVersions(ver, MinVersion) < 0 {
		return fmt.Errorf("%w: host CLI version %s is below the minimum %s", agent.ErrProviderUnavailable, ver, MinVersion)
	}
	return nil
}
