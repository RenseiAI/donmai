package vcs

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"strings"
)

// commandRunner runs a single CLI command in a working directory and returns its
// trimmed stdout. It is the seam that lets provider tests substitute git / gh /
// atomic without spawning processes or hitting a live API — the Go equivalent of
// the legacy TS suite mocking child_process.exec.
//
// extraEnv entries (each "KEY=VALUE") are appended to the inherited environment.
// This mirrors landing.commandRunner; it is duplicated rather than shared because
// the vcs package is a leaf the landing package imports, not the reverse, and a
// shared seam would force an import cycle.
type commandRunner interface {
	run(ctx context.Context, dir string, extraEnv []string, name string, args ...string) (string, error)
}

// execRunner is the production commandRunner backed by os/exec.
type execRunner struct{}

func (execRunner) run(ctx context.Context, dir string, extraEnv []string, name string, args ...string) (string, error) {
	// #nosec G204 -- this is the VCS provider's git/gh/atomic runner; the command
	// name and args are constructed from internal logic and provider options, not
	// untrusted input. The seam exists so tests inject a fake; production callers
	// pass fixed verbs (git, gh, atomic).
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = dir
	if len(extraEnv) > 0 {
		cmd.Env = append(cmd.Environ(), extraEnv...)
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		// Combine stderr + stdout into the error message so the push/pull error
		// classifiers can pattern-match the CLI's diagnostic text (git writes
		// "[rejected]", "non-fast-forward", "CONFLICT", etc. to stderr).
		diag := strings.TrimSpace(stderr.String())
		if diag == "" {
			diag = strings.TrimSpace(stdout.String())
		}
		if diag != "" {
			return strings.TrimSpace(stdout.String()), fmt.Errorf("%s %s: %s: %w", name, strings.Join(args, " "), diag, err)
		}
		return strings.TrimSpace(stdout.String()), fmt.Errorf("%s %s: %w", name, strings.Join(args, " "), err)
	}
	return strings.TrimSpace(stdout.String()), nil
}

// defaultRunner is the package-wide production runner. Constructors default to
// it; tests assign a fake.
var defaultRunner commandRunner = execRunner{}

// splitLines splits trimmed stdout into non-empty lines, mirroring the legacy
// `stdout.trim().split('\n').filter(Boolean)`.
func splitLines(s string) []string {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil
	}
	parts := strings.Split(s, "\n")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}
