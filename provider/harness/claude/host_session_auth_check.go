package claude

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/RenseiAI/donmai/agent"
	"github.com/RenseiAI/donmai/tempown"
)

const (
	hostLoginTimeout   = 5 * time.Second
	hostLoginOutputMax = 16 << 10
)

type boundedHostLoginOutput struct{ buffer bytes.Buffer }

func (b *boundedHostLoginOutput) Write(data []byte) (int, error) {
	if len(data) > hostLoginOutputMax-b.buffer.Len() {
		return 0, errors.New("claude host login status exceeded its output bound")
	}
	return b.buffer.Write(data)
}

// CheckHostSessionLogin observes the selected native CLI's first-party host
// login. It carries only the operator's intentional home/config locators to
// the status child; unrelated process credentials are never inherited.
// This is local login evidence, not online model entitlement.
func CheckHostSessionLogin(ctx context.Context, binary string) error {
	if ctx == nil || !filepath.IsAbs(binary) {
		return fmt.Errorf("%w: Claude host login needs an absolute CLI path and context", agent.ErrProviderUnavailable)
	}
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("%w: Claude host login: %w", agent.ErrProviderUnavailable, err)
	}
	baseEnv, err := loginBaseEnv("", true)
	if err != nil {
		return err
	}
	_, _, err = runLoginStatus(ctx, binary, baseEnv)
	return err
}

// loginBaseEnv carries the operator's own login locators to the login and
// usage children: the real home (the read is authenticated as the host
// login), PATH, NO_COLOR, and USER (the keychain login lookup needs it on
// macOS). Unrelated process credentials are never inherited. The default
// entry inherits the ambient config locators, resolving relative ones before
// the caller enters a scratch directory; the selected entry carries exactly
// one CLAUDE_CONFIG_DIR and never the ambient one, so one call can never
// answer for another login.
func loginBaseEnv(configDir string, useDefault bool) ([]string, error) {
	home, err := os.UserHomeDir()
	if err != nil || !filepath.IsAbs(home) {
		return nil, fmt.Errorf("%w: Claude host login home is unavailable", agent.ErrProviderUnavailable)
	}
	env := []string{"HOME=" + home, "PATH=" + os.Getenv("PATH"), "NO_COLOR=1"}
	// The keychain-backed login lookup on macOS needs USER; without it a
	// signed-in host reports logged-out. USER carries no credential material.
	if user, present := os.LookupEnv("USER"); present && user != "" {
		env = append(env, "USER="+user)
	}
	resolve := func(name, value string) (string, error) {
		if !filepath.IsAbs(value) {
			value, err = filepath.Abs(value)
			if err != nil {
				return "", fmt.Errorf("%w: resolve Claude host login config locator: %w", agent.ErrProviderUnavailable, err)
			}
		}
		return name + "=" + value, nil
	}
	if !useDefault {
		if strings.TrimSpace(configDir) == "" || !filepath.IsAbs(configDir) {
			return nil, fmt.Errorf("%w: Claude host login needs an absolute config directory", agent.ErrProviderUnavailable)
		}
		env = append(env, claudeConfigDirEnv+"="+configDir)
	}
	for _, name := range []string{claudeConfigDirEnv, "XDG_CONFIG_HOME"} {
		if !useDefault && name == claudeConfigDirEnv {
			continue
		}
		if value, present := os.LookupEnv(name); present && value != "" {
			entry, err := resolve(name, value)
			if err != nil {
				return nil, err
			}
			env = append(env, entry)
		}
	}
	return env, nil
}

// runLoginStatus runs one owned `auth status --json` child under ctx with the
// given locator environment and returns its exact validated stdout bytes.
// The scratch directory is removed only once its writers are proven quiet.
// Only an explicit logged-out response or the CLI's own "not logged in"
// refusal reports refused=true.
func runLoginStatus(ctx context.Context, binary string, baseEnv []string) (raw []byte, refused bool, err error) {
	probeDir, err := tempown.MkdirTemp("", tempown.PrefixClaudeLogin)
	if err != nil {
		return nil, false, fmt.Errorf("%w: create private Claude login probe: %w", agent.ErrProviderUnavailable, err)
	}
	quiescent := true
	defer func() {
		if quiescent {
			_ = os.RemoveAll(probeDir)
		}
	}()
	probeCtx, cancel := context.WithTimeout(ctx, hostLoginTimeout)
	defer cancel()
	command := exec.CommandContext(probeCtx, binary, "auth", "status", "--json")
	command.Dir = probeDir
	command.Env = append(append([]string{}, baseEnv...), "TMPDIR="+probeDir, "XDG_CACHE_HOME="+probeDir)
	command.WaitDelay = 200 * time.Millisecond
	var stdout, stderr boundedHostLoginOutput
	command.Stdout, command.Stderr = &stdout, &stderr
	quiescent, err = runOwnedModelVersionProbe(probeCtx, command)
	if !quiescent {
		return nil, false, fmt.Errorf("%w: Claude host login could not prove child quiescence: %v", agent.ErrProviderUnavailable, err)
	}
	// A cancelled or expired check never ran: it carries no verdict,
	// even when the child also wrote a refusal-shaped line or document.
	if probeCtx.Err() != nil {
		return nil, false, fmt.Errorf("%w: Claude host login: %w", agent.ErrProviderUnavailable, probeCtx.Err())
	}
	// An explicit logged-out status document answers even when the CLI
	// exits nonzero: a signed-out `auth status --json` reports
	// loggedIn:false on stdout and exits 1 with empty stderr. Trust that
	// document before the generic failure below discards it. The document
	// must carry an explicit loggedIn verdict: `null`, `{}`, or an error
	// object decodes without error yet names no login, and trusting one
	// would mint a false refusal that overwrites a positive check.
	if out := bytes.TrimSpace(stdout.buffer.Bytes()); len(out) != 0 {
		if loggedIn, authMethod, apiProvider, ok := decodeLoginStatus(out); ok {
			if !loggedIn || authMethod != "claude.ai" || apiProvider != "firstParty" {
				return nil, true, fmt.Errorf("%w: Claude runtime requires an authenticated first-party host login", agent.ErrProviderUnavailable)
			}
			if err == nil && stderr.buffer.Len() == 0 {
				return append([]byte(nil), stdout.buffer.Bytes()...), false, nil
			}
			return nil, false, fmt.Errorf("%w: Claude host login status unavailable", agent.ErrProviderUnavailable)
		}
	}
	// The CLI answers a missing login on stderr with its own refusal
	// instead of a status document; that answered refusal is the verdict.
	// Match it as a standalone line: a configuration error that merely
	// quotes a directory named "not logged in" must not count.
	if isClaudeLoginRefusal(stderr.buffer.String()) {
		return nil, true, fmt.Errorf("%w: Claude runtime requires an authenticated first-party host login", agent.ErrProviderUnavailable)
	}
	if err != nil || stderr.buffer.Len() != 0 {
		return nil, false, fmt.Errorf("%w: Claude host login status unavailable", agent.ErrProviderUnavailable)
	}
	raw = append([]byte(nil), stdout.buffer.Bytes()...)
	loggedIn, authMethod, apiProvider, ok := decodeLoginStatus(raw)
	if !ok {
		return nil, false, fmt.Errorf("%w: Claude host login status unreadable", agent.ErrProviderUnavailable)
	}
	if !loggedIn || authMethod != "claude.ai" || apiProvider != "firstParty" {
		return nil, true, fmt.Errorf("%w: Claude runtime requires an authenticated first-party host login", agent.ErrProviderUnavailable)
	}
	return raw, false, nil
}

// decodeLoginStatus validates one `auth status --json` document: it must
// be a JSON object carrying an explicit loggedIn boolean. `null`, `{}`,
// and error objects decode into the zero value without error; without
// the explicit-field check they read as logged-out and mint a false
// refusal. A document that fails validation carries no verdict.
func decodeLoginStatus(raw []byte) (loggedIn bool, authMethod, apiProvider string, ok bool) {
	var doc struct {
		LoggedIn    *bool  `json:"loggedIn"`
		AuthMethod  string `json:"authMethod"`
		APIProvider string `json:"apiProvider"`
	}
	if err := json.Unmarshal(bytes.TrimSpace(raw), &doc); err != nil || doc.LoggedIn == nil {
		return false, "", "", false
	}
	return *doc.LoggedIn, doc.AuthMethod, doc.APIProvider, true
}

// isClaudeLoginRefusal reports whether stderr carries the CLI's own
// logged-out refusal as a standalone line. A configuration error that
// merely quotes a directory (for example one named "not logged in")
// must not count: the phrase has to open the line (after an optional
// "error:" label) and continue with punctuation or whitespace, and the
// line must carry no path separator. A corrupt-config diagnostic that
// quotes an operator-selected directory containing the phrase always
// carries the directory's separators, so it can never match.
func isClaudeLoginRefusal(stderr string) bool {
	const phrase = "not logged in"
	for _, line := range strings.Split(stderr, "\n") {
		lower := strings.ToLower(strings.TrimSpace(line))
		if lower == "" {
			continue
		}
		if rest, ok := strings.CutPrefix(lower, "error:"); ok {
			lower = strings.TrimSpace(rest)
		} else if rest, ok := strings.CutPrefix(lower, "error -"); ok {
			lower = strings.TrimSpace(rest)
		}
		if lower == phrase {
			return true
		}
		if !strings.HasPrefix(lower, phrase) {
			continue
		}
		rest := lower[len(phrase):]
		if rest == "" {
			return true
		}
		// A diagnostic quoting a directory path carries its separators
		// past the phrase (for example " - archived/.claude.json: ..."
		// when the directory name itself holds a line break). A genuine
		// refusal sentence never names a path.
		if strings.ContainsAny(rest, `/\`) {
			continue
		}
		switch rest[0] {
		case ' ', '\t', '.', ',', ':', ';', '!', '?', '"', '\'', '-':
			return true
		}
	}
	return false
}
