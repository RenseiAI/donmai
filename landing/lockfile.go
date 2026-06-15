package landing

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// PackageManager identifies the package manager whose lock file may need
// regenerating after a landing.
type PackageManager string

// Supported package managers. PMNone disables lock-file regeneration; the rest
// are the recognized Node package managers.
const (
	// PMNone — no package manager; lock-file regeneration is skipped.
	PMNone PackageManager = "none"
	// PMNpm is npm.
	PMNpm PackageManager = "npm"
	// PMPnpm is pnpm.
	PMPnpm PackageManager = "pnpm"
	// PMYarn is yarn.
	PMYarn PackageManager = "yarn"
	// PMBun is bun.
	PMBun PackageManager = "bun"
)

// lockFiles maps a package manager to its lock file name. Ported from
// package-manager.ts LOCK_FILES.
var lockFiles = map[PackageManager]string{
	PMPnpm: "pnpm-lock.yaml",
	PMNpm:  "package-lock.json",
	PMYarn: "yarn.lock",
	PMBun:  "bun.lockb",
}

// regenCommands maps a package manager to its install command + args that
// explicitly allow lock-file regeneration. Ported from package-manager.ts
// getRegenerateCommand (only pnpm has a no-frozen flag).
var regenCommands = map[PackageManager][]string{
	PMPnpm: {"pnpm", "install", "--no-frozen-lockfile"},
	PMNpm:  {"npm", "install"},
	PMYarn: {"yarn", "install"},
	PMBun:  {"bun", "install"},
}

// gitattributesEntries maps a package manager to its lock-file merge=ours
// .gitattributes entry. Ported from package-manager.ts GITATTRIBUTES_ENTRIES.
var gitattributesEntries = map[PackageManager]string{
	PMPnpm: "pnpm-lock.yaml merge=ours",
	PMNpm:  "package-lock.json merge=ours",
	PMYarn: "yarn.lock merge=ours",
	PMBun:  "bun.lockb merge=ours",
}

// RegenerationResult is the outcome of a lock-file regeneration.
type RegenerationResult struct {
	Success        bool
	LockFile       string
	PackageManager PackageManager
	Error          string
}

// LockFileRegeneration deletes the conflicted lock file and regenerates it by
// running the package manager's install, then stages the result. Avoids
// hand-merging machine-generated lock files, which almost never merge cleanly.
//
// Ported from donmai-libraries merge-queue/lock-file-regeneration.ts.
type LockFileRegeneration struct {
	runner commandRunner
	// fs abstracts the filesystem so .gitattributes handling and the lock-file
	// existence check are testable without touching disk.
	fs lockFS
}

// lockFS is the filesystem surface LockFileRegeneration needs.
type lockFS interface {
	stat(path string) error
	readFile(path string) (string, error)
	writeFile(path, content string) error
}

// osFS is the production lockFS backed by the os package.
type osFS struct{}

func (osFS) stat(path string) error { _, err := os.Stat(path); return err }

func (osFS) readFile(path string) (string, error) {
	// #nosec G304 -- path is the repo's .gitattributes, built from a
	// caller-supplied repo path joined with a fixed filename, not user input.
	b, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

func (osFS) writeFile(path, content string) error {
	// .gitattributes is a tracked, shared config file: 0644 is the correct,
	// expected mode (it must be world-readable to function for every checkout).
	// #nosec G306 -- 0644 is intentional for a committed git config file.
	return os.WriteFile(path, []byte(content), 0o644)
}

// NewLockFileRegeneration returns a LockFileRegeneration backed by the
// production command runner and filesystem.
func NewLockFileRegeneration() *LockFileRegeneration {
	return &LockFileRegeneration{runner: defaultRunner, fs: osFS{}}
}

func (l *LockFileRegeneration) r() commandRunner {
	if l.runner == nil {
		return defaultRunner
	}
	return l.runner
}

func (l *LockFileRegeneration) f() lockFS {
	if l.fs == nil {
		return osFS{}
	}
	return l.fs
}

// ShouldRegenerate reports whether regeneration applies for the given package
// manager and config flag.
func (l *LockFileRegeneration) ShouldRegenerate(pm PackageManager, lockFileRegenerate bool) bool {
	return lockFileRegenerate && pm != PMNone
}

// LockFileName returns the lock file name for a package manager, or "" if none.
func (l *LockFileRegeneration) LockFileName(pm PackageManager) string {
	if pm == PMNone {
		return ""
	}
	return lockFiles[pm]
}

// Regenerate deletes the conflicted lock file (if present), runs the package
// manager's install to regenerate it, and stages the result. A missing lock file
// is not an error — the delete is skipped. An install failure yields a
// non-success result carrying the error message rather than returning an error,
// matching the TS source.
func (l *LockFileRegeneration) Regenerate(ctx context.Context, worktreePath string, pm PackageManager) (RegenerationResult, error) {
	lockFile := l.LockFileName(pm)
	if lockFile == "" {
		return RegenerationResult{
			Success:        false,
			LockFile:       "",
			PackageManager: pm,
			Error:          fmt.Sprintf("Unsupported package manager: %s", pm),
		}, nil
	}

	installCmd := regenCommands[pm]
	if len(installCmd) == 0 {
		return RegenerationResult{
			Success:        false,
			LockFile:       "",
			PackageManager: pm,
			Error:          fmt.Sprintf("No install command for: %s", pm),
		}, nil
	}

	// 1. Delete the conflicted lock file if it exists.
	lockFilePath := filepath.Join(worktreePath, lockFile)
	if l.f().stat(lockFilePath) == nil {
		if _, err := l.r().run(ctx, worktreePath, nil, "rm", lockFile); err != nil {
			return RegenerationResult{Success: false, LockFile: lockFile, PackageManager: pm, Error: err.Error()}, nil
		}
	}

	// 2. Run the package manager install to regenerate.
	if _, err := l.r().run(ctx, worktreePath, nil, installCmd[0], installCmd[1:]...); err != nil {
		return RegenerationResult{Success: false, LockFile: lockFile, PackageManager: pm, Error: err.Error()}, nil
	}

	// 3. Stage the regenerated lock file.
	if _, err := l.r().run(ctx, worktreePath, nil, "git", "add", lockFile); err != nil {
		return RegenerationResult{Success: false, LockFile: lockFile, PackageManager: pm, Error: err.Error()}, nil
	}

	return RegenerationResult{Success: true, LockFile: lockFile, PackageManager: pm}, nil
}

// EnsureGitAttributes ensures the .gitattributes entry that marks the lock file
// as merge=ours exists in repoPath. A no-op for PMNone. Idempotent: if the entry
// is already present, the file is left untouched.
func (l *LockFileRegeneration) EnsureGitAttributes(ctx context.Context, repoPath string, pm PackageManager) error {
	_ = ctx
	if pm == PMNone {
		return nil
	}
	entry := gitattributesEntries[pm]
	if entry == "" {
		return nil
	}

	path := filepath.Join(repoPath, ".gitattributes")
	content, err := l.f().readFile(path)
	if err != nil {
		// File does not exist yet — start from empty content.
		content = ""
	}

	if strings.Contains(content, entry) {
		return nil // already configured
	}

	var newContent string
	if content == "" || strings.HasSuffix(content, "\n") {
		newContent = content + entry + "\n"
	} else {
		newContent = content + "\n" + entry + "\n"
	}

	if err := l.f().writeFile(path, newContent); err != nil {
		return fmt.Errorf("EnsureGitAttributes write: %w", err)
	}
	return nil
}
