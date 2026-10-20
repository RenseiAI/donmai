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
// IDs, chat-platform tokens, PEM private-key blocks and connection-string
// userinfo. The GitHub shape is intentionally wide (gh[a-z]_): it covers
// ghp_/gho_/ghs_/ghr_/ghu_ and any future single-letter prefix without
// another allow-list update. The sk- shape allows - and _ so project and
// service keys (sk-proj-, sk-svcacct-) match like plain sk- keys.
var wipSecretContentRE = regexp.MustCompile(`(?:gh[a-z]_[A-Za-z0-9]{20,}|github_pat_[A-Za-z0-9_]{20,}|sk-ant-[A-Za-z0-9_-]{10,}|sk-[A-Za-z0-9_-]{20,}|AKIA[0-9A-Z]{16}|xox[abposr]-[A-Za-z0-9-]{10,}|-----BEGIN [A-Z0-9 ]*PRIVATE KEY-----|://[^/\s]+:[^/\s]+@)`)

// wipSecretEnvNameRE matches environment variable names whose process-env
// values are treated as secrets by the content gate. Session env values
// (qw.Env) are all treated as secrets without a name check: a session that
// carries a credential under an innocuous name (DATABASE_URL, CONFIG) must
// still fail closed.
var wipSecretEnvNameRE = regexp.MustCompile(`(?i)TOKEN|KEY|SECRET|PASS|PAT\b|CREDENTIAL|AUTH|_URL$|DSN`)

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
// content gate scans for: the session bearers, ALL session env values (a
// credential under an innocuous name is still a credential), and the
// process env values with secret-shaped names (provider keys and
// credential files reach the checkout through all three).
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
	for _, value := range qw.Env {
		add(value)
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
// a secret. Oversize, binary and unreadable blobs ERROR so the caller fails
// closed on the whole checkpoint: the push publishes every blob it cannot
// scan otherwise, and scanning bytes needs no text decode. Any git failure
// errors the same way.
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
		return false, fmt.Errorf("unscannable staged blob %q: %d bytes over the %d-byte scan cap", path, size, wipSecretScanMaxBytes)
	}
	blob, err := checkpointGitStdout(ctx, dir, env, "show", ":"+path)
	if err != nil {
		return false, fmt.Errorf("read staged blob: %w", err)
	}
	if strings.IndexByte(blob, 0) >= 0 {
		return false, fmt.Errorf("unscannable staged blob %q: binary content", path)
	}
	return blobContainsSecret(blob, values), nil
}

// refBlobHasSecret reports whether the blob at rev:path carries a secret.
// Oversize, binary and unreadable blobs ERROR so the caller fails closed on
// the whole push. A blob that sizes fine but then fails to read errors the
// same way. Callers only invoke it for paths present in rev (added or
// modified in that commit); a deleted path is never scanned.
func refBlobHasSecret(ctx context.Context, dir, rev, path string, values []string) (bool, error) {
	sizeOut, err := checkpointGitStdout(ctx, dir, nil, "cat-file", "-s", rev+":"+path)
	if err != nil {
		return false, fmt.Errorf("size blob at %s: %w", rev, err)
	}
	size, err := blobSize(sizeOut)
	if err != nil {
		return false, err
	}
	if size > wipSecretScanMaxBytes {
		return false, fmt.Errorf("unscannable blob %q at %s: %d bytes over the %d-byte scan cap", path, rev, size, wipSecretScanMaxBytes)
	}
	blob, err := checkpointGitStdout(ctx, dir, nil, "show", rev+":"+path)
	if err != nil {
		return false, fmt.Errorf("read blob at %s: %w", rev, err)
	}
	if strings.IndexByte(blob, 0) >= 0 {
		return false, fmt.Errorf("unscannable blob %q at %s: binary content", path, rev)
	}
	return blobContainsSecret(blob, values), nil
}

// checkpointUnpushedSHAs lists every commit the WIP push would newly
// publish, oldest first: the provisioned-base-to-target range, or, without
// a recorded base, the unpushed commits on HEAD. One per-commit walk covers
// both modes so the base branch cannot drift weaker than the no-base walk.
func checkpointUnpushedSHAs(ctx context.Context, target rescueTarget, to string) ([]string, error) {
	if target.base != "" {
		out, err := checkpointGitStdout(ctx, target.path, nil, "rev-list", "--reverse", target.base+".."+to, "--")
		if err != nil {
			return nil, fmt.Errorf("list committed range for secret scan: %w", err)
		}
		return filterEmpty(strings.Split(strings.TrimSpace(out), "\n")), nil
	}
	commits, err := unpushedCommits(ctx, target)
	if err != nil {
		return nil, err
	}
	var shas []string
	for _, line := range commits {
		sha, _, _ := strings.Cut(line, " ")
		if sha != "" {
			shas = append(shas, sha)
		}
	}
	return shas, nil
}

// commitStatusPaths splits one `git diff-tree --name-status -z` output into
// alternating status/path fields: status, path, status, path, ... Renames
// are disabled by the caller so every entry is one status plus one path.

// checkpointHistoryDenied lists the committed-but-unpublished paths the WIP
// push would publish that the secret filter denies, by name or by content.
// Every unpushed commit is walked (not just the final tree): the push
// publishes each intermediate tree, so a secret committed and then deleted
// or scrubbed later still leaves the host in its own commit. A deleted path
// is name-checked only; an added or modified path is content-scanned at its
// own commit. Any content hit, unscannable blob or read error denies the
// range so the push is skipped with a warning and the local rescue patch
// keeps everything.
func checkpointHistoryDenied(ctx context.Context, target rescueTarget, to string, values []string) ([]string, error) {
	shas, err := checkpointUnpushedSHAs(ctx, target, to)
	if err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	var denied []string
	deny := func(path string) {
		if !seen[path] {
			seen[path] = true
			denied = append(denied, path)
		}
	}
	for _, sha := range shas {
		out, err := checkpointGitStdout(ctx, target.path, nil,
			"-c", "core.quotePath=false", "diff-tree", "--no-commit-id", "--name-status", "-r", "--root", "--no-renames", "-z", sha, "--")
		if err != nil {
			return nil, fmt.Errorf("list unpushed commit for secret scan: %w", err)
		}
		fields := strings.Split(out, "\x00")
		for i := 0; i+1 < len(fields); i += 2 {
			status, path := fields[i], fields[i+1]
			if path == "" {
				continue
			}
			if shouldExcludeFromWIPCheckpoint(path) {
				deny(path)
				continue
			}
			if strings.HasPrefix(status, "D") {
				continue
			}
			hit, herr := refBlobHasSecret(ctx, target.path, sha, path, values)
			if herr != nil {
				return nil, herr
			}
			if hit {
				deny(path)
			}
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
