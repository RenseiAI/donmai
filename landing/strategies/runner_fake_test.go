package strategies

import (
	"context"
	"errors"
	"strings"
	"sync"
)

// fakeRunner records every command and replies from a programmable router. Safe
// for concurrent use so strategy tests pass under -race.
type fakeRunner struct {
	mu    sync.Mutex
	calls []fakeCall
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

func (c fakeCall) commandLine() string {
	if len(c.args) == 0 {
		return c.name
	}
	return c.name + " " + strings.Join(c.args, " ")
}

func (f *fakeRunner) commandLines() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, len(f.calls))
	for i, c := range f.calls {
		out[i] = c.commandLine()
	}
	return out
}

func (f *fakeRunner) dirs() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, len(f.calls))
	for i, c := range f.calls {
		out[i] = c.dir
	}
	return out
}

// seqReply replies to git-subcommand calls in sequence, keyed by a substring
// match on the rendered command line. Each entry pops once; unmatched calls
// return ("", nil). An entry with errMsg set returns that error.
type seqEntry struct {
	match  string
	stdout string
	errMsg string
}

func seqReply(entries []seqEntry) func(string, []string) (string, error) {
	remaining := append([]seqEntry(nil), entries...)
	var mu sync.Mutex
	return func(name string, args []string) (string, error) {
		line := name
		if len(args) > 0 {
			line = name + " " + strings.Join(args, " ")
		}
		mu.Lock()
		defer mu.Unlock()
		for i, e := range remaining {
			if strings.Contains(line, e.match) {
				remaining = append(remaining[:i], remaining[i+1:]...)
				if e.errMsg != "" {
					return "", errors.New(e.errMsg)
				}
				return e.stdout, nil
			}
		}
		return "", nil
	}
}
