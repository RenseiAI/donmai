package landing

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"strings"
)

// commandRunner runs a single command in a working directory and returns its
// trimmed stdout. It is the seam that lets file-manifest, lock-file, and
// conflict-resolver tests substitute git/grep/package-managers without touching
// the filesystem — the Go equivalent of the legacy TS suite mocking
// child_process.exec.
//
// extraEnv entries (each "KEY=VALUE") are appended to the inherited environment.
type commandRunner interface {
	run(ctx context.Context, dir string, extraEnv []string, name string, args ...string) (string, error)
}

// execRunner is the production commandRunner backed by os/exec.
type execRunner struct{}

func (execRunner) run(ctx context.Context, dir string, extraEnv []string, name string, args ...string) (string, error) {
	// #nosec G204 -- this is the landing serializer's git/grep/package-manager
	// runner; the command name and args are constructed from internal logic and
	// repository config, not untrusted input. The seam exists so tests inject a
	// fake; production callers pass fixed verbs (git, grep, pnpm, …).
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = dir
	if len(extraEnv) > 0 {
		cmd.Env = append(cmd.Environ(), extraEnv...)
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg != "" {
			return strings.TrimSpace(stdout.String()), fmt.Errorf("%s %s: %s: %w", name, strings.Join(args, " "), msg, err)
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
