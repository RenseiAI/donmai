package landing

import (
	"context"
	"errors"
	"strings"
	"sync"
)

// fakeRunner records every command and replies from a programmable router. It is
// the Go analogue of the legacy TS suite mocking child_process.exec, and is safe
// for concurrent use so tests pass under -race.
type fakeRunner struct {
	mu sync.Mutex
	// calls records every invocation in order.
	calls []fakeCall
	// reply returns (stdout, err) for a given command; nil ⇒ default success.
	reply func(name string, args []string) (string, error)
}

type fakeCall struct {
	dir      string
	extraEnv []string
	name     string
	args     []string
}

func (f *fakeRunner) run(ctx context.Context, dir string, extraEnv []string, name string, args ...string) (string, error) {
	_ = ctx
	f.mu.Lock()
	f.calls = append(f.calls, fakeCall{dir: dir, extraEnv: append([]string(nil), extraEnv...), name: name, args: append([]string(nil), args...)})
	reply := f.reply
	f.mu.Unlock()
	if reply == nil {
		return "", nil
	}
	return reply(name, args)
}

// commandLine renders a recorded call as "name arg1 arg2 …" for substring
// assertions that mirror the legacy `cmd.includes(...)` checks.
func (c fakeCall) commandLine() string {
	if len(c.args) == 0 {
		return c.name
	}
	return c.name + " " + strings.Join(c.args, " ")
}

// commandLines returns every recorded call as a command line.
func (f *fakeRunner) commandLines() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, len(f.calls))
	for i, c := range f.calls {
		out[i] = c.commandLine()
	}
	return out
}

// errReply is a convenience to build a reply func that errors on commands whose
// rendered command line contains match, and succeeds (returning okStdout)
// otherwise.
func errReply(match, errMsg, okStdout string) func(string, []string) (string, error) {
	return func(name string, args []string) (string, error) {
		line := name
		if len(args) > 0 {
			line = name + " " + strings.Join(args, " ")
		}
		if strings.Contains(line, match) {
			return "", errors.New(errMsg)
		}
		return okStdout, nil
	}
}
