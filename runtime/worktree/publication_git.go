package worktree

import (
	"bytes"
	"context"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/RenseiAI/donmai/internal/gitexec"
)

const (
	publicationMaxTrackedBytes        int64 = 1 << 30
	publicationLocalInspectionTimeout       = 15 * time.Second
)

// publicationWorktreeDirty compares the index with raw checkout bytes. Git
// status/diff can invoke repository-configured clean or process filters while
// hashing a changed tracked file. A raw mismatch is conservatively dirty,
// including worktrees that require content filters for a clean Git status.
func (m *Manager) publicationWorktreeDirty(ctx context.Context, path string, config []string) (bool, error) {
	ctx, cancel := context.WithTimeout(ctx, publicationLocalInspectionTimeout)
	defer cancel()
	// Compare the index to HEAD before inspecting checkout bytes. A staged
	// addition or edit can match the worktree exactly while remaining wholly
	// unpublished. --cached reads only the index and object store; the explicit
	// diff flags disable checkout-defined drivers and text conversion.
	stagedArgs := append(append([]string{}, config...), "-C", path, "diff-index", "--cached", "--no-ext-diff", "--no-textconv", "--name-only", "HEAD", "--")
	staged, err := m.runPublicationGit(ctx, "", stagedArgs...)
	if err != nil || len(staged) > publicationMaxGitOutput {
		return false, fmt.Errorf("runtime/worktree: compare publication index to HEAD: %w", publicationGitOutputError(err, staged))
	}
	if len(staged) != 0 {
		return true, nil
	}
	args := append(append([]string{}, config...), "-C", path, "ls-files", "--stage", "-z")
	output, err := m.runPublicationGit(ctx, "", args...)
	if err != nil || len(output) > publicationMaxGitOutput {
		return false, fmt.Errorf("runtime/worktree: enumerate publication index: %w", publicationGitOutputError(err, output))
	}
	seen := make(map[string]struct{})
	var total int64
	for _, record := range strings.Split(string(output), "\x00") {
		if record == "" {
			continue
		}
		metadata, name, found := strings.Cut(record, "\t")
		fields := strings.Fields(metadata)
		if !found || len(fields) != 3 || fields[2] != "0" || !filepath.IsLocal(name) {
			return false, fmt.Errorf("runtime/worktree: invalid publication index entry")
		}
		if _, duplicate := seen[name]; duplicate {
			return false, fmt.Errorf("runtime/worktree: duplicate publication index entry")
		}
		seen[name] = struct{}{}
		dirty, size, compareErr := m.rawPublicationBlobDiffers(ctx, path, config, name, fields[0], fields[1], publicationMaxTrackedBytes-total)
		if compareErr != nil {
			return false, compareErr
		}
		if dirty {
			return true, nil
		}
		total += size
	}
	otherArgs := append(append([]string{}, config...), "-C", path, "ls-files", "--others", "--exclude-standard", "-z")
	other, err := m.runPublicationGit(ctx, "", otherArgs...)
	if err != nil || len(other) > publicationMaxGitOutput {
		return false, fmt.Errorf("runtime/worktree: enumerate publication untracked files: %w", publicationGitOutputError(err, other))
	}
	return len(other) != 0, nil
}

func publicationGitOutputError(err error, output []byte) error {
	if err != nil {
		return err
	}
	return fmt.Errorf("git output exceeded %d bytes (received %d)", publicationMaxGitOutput, len(output))
}

func (m *Manager) rawPublicationBlobDiffers(ctx context.Context, root string, config []string, name, mode, objectID string, remaining int64) (bool, int64, error) {
	if err := ctx.Err(); err != nil {
		return false, 0, fmt.Errorf("runtime/worktree: compare publication checkout: %w", err)
	}
	full := filepath.Join(root, name)
	info, err := os.Lstat(full)
	if os.IsNotExist(err) {
		return true, 0, nil
	}
	if err != nil {
		return false, 0, fmt.Errorf("runtime/worktree: inspect publication checkout: %w", err)
	}
	var size int64
	switch mode {
	case "100644", "100755":
		if !info.Mode().IsRegular() || (mode == "100755") != (info.Mode().Perm()&0o111 != 0) {
			return true, 0, nil
		}
		size = info.Size()
		if size < 0 || size > remaining {
			return false, 0, fmt.Errorf("runtime/worktree: publication tracked-byte bound exceeded")
		}
	case "120000":
		if info.Mode()&os.ModeSymlink == 0 {
			return true, 0, nil
		}
		link, readErr := os.Readlink(full)
		if readErr != nil {
			return false, 0, fmt.Errorf("runtime/worktree: read publication link: %w", readErr)
		}
		size = int64(len(link))
		if size > remaining {
			return false, 0, fmt.Errorf("runtime/worktree: publication tracked-byte bound exceeded")
		}
		args := append(append([]string{}, config...), "-C", root, "hash-object", "--no-filters", "--stdin")
		output, hashErr := m.runPublicationGitInput(ctx, "", []byte(link), args...)
		if hashErr != nil || len(output) > 128 {
			return false, 0, fmt.Errorf("runtime/worktree: hash raw publication link: %w", publicationGitOutputError(hashErr, output))
		}
		return strings.TrimSpace(string(output)) != objectID, size, nil
	default:
		// Submodules and unknown modes retain rather than invoking checkout
		// conversions or following paths outside the root.
		return true, 0, nil
	}
	if !validGitObjectID(objectID) {
		return false, 0, fmt.Errorf("runtime/worktree: invalid publication object ID")
	}
	// --no-filters makes hash-object read raw bytes; otherwise it can run a
	// repository-configured clean/process filter just like git status.
	args := append(append([]string{}, config...), "-C", root, "hash-object", "--no-filters", "--", name)
	output, err := m.runPublicationGit(ctx, "", args...)
	if err != nil || len(output) > 128 {
		return false, 0, fmt.Errorf("runtime/worktree: hash raw publication checkout: %w", publicationGitOutputError(err, output))
	}
	if strings.TrimSpace(string(output)) != objectID {
		return true, size, nil
	}
	return false, size, nil
}

// safePublicationRepository admits only transports whose Git execution path is
// known here. Unknown git-remote-* protocols must never be discovered from PATH.
func safePublicationRepository(repository string) bool {
	if repository == "" || repository != strings.TrimSpace(repository) ||
		strings.HasPrefix(repository, "-") || strings.Contains(repository, "::") ||
		strings.ContainsAny(repository, "\x00\r\n") {
		return false
	}
	if filepath.IsAbs(repository) {
		return true
	}
	if strings.Contains(repository, "://") {
		parsed, err := url.Parse(repository)
		if err != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
			return false
		}
		switch parsed.Scheme {
		case "http", "https", "ssh":
			return parsed.Host != "" && parsed.Path != ""
		case "file":
			return (parsed.Host == "" || parsed.Host == "localhost") && filepath.IsAbs(parsed.Path)
		default:
			return false
		}
	}
	host, path, scpStyle := strings.Cut(repository, ":")
	if user, machine, hasUser := strings.Cut(host, "@"); hasUser {
		if user == "" || machine == "" || strings.Contains(machine, "@") {
			return false
		}
		host = user + "@" + machine
	}
	return scpStyle && host != "" && path != "" && !strings.ContainsAny(host, "/\\ \t!$&;|<>()`\"'") &&
		!strings.ContainsAny(path, "\x00\r\n")
}

// runPublicationGit uses a dedicated process boundary. General worktree Git
// operations keep their existing environment contract; publication proof may
// not inherit executable Git configuration from either that contract or the
// operator's shell. An explicitly injected CommandRunner remains a test seam.
func (m *Manager) runPublicationGit(ctx context.Context, repository string, args ...string) ([]byte, error) {
	return m.runPublicationGitInput(ctx, repository, nil, args...)
}

func (m *Manager) runPublicationGitInput(ctx context.Context, repository string, input []byte, args ...string) ([]byte, error) {
	commandCtx, cancel := context.WithTimeout(ctx, publicationGitTimeout)
	defer cancel()
	if m.publicationCustomRunner {
		if input != nil {
			return nil, fmt.Errorf("runtime/worktree: publication git input unsupported by custom runner")
		}
		return m.runner(commandCtx, "git", args...)
	}
	if m.publicationGitPath == "" {
		return nil, fmt.Errorf("runtime/worktree: publication git executable unavailable")
	}
	var authHeader string
	if repository != "" {
		var err error
		authHeader, err = m.publicationAuthHeader(commandCtx, repository)
		if err != nil {
			return nil, err
		}
	}
	env := publicationGitEnv(repository, authHeader)
	// nolint:gosec // fixed, construction-time-resolved git executable and
	// bounded literal inspection arguments; the admitted repository is one argv.
	cmd := exec.CommandContext(commandCtx, m.publicationGitPath, args...)
	// Remote ls-remote must not load .git/config from an ambient process cwd.
	// Local reads carry an explicit -C checkout and keep their CLI overrides.
	cmd.Dir = "/"
	cmd.Env = env
	if input != nil {
		cmd.Stdin = bytes.NewReader(input)
	}
	return cmd.CombinedOutput()
}

func (m *Manager) publicationAuthHeader(ctx context.Context, repository string) (string, error) {
	if m.gitAuth != nil {
		header, _, err := m.gitAuth(ctx, repository)
		if err != nil {
			return "", fmt.Errorf("runtime/worktree: resolve publication git auth: %w", err)
		}
		if header != "" && !validPublicationAuthHeader(header) {
			return "", fmt.Errorf("runtime/worktree: invalid publication git authorization header")
		}
		return header, nil
	}
	return inheritedPublicationAuthHeader(repository)
}

func inheritedPublicationAuthHeader(repository string) (string, error) {
	key, scoped := gitexec.ExtraHeaderConfigKey(repository)
	if !scoped {
		return "", nil
	}
	rawCount := os.Getenv("GIT_CONFIG_COUNT")
	if rawCount == "" {
		return "", nil
	}
	count, err := strconv.Atoi(rawCount)
	if err != nil || count < 0 || count > 128 {
		return "", fmt.Errorf("runtime/worktree: invalid inherited git config count")
	}
	var header string
	for i := range count {
		if os.Getenv(fmt.Sprintf("GIT_CONFIG_KEY_%d", i)) != key {
			continue
		}
		value := os.Getenv(fmt.Sprintf("GIT_CONFIG_VALUE_%d", i))
		if header != "" || !validPublicationAuthHeader(value) {
			return "", fmt.Errorf("runtime/worktree: invalid inherited publication git authorization")
		}
		header = value
	}
	return header, nil
}

func validPublicationAuthHeader(header string) bool {
	const prefix = "Authorization:"
	return len(header) > len(prefix) && strings.EqualFold(header[:len(prefix)], prefix) &&
		strings.TrimSpace(header[len(prefix):]) != "" && !strings.ContainsAny(header, "\x00\r\n")
}

func publicationGitEnv(repository, header string) []string {
	base := []string{
		"PATH=/usr/bin:/bin",
		"HOME=" + os.Getenv("HOME"),
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_CONFIG_SYSTEM=/dev/null",
		"GIT_CONFIG_GLOBAL=/dev/null",
		"GIT_TERMINAL_PROMPT=0",
		"GCM_INTERACTIVE=never",
		"SSH_ASKPASS=/usr/bin/false",
		"GIT_SSH_COMMAND=/usr/bin/ssh -F /dev/null -o BatchMode=yes -o ProxyCommand=none -o ProxyJump=none -o PermitLocalCommand=no",
		"GIT_NO_REPLACE_OBJECTS=1",
	}
	if socket := os.Getenv("SSH_AUTH_SOCK"); socket != "" {
		base = append(base, "SSH_AUTH_SOCK="+socket)
	}
	// A CA file is passive trust data; unlike proxy or Git config variables it
	// cannot name a process to execute. It also supports an admitted HTTPS
	// remote served by a non-system CA without disabling verification.
	if ca := os.Getenv("GIT_SSL_CAINFO"); filepath.IsAbs(ca) {
		base = append(base, "GIT_SSL_CAINFO="+ca)
	}
	env := gitexec.HardenedEnv(base, true, gitexec.Auth{Header: header, RemoteURL: repository})
	return append(env, "GIT_ASKPASS=/usr/bin/false")
}
