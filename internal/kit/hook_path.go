package kit

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// ErrHookPathInvalid marks a hook that names a package path the kit cannot
// own: absolute, escaping its package root (lexically or through a symlink),
// missing, not a regular file, or declared by a manifest with no verified
// package root. It wraps ErrPackagePathInvalid.
var ErrHookPathInvalid = fmt.Errorf("%w: kit hook path invalid", ErrPackagePathInvalid)

// hookShellMeta is every character that makes a hook string shell text rather
// than a bare path: whitespace, quoting, expansion, redirection, control
// operators and globbing. Backslash is not listed; it is a path separator
// only for a windows target.
const hookShellMeta = " \t\r\n;&|<>()$`\"'*?[]{}!~#=%^"

// isHookPath reports whether a hook string is a bare relative or absolute
// path rather than inline shell text. Until the donmai.dev/v3 manifest types
// package paths, the rule is purely lexical, and it deliberately ignores
// filename extensions:
//
//   - a path is a single token (no whitespace, quotes, expansions or other
//     shell metacharacters), contains a path separator (or is "." or ".."),
//     does not start with "-" and is not a URL;
//   - everything else is inline shell text: a command with arguments
//     ("bin/setup.sh --fast"), a pipeline, an expansion ("$HOME/x") or a bare
//     word with no separator ("make", "setup"), which names a system
//     executable.
//
// A bare word with no separator is never a path, so a kit that wants its own
// script must write the separator ("bin/setup", not "setup").
func isHookPath(hook string) bool {
	if hook == "" || strings.ContainsAny(hook, hookShellMeta) || strings.Contains(hook, "://") || strings.HasPrefix(hook, "-") {
		return false
	}
	return hook == ".." || hook == "." || strings.ContainsAny(hook, `/\`)
}

// resolveHookCommand turns one declared hook into the command the runner
// executes. Inline shell text is returned unchanged (it runs with the
// repository as the working directory, as before). A path resolves inside the
// verified package root and is returned as one shell-quoted absolute path; a
// path that cannot be owned by the kit is refused with ErrHookPathInvalid.
// Nothing here ever resolves against the repository.
func resolveHookCommand(v ManifestView, targetOS, phase, hook string) (string, error) {
	if !isHookPath(hook) {
		return hook, nil
	}
	refuse := func(why string) (string, error) {
		return "", fmt.Errorf("kit %s: %s hook %q: %s: %w", v.ID, phase, hook, why, ErrHookPathInvalid)
	}
	if v.PackageRoot == "" {
		return refuse("the manifest has no verified package root, so a relative path has nothing to resolve against; use inline shell text or install the kit as a package")
	}
	if !filepath.IsAbs(v.PackageRoot) {
		return refuse("the package root is not absolute")
	}
	rel := hook
	if targetOS == OSWindows {
		rel = strings.ReplaceAll(rel, `\`, "/")
	}
	rel = strings.TrimPrefix(rel, "./")
	if strings.HasPrefix(rel, "/") {
		return refuse("an absolute path names the host, not the package")
	}
	if rel == ".." || strings.HasPrefix(rel, "../") || strings.Contains(rel, "/../") || strings.HasSuffix(rel, "/..") ||
		strings.Contains(rel, `\`) || strings.Contains(rel, "//") || strings.HasSuffix(rel, "/") || rel == "." || strings.Contains(rel, "/./") {
		return refuse("not a canonical path inside the package")
	}
	root, err := filepath.EvalSymlinks(v.PackageRoot)
	if err != nil {
		return refuse("the package root cannot be resolved")
	}
	resolved, err := filepath.EvalSymlinks(filepath.Join(root, filepath.FromSlash(rel)))
	if err != nil {
		return refuse("the file is not in the package")
	}
	within, err := filepath.Rel(root, resolved)
	if err != nil || within == ".." || strings.HasPrefix(within, ".."+string(filepath.Separator)) || filepath.IsAbs(within) {
		return refuse("it resolves outside the package root")
	}
	info, err := os.Stat(resolved)
	if err != nil || !info.Mode().IsRegular() {
		return refuse("it is not a regular file")
	}
	return shellQuote(resolved), nil
}

// shellQuote single-quotes s for `sh -c`.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
