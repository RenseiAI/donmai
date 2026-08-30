// Package harnessstate is the single source of truth for the directories a
// session materializes INSIDE the checkout it works in.
//
// These directories are not project content and they are not session output.
// They are live machinery: the runner's own state and event journal, a
// harness's session storage, a CLI's project-local configuration. Two rules
// follow from that, and both were being applied from separate, drifting lists
// before this package existed:
//
//  1. They must never enter a commit. The backstop unstages them before it
//     auto-commits, or a PR carries a session's internals.
//  2. They must never show up as untracked. A directory that reports as
//     `?? .pi/` in `git status` reads as junk the run dropped — and on
//     2026-08-29 a session tidied one away while it was still being written
//     to, stranding itself with no error. Excluding them at provision removes
//     the motive.
//
// One list, two consumers, so the two can never disagree about what is
// harness state. The list is a static table rather than an init-time
// registration seam on purpose: a consumer that reads it during its own
// package initialization (the backstop does) would otherwise depend on
// package init ORDER, which is exactly the kind of silent,
// works-until-it-doesn't coupling this package exists to remove.
package harnessstate

import "sort"

// RunnerStateDir is the runner's own state directory, named here rather than
// in runtime/state so the two can never disagree. runtime/state.AgentDirName
// is defined as this constant.
const RunnerStateDir = ".agent"

// PiStateDir is the pi harness's session state directory.
const PiStateDir = ".pi"

// Dir describes one checkout-resident state directory and who writes it. The
// Owner/Why fields are documentation that stays attached to the data: the
// next person to touch this table needs to know whether donmai writes the
// directory or a harness's CLI does, because that decides whether removing an
// entry is safe.
type Dir struct {
	// Name is the directory's name at the top level of the checkout.
	Name string
	// Owner names what creates it.
	Owner string
	// Why is what it holds and why losing it hurts.
	Why string
}

// dirs is the table. Add a row here — nowhere else — when a harness starts
// keeping state inside the checkout.
//
// Deliberately NOT here: `.donmai`. That is the repo's own tracked
// configuration (afclient/repoconfig reads `<gitRoot>/.donmai/config.yaml`),
// project content that a session may legitimately edit and commit. Only its
// generated code-index subdirectory is excluded from backstop commits, and
// that rule lives in the backstop's path-prefix table where it belongs.
var dirs = []Dir{
	{
		Name:  RunnerStateDir,
		Owner: "the runner (runtime/state)",
		Why:   "state.json, events.jsonl, the terminal cast and the turn manifest for the running session",
	},
	{
		Name:  PiStateDir,
		Owner: "the pi harness (provider/harness/pi)",
		Why:   "session storage, the materialized policy extension, injected extensions and the per-session agent home; handed to the child as --session-dir and PI_CODING_AGENT_DIR",
	},
	{
		Name:  ".claude",
		Owner: "the Claude Code CLI the claude harness drives",
		Why:   "project-local CLI settings the CLI writes into its working directory; donmai does not create it, which is exactly why nothing else would exclude it",
	},
	{
		Name:  ".codex",
		Owner: "the Codex CLI the codex harness drives",
		Why:   "project-local CLI state. The harness redirects CODEX_HOME to a private directory outside the checkout, so this should stay absent — it is listed so a CLI version that ignores the redirect cannot quietly land in a commit",
	},
}

// Dirs returns the state-directory names, sorted, as a fresh slice the caller
// may keep or mutate.
func Dirs() []string {
	out := make([]string, 0, len(dirs))
	for _, d := range dirs {
		out = append(out, d.Name)
	}
	sort.Strings(out)
	return out
}

// All returns the full table, sorted by name, as a fresh slice.
func All() []Dir {
	out := make([]Dir, len(dirs))
	copy(out, dirs)
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// ExcludeEntries returns one gitignore-syntax entry per state directory —
// each with a trailing slash, so the pattern matches the directory and
// everything under it and can never match a FILE that happens to share the
// name.
func ExcludeEntries() []string {
	names := Dirs()
	out := make([]string, 0, len(names))
	for _, n := range names {
		out = append(out, n+"/")
	}
	return out
}

// IsStateDir reports whether name is one of the state directories. The
// comparison is on the whole name, so a directory that merely shares a prefix
// (".pi-cache") is not one.
func IsStateDir(name string) bool {
	for _, d := range dirs {
		if d.Name == name {
			return true
		}
	}
	return false
}
