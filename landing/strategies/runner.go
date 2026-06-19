package strategies

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"strings"

	"github.com/RenseiAI/donmai/internal/gitexec"
)

// commandRunner runs a single command in a working directory and returns its
// trimmed stdout. It is the seam that lets strategy tests substitute git/grep
// without a real repository — the Go equivalent of the legacy TS suite mocking
// child_process.exec.
//
// extraEnv entries (each "KEY=VALUE") are appended to the inherited environment.
type commandRunner interface {
	run(ctx context.Context, dir string, extraEnv []string, name string, args ...string) (string, error)
}

// execRunner is the production commandRunner backed by os/exec.
type execRunner struct{}

func (execRunner) run(ctx context.Context, dir string, extraEnv []string, name string, args ...string) (string, error) {
	// #nosec G204 -- this is the landing serializer's git/package-manager runner;
	// the command name and args are constructed from internal strategy logic and
	// repository config, not untrusted input. The seam exists so tests inject a
	// fake; production callers pass fixed verbs (git, pnpm, …).
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = dir
	extraEnv = gitNonInteractiveEnv(name, extraEnv)
	if len(extraEnv) > 0 {
		// nil Env means inherit the parent's environment; we must materialize
		// it before appending so the extra entries do not replace it wholesale.
		cmd.Env = append(cmd.Environ(), extraEnv...)
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		// Surface stderr in the error so the branch-conflict classifiers can
		// match on git's messages, mirroring the legacy "Command failed: …"
		// error text that carried stderr.
		msg := strings.TrimSpace(stderr.String())
		if msg != "" {
			return strings.TrimSpace(stdout.String()), fmt.Errorf("%s %s: %s: %w", name, strings.Join(args, " "), msg, err)
		}
		return strings.TrimSpace(stdout.String()), fmt.Errorf("%s %s: %w", name, strings.Join(args, " "), err)
	}
	return strings.TrimSpace(stdout.String()), nil
}

// gitNonInteractiveEnv hardens the environment for git so a headless serializer
// can never block on an interactive credential or passphrase prompt. When name
// is "git" it delegates to gitexec.HardenedEnv with no helper-suppression and no
// auth header, which appends GIT_TERMINAL_PROMPT=0 (git fails fast instead of
// waiting on a controlling terminal) and GCM_INTERACTIVE=never
// (git-credential-manager, if installed, skips its interactive device-code
// flow). Auth must come from the remote URL token or a non-interactive helper.
// Without this a push/fetch that awaits credentials hangs the land step forever
// — the entry is dequeued but no terminal marker is ever written. Non-git
// commands are returned unchanged.
//
// The keychain-suppression / per-invocation auth-header behaviour of
// gitexec.HardenedEnv is not engaged here: callers that need it pass the
// already-hardened slice through extraEnv (HardenedEnv composes — its
// GIT_CONFIG_COUNT continuation preserves any pairs already present).
func gitNonInteractiveEnv(name string, extraEnv []string) []string {
	if name != "git" {
		return extraEnv
	}
	return gitexec.HardenedEnv(extraEnv, false, "")
}

// defaultRunner is the package-wide production runner. Strategy constructors
// default to it; tests assign a fake.
var defaultRunner commandRunner = execRunner{}
