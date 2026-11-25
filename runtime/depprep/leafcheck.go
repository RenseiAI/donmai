package depprep

import (
	"bytes"
	"context"
	"path"
	"sort"
	"strings"
)

// leafStatusLimit bounds the status listing. Directories are collapsed, so a
// clean checkout plus its installed trees stays far below it.
const leafStatusLimit = 1 << 20

// leafStatus is the leaf's untracked ("??") and ignored ("!!") entries, by
// path, as git reports them with directories collapsed.
type leafStatus map[string]string

// gitStatus lists the leaf's untracked and ignored entries. It runs git with
// the constructed environment, no system or user configuration, no
// filesystem monitor and no optional locks, so it neither executes
// configured programs nor writes the index. ok is false when git is not on
// the constructed PATH or the leaf is not a work tree.
func gitStatus(ctx context.Context, r procRunner, root string, env envSet, dirs []string) (leafStatus, bool) {
	git, found := lookProgram("git", dirs)
	if !found {
		return nil, false
	}
	res, err := r.run(ctx, proc{
		argv: []string{
			git, "-c", "core.fsmonitor=false", "--no-optional-locks", "status",
			"--porcelain=v1", "-z", "--untracked-files=normal", "--ignored=matching",
		},
		env:         env.with(map[string]string{"GIT_CONFIG_NOSYSTEM": "1", "GIT_CONFIG_GLOBAL": "/dev/null"}).list(),
		dir:         root,
		outputLimit: leafStatusLimit,
	})
	if err != nil || res.exit != 0 || res.truncated {
		// A truncated listing cannot be trusted either way.
		return nil, false
	}
	status := leafStatus{}
	for _, rec := range bytes.Split(res.output, []byte{0}) {
		if len(rec) < 4 {
			continue
		}
		code := string(rec[:2])
		if code != "??" && code != "!!" {
			continue
		}
		status[strings.TrimSuffix(string(rec[3:]), "/")] = code
	}
	return status, true
}

// undeclaredPaths names every entry the install created that is either not
// ignored by git or outside every selected entry's snapshot.installed.
func undeclaredPaths(before, after leafStatus, installed []string) []string {
	var out []string
	for p, code := range after {
		if _, existed := before[p]; existed {
			continue
		}
		if code == "!!" && matchesAny(installed, p) {
			continue
		}
		out = append(out, p)
	}
	sort.Strings(out)
	return out
}

func matchesAny(patterns []string, p string) bool {
	for _, pattern := range patterns {
		if matchSegments(strings.Split(pattern, "/"), strings.Split(p, "/")) {
			return true
		}
	}
	return false
}

// matchSegments matches path segments against pattern segments, where "**"
// matches zero or more segments and every other segment is a path.Match
// pattern.
func matchSegments(pattern, segs []string) bool {
	if len(pattern) == 0 {
		return len(segs) == 0
	}
	if pattern[0] == "**" {
		for i := 0; i <= len(segs); i++ {
			if matchSegments(pattern[1:], segs[i:]) {
				return true
			}
		}
		return false
	}
	if len(segs) == 0 {
		return false
	}
	ok, err := path.Match(pattern[0], segs[0])
	return err == nil && ok && matchSegments(pattern[1:], segs[1:])
}
