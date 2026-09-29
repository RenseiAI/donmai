package worktree

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/RenseiAI/donmai/internal/gitexec"
)

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
	commandCtx, cancel := context.WithTimeout(ctx, publicationGitTimeout)
	defer cancel()
	if m.publicationCustomRunner {
		return m.runner(commandCtx, "git", args...)
	}
	if m.publicationGitPath == "" {
		return nil, fmt.Errorf("runtime/worktree: publication git executable unavailable")
	}
	authHeader, err := m.publicationAuthHeader(commandCtx, repository)
	if err != nil {
		return nil, err
	}
	env := publicationGitEnv(repository, authHeader)
	// nolint:gosec // fixed, construction-time-resolved git executable and
	// bounded literal inspection arguments; the admitted repository is one argv.
	cmd := exec.CommandContext(commandCtx, m.publicationGitPath, args...)
	// Remote ls-remote must not load .git/config from an ambient process cwd.
	// Local reads carry an explicit -C checkout and keep their CLI overrides.
	cmd.Dir = "/"
	cmd.Env = env
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
