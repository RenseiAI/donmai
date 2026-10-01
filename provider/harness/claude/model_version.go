package claude

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/RenseiAI/donmai/agent"
)

const (
	sonnet5ModelID        = "claude-sonnet-5"
	sonnet5MinimumVersion = "2.1.197"
	modelVersionTimeout   = 3 * time.Second
	modelVersionOutputMax = 128
)

// Claude Code normally prints "X.Y.Z (Claude Code)". A bare X.Y.Z is also
// accepted. Extra lines, warnings, prereleases, and build suffixes are
// unverified and refuse Sonnet 5 rather than being guessed into a version.
var modelVersionLine = regexp.MustCompile(`^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(?: \(Claude Code\))?$`)

var errModelVersionOutputTooLarge = errors.New("version output exceeds bound")

// Keep the buffer named so io.Copy cannot bypass Write through promoted ReadFrom.
type boundedModelVersionOutput struct{ buffer bytes.Buffer }

func (b *boundedModelVersionOutput) Len() int       { return b.buffer.Len() }
func (b *boundedModelVersionOutput) String() string { return b.buffer.String() }

func (b *boundedModelVersionOutput) Write(p []byte) (int, error) {
	if len(p) > modelVersionOutputMax-b.Len() {
		return 0, errModelVersionOutputTooLarge
	}
	return b.buffer.Write(p)
}

// CheckModelBinaryVersion enforces only the documented Sonnet 5 CLI floor.
// Other models retain their existing behavior. This checks the native CLI's
// own version; it does not attest login, account entitlement, or a model turn.
func CheckModelBinaryVersion(ctx context.Context, binary, model string) error {
	if model != sonnet5ModelID {
		return nil
	}
	if ctx == nil || !filepath.IsAbs(binary) {
		return fmt.Errorf("%w: Claude Code Sonnet 5 needs an absolute CLI path and context", agent.ErrProviderUnavailable)
	}
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("%w: Claude Code version probe: %w", agent.ErrProviderUnavailable, err)
	}

	// An empty, private cwd and home keep repository config and ambient
	// credentials out of this read-only --version probe. PATH is retained for
	// installed script wrappers that resolve their interpreter through env.
	probeDir, err := os.MkdirTemp("", "donmai-claude-version-")
	if err != nil {
		return fmt.Errorf("%w: create isolated Claude Code probe directory: %w", agent.ErrProviderUnavailable, err)
	}
	quiescent := true
	defer func() {
		if quiescent {
			_ = os.RemoveAll(probeDir)
		}
	}()
	probeCtx, cancel := context.WithTimeout(ctx, modelVersionTimeout)
	defer cancel()
	cmd := exec.CommandContext(probeCtx, binary, "--version")
	cmd.Dir = probeDir
	cmd.Env = []string{
		"HOME=" + probeDir,
		"TMPDIR=" + probeDir,
		"XDG_CONFIG_HOME=" + probeDir,
		"XDG_CACHE_HOME=" + probeDir,
		"PATH=" + os.Getenv("PATH"),
	}
	cmd.WaitDelay = 200 * time.Millisecond
	var stdout, stderr boundedModelVersionOutput
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	var runErr error
	quiescent, runErr = runOwnedModelVersionProbe(probeCtx, cmd)
	if !quiescent {
		return fmt.Errorf("%w: Claude Code version probe could not prove child quiescence: %v", agent.ErrProviderUnavailable, runErr)
	}
	if runErr != nil {
		if ctxErr := probeCtx.Err(); ctxErr != nil {
			return fmt.Errorf("%w: Claude Code version probe: %w", agent.ErrProviderUnavailable, ctxErr)
		}
		return fmt.Errorf("%w: Claude Code version probe failed: %v", agent.ErrProviderUnavailable, runErr)
	}
	if stderr.Len() != 0 {
		return fmt.Errorf("%w: Claude Code version probe printed unexpected diagnostics", agent.ErrProviderUnavailable)
	}
	line := strings.TrimSuffix(stdout.String(), "\n")
	line = strings.TrimSuffix(line, "\r")
	parts := modelVersionLine.FindStringSubmatch(line)
	if parts == nil {
		return fmt.Errorf("%w: Claude Code version probe returned an unrecognized stable version", agent.ErrProviderUnavailable)
	}
	var version [3]uint64
	for i := range version {
		version[i], err = strconv.ParseUint(parts[i+1], 10, 32)
		if err != nil {
			return fmt.Errorf("%w: Claude Code version component is invalid", agent.ErrProviderUnavailable)
		}
	}
	if version[0] < 2 || version[0] == 2 && version[1] < 1 || version[0] == 2 && version[1] == 1 && version[2] < 197 {
		return fmt.Errorf("%w: Claude Code %s requires CLI >= %s", agent.ErrProviderUnavailable, sonnet5ModelID, sonnet5MinimumVersion)
	}
	return nil
}
