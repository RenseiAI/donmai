package server

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Config is the validated startup configuration for the code-intelligence MCP
// server. It maps 1:1 to the frozen `donmai mcp code-intel` flag contract.
type Config struct {
	// Root is the ABSOLUTE path to the session repo/worktree root the engine
	// indexes. Required. Wave-0 traced that the runner process cwd is
	// unreliable across sandbox targets, so the root is always explicit.
	Root string

	// RepoPath is an OPTIONAL path RELATIVE to Root scoping indexing to a
	// subtree (e.g. one package in a monorepo). Absolute paths and traversal
	// escapes are rejected, matching the `donmai code --repo-path` semantics.
	RepoPath string

	// Tools is an OPTIONAL subset of the six tool names to expose. Empty means
	// all six. Unknown names are a startup error.
	Tools []string

	// Logf, when non-nil, receives warm-up / lifecycle log lines. It MUST NOT
	// write to stdout (the JSON-RPC protocol channel); the CLI wires it to
	// stderr. Nil discards all logging.
	Logf func(format string, args ...any)
}

// resolveIndexRoot validates Root and resolves the effective index directory,
// optionally narrowed to a RepoPath subtree.
//
// Root must be a non-empty ABSOLUTE path that exists and is a directory — the
// server refuses to serve otherwise (fail-loud startup). RepoPath, when
// non-empty, must be RELATIVE and must resolve (after filepath.Clean) to a
// location inside Root; absolute paths and ../ escapes are rejected. These are
// the same rules the afcli `code` group enforces (indexRoot in afcli/code.go),
// re-implemented here because that root is discovered from cwd whereas ours is
// always explicit.
func resolveIndexRoot(root, repoPath string) (string, error) {
	if root == "" {
		return "", errors.New("--root is required (absolute path to the repo root)")
	}
	if !filepath.IsAbs(root) {
		return "", fmt.Errorf("--root must be an absolute path, got %q", root)
	}
	info, err := os.Stat(root)
	if err != nil {
		return "", fmt.Errorf("--root %q: %w", root, err)
	}
	if !info.IsDir() {
		return "", fmt.Errorf("--root %q is not a directory", root)
	}
	root = filepath.Clean(root)

	if repoPath == "" {
		return root, nil
	}
	if filepath.IsAbs(repoPath) {
		return "", fmt.Errorf("--repo-path must be a relative path under the root, got absolute path %q", repoPath)
	}
	cleaned := filepath.Clean(filepath.Join(root, repoPath))
	rel, err := filepath.Rel(root, cleaned)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("--repo-path %q escapes the root %q", repoPath, root)
	}
	sInfo, err := os.Stat(cleaned)
	if err != nil {
		return "", fmt.Errorf("--repo-path %q: %w", repoPath, err)
	}
	if !sInfo.IsDir() {
		return "", fmt.Errorf("--repo-path %q is not a directory", repoPath)
	}
	return cleaned, nil
}

// validateTools resolves the enabled tool subset. An empty subset expands to
// all six tools (in canonical order). A non-empty subset is preserved in the
// caller's order, de-duplicated; any unknown name is a hard error so a typo in
// the --tools flag fails loud at startup rather than silently exposing fewer
// tools than intended.
func validateTools(subset []string) ([]string, error) {
	if len(subset) == 0 {
		return allToolNames(), nil
	}
	known := make(map[string]bool, 6)
	for _, n := range allToolNames() {
		known[n] = true
	}
	seen := make(map[string]bool, len(subset))
	out := make([]string, 0, len(subset))
	for _, raw := range subset {
		name := strings.TrimSpace(raw)
		if name == "" {
			continue
		}
		if !known[name] {
			return nil, fmt.Errorf("--tools: unknown tool %q (valid: %s)", name, strings.Join(allToolNames(), ", "))
		}
		if seen[name] {
			continue
		}
		seen[name] = true
		out = append(out, name)
	}
	if len(out) == 0 {
		return nil, errors.New("--tools: no valid tool names after parsing")
	}
	return out, nil
}

// resolveScopedFile resolves a caller-supplied file path against Root, refusing
// absolute paths and traversal escapes. Used by the check-duplicate tool for
// its optional contentFile argument: the engine's CLI accepts an arbitrary
// host path there, but the agent-facing MCP surface confines it to the indexed
// root so a tool call cannot read files outside --root.
func resolveScopedFile(root, p string) (string, error) {
	if p == "" {
		return "", errors.New("contentFile is empty")
	}
	if filepath.IsAbs(p) {
		return "", fmt.Errorf("contentFile must be relative to the root, got absolute path %q", p)
	}
	cleaned := filepath.Clean(filepath.Join(root, p))
	rel, err := filepath.Rel(root, cleaned)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("contentFile %q escapes the root %q", p, root)
	}
	return cleaned, nil
}
