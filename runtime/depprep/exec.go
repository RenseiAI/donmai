package depprep

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// command is a parsed kit command line.
type command struct {
	// assign are leading NAME=value words, applied over the constructed
	// environment for this process only.
	assign map[string]string
	// program is the bare program name, resolved on the constructed PATH.
	program string
	args    []string
}

// shellChars are refused anywhere in a kit command: commands run as
// argument vectors, so quoting, expansion, redirection and globbing would
// otherwise be passed literally or, worse, read as intended.
const shellChars = "'\"`$;&|<>(){}[]*?~!#\\\n\r\t\x00"

// parseCommand splits a kit command line into assignments, program and
// arguments. The program must be a bare name: a path would resolve against
// the leaf, which is repository content.
func parseCommand(line string) (command, error) {
	if strings.ContainsAny(line, shellChars) {
		return command{}, errors.New("command uses shell syntax; commands run without a shell")
	}
	words := strings.Fields(line)
	c := command{assign: map[string]string{}}
	i := 0
	for ; i < len(words); i++ {
		eq := strings.IndexByte(words[i], '=')
		if eq <= 0 || !validEnvName(words[i][:eq]) {
			break
		}
		name := words[i][:eq]
		if forbiddenEnv(name) {
			return command{}, fmt.Errorf("command may not assign %s", name)
		}
		c.assign[name] = words[i][eq+1:]
	}
	if i == len(words) {
		return command{}, errors.New("command names no program")
	}
	c.program = words[i]
	if strings.ContainsRune(c.program, '/') || strings.HasPrefix(c.program, "-") {
		return command{}, errors.New("command program must be a bare name resolved on the toolchain PATH")
	}
	c.args = words[i+1:]
	return c, nil
}

// lookProgram resolves a bare program name on the constructed PATH. Only
// regular, executable files count.
func lookProgram(name string, pathDirs []string) (string, bool) {
	for _, dir := range pathDirs {
		candidate := filepath.Join(dir, name)
		info, err := os.Stat(candidate)
		if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0o111 == 0 {
			continue
		}
		return candidate, true
	}
	return "", false
}

// proc is one child process.
type proc struct {
	argv []string
	env  []string
	dir  string
	// outputLimit bounds the kept output tail; zero is outputTailLimit.
	outputLimit int
}

// procResult is a finished child.
type procResult struct {
	exit int
	// output is a bounded tail of stdout and stderr together.
	output []byte
	// truncated reports that output lost its head.
	truncated bool
}

// procRunner runs children. Production is osRunner; tests count or fake.
type procRunner interface {
	run(ctx context.Context, p proc) (procResult, error)
}

const (
	outputTailLimit = 4 * 1024
	// killGrace bounds the wait for a descendant that escaped the killed
	// process group while holding an output pipe.
	killGrace = time.Second
)

// tailBuffer keeps the last limit bytes written, and whether any were
// dropped. stdout and stderr share one, so os/exec serializes the writes.
type tailBuffer struct {
	data      []byte
	limit     int
	truncated bool
}

func (b *tailBuffer) Write(p []byte) (int, error) {
	n := len(p)
	if len(p) >= b.limit {
		b.truncated = b.truncated || len(p) > b.limit || len(b.data) > 0
		b.data = append(b.data[:0], p[len(p)-b.limit:]...)
		return n, nil
	}
	b.data = append(b.data, p...)
	if over := len(b.data) - b.limit; over > 0 {
		b.truncated = true
		b.data = append(b.data[:0], b.data[over:]...)
	}
	return n, nil
}

type osRunner struct{}

func (osRunner) run(ctx context.Context, p proc) (procResult, error) {
	if len(p.argv) == 0 {
		return procResult{exit: -1}, errors.New("empty argv")
	}
	cmd := exec.CommandContext(ctx, p.argv[0], p.argv[1:]...) //nolint:gosec // argv is a parsed kit command resolved on the constructed toolchain PATH
	cmd.Dir = p.dir
	// The environment is exactly the constructed one. A non-nil empty
	// slice still means "nothing inherited".
	cmd.Env = append([]string{}, p.env...)
	cmd.Stdin = nil
	isolateProcessGroup(cmd)
	cmd.WaitDelay = killGrace
	out := tailBuffer{limit: p.outputLimit}
	if out.limit <= 0 {
		out.limit = outputTailLimit
	}
	cmd.Stdout, cmd.Stderr = &out, &out
	err := cmd.Run()
	res := procResult{exit: -1, output: out.data, truncated: out.truncated}
	if err == nil {
		res.exit = 0
		return res, nil
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) && ctx.Err() == nil {
		res.exit = exitErr.ExitCode()
		return res, nil
	}
	if ctx.Err() != nil {
		return res, ctx.Err()
	}
	return res, err
}
