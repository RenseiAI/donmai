package runner

import (
	"context"
	"fmt"
	"os"
	"regexp"
	"strconv"
	"strings"
)

// This file holds the content half of the WIP checkpoint's secret filter.
// The name half lives in backstop.go's shouldExcludeFromWIPCheckpoint: it
// drops secret-shaped PATHS from the scratch index before the checkpoint
// commits. This file drops secret-bearing CONTENT: it scans every blob the
// checkpoint would publish — the staged dirty delta and the committed-but-
// unpushed history — for the session's own secret values and for common
// token shapes. A filename deny-list fails open on every shape it does not
// name; the content gate fails closed on the values that actually matter.

// wipSecretContentRE matches common secret token shapes that must never
// enter a remote WIP checkpoint: provider-issued tokens, cloud access-key
// IDs, chat-platform tokens and PEM private-key blocks.
var wipSecretContentRE = regexp.MustCompile(`(?:gh[pohu]_[A-Za-z0-9]{20,}|github_pat_[A-Za-z0-9_]{20,}|sk-ant-[A-Za-z0-9_-]{10,}|sk-[A-Za-z0-9]{20,}|AKIA[0-9A-Z]{16}|xox[abposr]-[A-Za-z0-9-]{10,}|-----BEGIN [A-Z0-9 ]*PRIVATE KEY-----)`)

// wipSecretEnvNameRE matches environment variable names whose values are
// treated as secrets by the content gate: anything token-, key-,
// secret- or password-shaped.
var wipSecretEnvNameRE = regexp.MustCompile(`(?i)TOKEN|KEY|SECRET|PASSWORD`)

const (
	// wipSecretMinValueLen floors secret values: short values ("top",
	// "true", "localhost") appear in ordinary prose and would turn the
	// gate into a false-positive machine.
	wipSecretMinValueLen = 8
	// wipSecretMaxValueLen caps secret values so one pathological
	// environment value cannot make every blob scan quadratic.
	wipSecretMaxValueLen = 8192
	// wipSecretScanMaxBytes caps one blob's content scan: larger blobs
	// skip the content gate (the name rules still apply to them).
	wipSecretScanMaxBytes = 1 << 20
	// wipSecretWarnPaths caps how many denied paths one warning names;
	// beyond it the warning counts the rest instead of growing with the
	// checkout.
	wipSecretWarnPaths = 5
)

// wipCheckpointSecretValues collects the secret values the checkpoint's
// content gate scans for: the session bearers, the session env values with
// secret-shaped names, and the process env values with secret-shaped names
// (provider keys and credential files reach the checkout through all three).
func wipCheckpointSecretValues(qw QueuedWork) []string {
	seen := map[string]bool{}
	var out []string
	add := func(v string) {
		if len(v) < wipSecretMinValueLen || len(v) > wipSecretMaxValueLen || seen[v] {
			return
		}
		seen[v] = true
		out = append(out, v)
	}
	add(qw.AuthToken)
	add(qw.McpAuthToken)
	for name, value := range qw.Env {
		if wipSecretEnvNameRE.MatchString(name) {
			add(value)
		}
	}
	for _, kv := range os.Environ() {
		if name, value, ok := strings.Cut(kv, "="); ok && wipSecretEnvNameRE.MatchString(name) {
			add(value)
		}
	}
	return out
}

// blobContainsSecret reports whether content carries a secret: one of the
// session's secret values or a common token shape.
func blobContainsSecret(content string, values []string) bool {
	for _, v := range values {
		if strings.Contains(content, v) {
			return true
		}
	}
	return wipSecretContentRE.MatchString(content)
}

// blobSize parses one `git cat-file -s` output.
func blobSize(out string) (int64, error) {
	size, err := strconv.ParseInt(strings.TrimSpace(out), 10, 64)
	if err != nil {
		return 0, fmt.Errorf("size blob: unexpected output %q", out)
	}
	return size, nil
}

// stagedBlobHasSecret reports whether the scratch-index blob :path carries
// a secret. Oversize and binary blobs skip the content gate (the name rules
// still apply to them); any git failure errors so the caller can fail
// closed on the path.
func stagedBlobHasSecret(ctx context.Context, dir string, env []string, path string, values []string) (bool, error) {
	sizeOut, err := checkpointGitStdout(ctx, dir, env, "cat-file", "-s", ":"+path)
	if err != nil {
		return false, fmt.Errorf("size staged blob: %w", err)
	}
	size, err := blobSize(sizeOut)
	if err != nil {
		return false, err
	}
	if size > wipSecretScanMaxBytes {
		return false, nil
	}
	blob, err := checkpointGitStdout(ctx, dir, env, "show", ":"+path)
	if err != nil {
		return false, fmt.Errorf("read staged blob: %w", err)
	}
	if strings.IndexByte(blob, 0) >= 0 {
		return false, nil
	}
	return blobContainsSecret(blob, values), nil
}

// refBlobHasSecret reports whether the blob at rev:path carries a secret.
// A path missing from rev (deleted later in the range) is not in the
// published tree and reports no hit; oversize and binary blobs skip the
// content gate like the staged gate above. A blob that sizes fine but then
// fails to read errors so the caller can fail closed on the push.
func refBlobHasSecret(ctx context.Context, dir, rev, path string, values []string) (bool, error) {
	sizeOut, err := checkpointGitStdout(ctx, dir, nil, "cat-file", "-s", rev+":"+path)
	if err != nil {
		return false, nil
	}
	size, err := blobSize(sizeOut)
	if err != nil {
		return false, err
	}
	if size > wipSecretScanMaxBytes {
		return false, nil
	}
	blob, err := checkpointGitStdout(ctx, dir, nil, "show", rev+":"+path)
	if err != nil {
		return false, fmt.Errorf("read blob at %s: %w", rev, err)
	}
	if strings.IndexByte(blob, 0) >= 0 {
		return false, nil
	}
	return blobContainsSecret(blob, values), nil
}

// splitNULPaths splits one `git -z` name output into its paths.
func splitNULPaths(out string) []string {
	var paths []string
	for _, p := range strings.Split(out, "\x00") {
		if p != "" {
			paths = append(paths, p)
		}
	}
	return paths
}

// checkpointHistoryPaths lists the paths the WIP push would newly publish:
// the provisioned-base-to-target diff, or, without a recorded base, the
// union of files touched by the unpushed commits.
func checkpointHistoryPaths(ctx context.Context, target rescueTarget, to string) ([]string, error) {
	if target.base != "" {
		out, err := checkpointGitStdout(ctx, target.path, nil,
			"-c", "core.quotePath=false", "diff", "--name-only", "-z", target.base, to, "--")
		if err != nil {
			return nil, fmt.Errorf("diff committed range for secret scan: %w", err)
		}
		return splitNULPaths(out), nil
	}
	commits, err := unpushedCommits(ctx, target)
	if err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	var paths []string
	for _, line := range commits {
		sha, _, _ := strings.Cut(line, " ")
		if sha == "" {
			continue
		}
		out, err := checkpointGitStdout(ctx, target.path, nil,
			"-c", "core.quotePath=false", "diff-tree", "--no-commit-id", "--name-only", "-r", "--root", "-z", sha, "--")
		if err != nil {
			return nil, fmt.Errorf("list unpushed commit for secret scan: %w", err)
		}
		for _, p := range splitNULPaths(out) {
			if !seen[p] {
				seen[p] = true
				paths = append(paths, p)
			}
		}
	}
	return paths, nil
}

// checkpointHistoryDenied lists the committed-but-unpublished paths the WIP
// push would publish that the secret filter denies, by name or by content.
// The dirty delta is scanned separately in the scratch index; this gates the
// committed history the push carries as-is, so a secret the session
// committed (rather than left dirty) never leaves the host through the
// reserved ref.
func checkpointHistoryDenied(ctx context.Context, target rescueTarget, to string, values []string) ([]string, error) {
	paths, err := checkpointHistoryPaths(ctx, target, to)
	if err != nil {
		return nil, err
	}
	var denied []string
	for _, path := range paths {
		if shouldExcludeFromWIPCheckpoint(path) {
			denied = append(denied, path)
			continue
		}
		hit, herr := refBlobHasSecret(ctx, target.path, to, path, values)
		if herr != nil {
			return nil, herr
		}
		if hit {
			denied = append(denied, path)
		}
	}
	return denied, nil
}

// quoteWIPPaths names denied paths for a warning: quoted and capped, so the
// warning identifies the files without growing with the checkout and never
// carries a secret value, only paths.
func quoteWIPPaths(paths []string) string {
	quoted := make([]string, 0, min(len(paths), wipSecretWarnPaths))
	for _, p := range paths[:min(len(paths), wipSecretWarnPaths)] {
		quoted = append(quoted, fmt.Sprintf("%q", p))
	}
	if len(paths) > wipSecretWarnPaths {
		quoted = append(quoted, fmt.Sprintf("and %d more", len(paths)-wipSecretWarnPaths))
	}
	return strings.Join(quoted, ", ")
}
