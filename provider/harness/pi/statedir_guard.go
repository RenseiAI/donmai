package pi

import (
	"path/filepath"
	"regexp"
	"strings"
	"unicode"
)

// The harness state directory is the one part of the worktree the session
// cannot survive losing. `.pi` (piStateDir) holds this run's session storage
// (the jsonl the child appends every turn to), the materialized policy
// extension, the injected-extension artifacts, and the per-session agent
// home — all created by materializeExtension BEFORE the child starts and all
// referenced by the argv/env the child was spawned with (`--session-dir`,
// PI_CODING_AGENT_DIR). Deleting it mid-session does not fail loudly: the
// child keeps running against paths that no longer exist, so the session
// stops making progress while still looking alive.
//
// It is also unusually easy to delete by accident. It is created inside the
// checkout deliberately (so the worktree lifecycle owns its cleanup), it is
// dot-prefixed, and unless the checkout's git exclude file lists it (see
// gitexclude.go, which is the other half of this fix) it shows up as
// untracked in `git status` — where it reads as stray junk from the session's
// own build, not as live harness state.
//
// So the adjudicator treats deleting it the same way it treats `rm -rf /`: a
// built-in deny that no allow pattern can override, carrying a reason that
// says what the directory is and that the harness needs it.

// stateDirGuardReasonPrefix opens every state-dir refusal. The model receives
// the reason verbatim, so it states what the directory is rather than just
// naming a blocked pattern.
const stateDirGuardReasonPrefix = "refusing to delete the pi harness state directory"

// stateDirGuardExplanation is appended to every state-dir refusal.
const stateDirGuardExplanation = " — " + piStateDir + " holds this session's own storage " +
	"(session transcript, the loaded policy extension, and the per-session agent home). " +
	"The harness created it before the session started and reads it for the session's whole life; " +
	"removing it does not fail loudly, it silently strands the run. " +
	"It is harness state, not project output — leave it in place."

// shellSegmentSplit splits a compound bash command into the segments a shell
// would run separately. The 2026-08-29 loss arrived as a four-segment
// one-liner whose THIRD segment did the deleting, so a rule that inspected
// only the whole string's first word (or matched a pattern anchored at the
// start) would have waved it through.
//
// `&&` and `||` precede the single-character forms so alternation never
// splits them in half. A `2>&1` redirection is split at its `&`, which is
// harmless here: both halves are scanned and neither is a command name.
var shellSegmentSplit = regexp.MustCompile(`&&|\|\||;|\||&|\n`)

// envAssignmentPrefix matches a leading `KEY=value` token, which a shell
// treats as environment for the command that follows rather than as the
// command itself (`FOO=1 rm -rf .pi`).
var envAssignmentPrefix = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*=`)

// redirectionToken matches a standalone shell redirection operator
// (`>`, `>>`, `2>`, `&>`, `<`), whose following token is a redirect target,
// not an argument to the command. redirectionPrefix matches the same
// operators with the target attached (`>out.txt`, `2>&1`).
var (
	redirectionToken  = regexp.MustCompile(`^[0-9&]*(>>?|<)$`)
	redirectionPrefix = regexp.MustCompile(`^[0-9&]*(>>?|<)`)
)

// stateDirDeletionReason returns a non-empty refusal reason when command
// would delete (or move away) the session's harness state directory or
// anything inside it. cwd is the session worktree root; an empty cwd falls
// back to a purely relative comparison against piStateDir.
//
// It recognizes the deletion verbs a session actually reaches for — rm,
// rmdir, unlink, shred, mv (moving the directory away is losing it), a `find`
// whose search root is the state dir and whose action deletes, and a forced
// `git clean` that would sweep it. Everything else is left to the ordinary
// rules: this is a targeted guard, not a general filesystem policy.
func stateDirDeletionReason(command, cwd string) string {
	if strings.TrimSpace(command) == "" {
		return ""
	}
	root := stateDirRoot(cwd)
	for _, segment := range shellSegmentSplit.Split(command, -1) {
		tokens := shellTokens(segment)
		// Skip leading `KEY=value` env assignments to reach the real command.
		for len(tokens) > 0 && envAssignmentPrefix.MatchString(tokens[0]) {
			tokens = tokens[1:]
		}
		if len(tokens) == 0 {
			continue
		}
		name := filepath.Base(tokens[0])
		args := tokens[1:]

		switch name {
		case "rm", "rmdir", "unlink", "shred":
			if hit, ok := firstStateDirPath(args, cwd, root); ok {
				return stateDirRefusal(name, hit)
			}
		case "mv":
			// Only the SOURCE operands matter: moving the state directory (or
			// its session file) somewhere else loses it exactly as deleting it
			// does. The final operand is the destination.
			if paths := pathOperands(args); len(paths) > 1 {
				if hit, ok := firstStateDirPath(paths[:len(paths)-1], cwd, root); ok {
					return stateDirRefusal(name, hit)
				}
			}
		case "find":
			if !findDeletes(args) {
				continue
			}
			if hit, ok := firstStateDirPath(findSearchRoots(args), cwd, root); ok {
				return stateDirRefusal(name, hit)
			}
		case "git":
			if reason := gitCleanStateDirReason(args, cwd, root); reason != "" {
				return reason
			}
		}
	}
	return ""
}

// stateDirRefusal composes the refusal text for one offending command.
func stateDirRefusal(command, path string) string {
	return stateDirGuardReasonPrefix + " via `" + command + "` (" + path + ")" + stateDirGuardExplanation
}

// stateDirRoot is the path every candidate operand is compared against:
// <cwd>/.pi when the session root is known, the bare relative ".pi"
// otherwise.
func stateDirRoot(cwd string) string {
	if strings.TrimSpace(cwd) == "" {
		return piStateDir
	}
	return filepath.Join(filepath.Clean(cwd), piStateDir)
}

// resolvesIntoStateDir reports whether a single command operand names the
// state directory or something inside it.
//
// The comparison is on whole path components after cleaning, so a directory
// that merely SHARES A PREFIX with the state dir — `pipeline/`, `.pi-cache`,
// `pi/` — never matches. A relative operand is resolved against cwd, which is
// what makes `.pi`, `./.pi`, `.pi/`, `.pi/sessions/x.jsonl` and the absolute
// layout path all land on the same answer.
func resolvesIntoStateDir(operand, cwd, root string) bool {
	operand = strings.TrimSpace(operand)
	if operand == "" {
		return false
	}
	// A `~`-rooted operand is the invoking user's home, never this session's
	// worktree — resolving it against cwd would invent a path that is not what
	// the shell would use, so leave it to the ordinary rules.
	if operand == "~" || strings.HasPrefix(operand, "~/") {
		return false
	}
	var resolved string
	switch {
	case filepath.IsAbs(operand):
		resolved = filepath.Clean(operand)
	case cwd != "":
		resolved = filepath.Clean(filepath.Join(filepath.Clean(cwd), operand))
	default:
		resolved = filepath.Clean(operand)
	}
	return resolved == root || strings.HasPrefix(resolved, root+string(filepath.Separator))
}

// firstStateDirPath returns the first operand in args that resolves into the
// state directory.
func firstStateDirPath(args []string, cwd, root string) (string, bool) {
	for _, operand := range pathOperands(args) {
		if resolvesIntoStateDir(operand, cwd, root) {
			return operand, true
		}
	}
	return "", false
}

// pathOperands filters a command's argument list down to the tokens that can
// plausibly be paths: flags and redirections (plus a redirection's target)
// are dropped.
func pathOperands(args []string) []string {
	out := make([]string, 0, len(args))
	skipNext := false
	for _, a := range args {
		if skipNext {
			skipNext = false
			continue
		}
		switch {
		case a == "":
		case a == "--":
		case redirectionToken.MatchString(a):
			// `> out.txt` — the next token is the redirect target.
			skipNext = true
		case redirectionPrefix.MatchString(a):
			// `>out.txt` / `2>&1` — target is attached, nothing to skip.
		case strings.HasPrefix(a, "-"):
		default:
			out = append(out, a)
		}
	}
	return out
}

// findDeletes reports whether a `find` invocation carries a deleting action.
func findDeletes(args []string) bool {
	for i, a := range args {
		if a == "-delete" {
			return true
		}
		if a == "-exec" || a == "-execdir" || a == "-ok" || a == "-okdir" {
			for _, rest := range args[i+1:] {
				switch filepath.Base(rest) {
				case "rm", "rmdir", "unlink", "shred":
					return true
				}
			}
		}
	}
	return false
}

// findSearchRoots returns the leading path operands of a `find` invocation —
// everything before the first predicate (`-name`, `-delete`, …).
func findSearchRoots(args []string) []string {
	roots := make([]string, 0, len(args))
	for _, a := range args {
		if strings.HasPrefix(a, "-") {
			break
		}
		roots = append(roots, a)
	}
	return roots
}

// gitCleanStateDirReason adjudicates `git … clean …`. A clean that is not
// forced does nothing (git refuses without -f) and a dry run is harmless, so
// both are left alone. A forced clean is refused when it names a path inside
// the state directory, and ALSO when it names no path at all: a whole-tree
// sweep reaches the state dir whether or not the checkout's exclude file
// happens to list it, and that exclude write is best-effort by design.
func gitCleanStateDirReason(args []string, cwd, root string) string {
	rest, ok := gitSubcommandArgs(args, "clean")
	if !ok {
		return ""
	}
	forced := false
	for _, a := range rest {
		if a == "--force" {
			forced = true
			continue
		}
		if a == "--dry-run" {
			return ""
		}
		if strings.HasPrefix(a, "--") || !strings.HasPrefix(a, "-") {
			continue
		}
		// Short flag cluster: -fdx, -n, …
		for _, r := range a[1:] {
			switch r {
			case 'f':
				forced = true
			case 'n':
				return ""
			}
		}
	}
	if !forced {
		return ""
	}
	paths := pathOperands(rest)
	if len(paths) == 0 {
		return stateDirGuardReasonPrefix + " via an unrestricted `git clean` (it sweeps the whole worktree, " +
			piStateDir + " included)" + stateDirGuardExplanation
	}
	if hit, ok := firstStateDirPath(paths, cwd, root); ok {
		return stateDirRefusal("git clean", hit)
	}
	return ""
}

// gitSubcommandArgs finds `want` among a git invocation's arguments, skipping
// git's own leading options, and returns the arguments that follow it.
func gitSubcommandArgs(args []string, want string) ([]string, bool) {
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "-C" || a == "-c" || a == "--git-dir" || a == "--work-tree" || a == "--namespace":
			i++ // the option's value
		case strings.HasPrefix(a, "-"):
		case a == want:
			return args[i+1:], true
		default:
			// A different subcommand: nothing here for this rule.
			return nil, false
		}
	}
	return nil, false
}

// shellTokens splits one shell segment into tokens, honoring single quotes,
// double quotes and backslash escapes so a quoted operand (`rm -rf ".pi"`) is
// compared as the path the shell would pass, not as the literal source text.
// It is deliberately a small approximation: it is used only to decide whether
// a command is reaching for the state directory, never to execute anything.
func shellTokens(segment string) []string {
	var (
		out     []string
		cur     strings.Builder
		quote   rune
		escaped bool
		started bool
	)
	flush := func() {
		if started {
			out = append(out, cur.String())
			cur.Reset()
			started = false
		}
	}
	for _, r := range segment {
		switch {
		case escaped:
			cur.WriteRune(r)
			escaped = false
			started = true
		case r == '\\' && quote != '\'':
			escaped = true
			started = true
		case quote != 0:
			if r == quote {
				quote = 0
			} else {
				cur.WriteRune(r)
			}
		case r == '\'' || r == '"':
			quote = r
			started = true
		case unicode.IsSpace(r):
			flush()
		default:
			cur.WriteRune(r)
			started = true
		}
	}
	flush()
	return out
}
