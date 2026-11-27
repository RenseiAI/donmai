package claude

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/RenseiAI/donmai/agent"
	"github.com/RenseiAI/donmai/tempown"
)

// ProbeUsage runs one claude usage read: a short-lived CLI session in
// stream-json mode that answers a single `get_usage` control request.
// The control answer carries the same usage shape UsageResponseToLimits
// maps, so the rows carry the same window ids a streamed
// `rate_limit_event` lands on. It returns the opaque account identity
// the login check names, the mapped limits, and the scoped-bucket
// names the response carried for the event mapper to reuse.
//
// The probe sends no user message and performs no model turn: after
// the `initialize` line it sends one control request and closes stdin,
// so the child answers and exits without spending quota. The child runs
// detached from the operator's hooks, MCP servers and session store
// (safe mode, strict MCP config, no session persistence), so a probe
// leaves no hook side effect and no session file behind.
//
// The child inherits the operator's real home and config locators —
// the usage read is authenticated as the host login, and an isolated
// home would have no login to read. USER is inherited too: the login
// needs it to locate the keychain item on macOS.
//
// A read that never produced a usage response is a failed probe,
// never an error: the last good windows stay published and no login
// verdict is recorded. Only the login check's own refusal marks the
// failure as answered. The account identity never carries an address:
// a stable hash of the login response, so the platform can correlate
// the entry without ever seeing who signed in.
func ProbeUsage(ctx context.Context, binary string) (accountID string, probed agent.UsageLimits, names ScopedLimitNames) {
	return probeUsage(ctx, binary, "", true)
}

// ProbeUsageForConfigDir reads one caller-selected login's usage and
// identity under the caller's deadline: the usage child and the single
// owned `auth status --json` read behind it both run with exactly one
// CLAUDE_CONFIG_DIR, so the windows and digest answer for the selected home
// and never the ambient login. configDir must be absolute; an empty or
// relative selection fails unanswered before any child launches.
func ProbeUsageForConfigDir(ctx context.Context, binary, configDir string) (accountID string, probed agent.UsageLimits, names ScopedLimitNames) {
	return probeUsage(ctx, binary, configDir, false)
}

// DefaultProbeConfigDir reports the default config directory the usage
// probe reads: CLAUDE_CONFIG_DIR, or ~/.claude under the host home. Empty
// when no absolute default can be resolved.
func DefaultProbeConfigDir() string {
	return interactiveUsageConfigDir(nil)
}

// selectedConfigDirReadable reports whether a selected config directory
// exists and can still be read. A home whose permissions were revoked
// still stats but answers no CLI read, and the status child may then
// report it logged out; such a home must stay an unanswered attempt,
// never an answered refusal. Permission bits are checked explicitly so a
// revoked home reads as unreadable even to a privileged reader.
func selectedConfigDirReadable(dir string) bool {
	info, err := os.Stat(dir)
	if err != nil || !info.IsDir() {
		return false
	}
	if info.Mode().Perm()&0o555 == 0 {
		return false
	}
	// nolint:gosec // dir is the caller-selected config home, already validated absolute.
	f, err := os.Open(dir)
	if err != nil {
		return false
	}
	defer func() { _ = f.Close() }()
	if _, err := f.Readdirnames(1); err != nil && !errors.Is(err, io.EOF) {
		return false
	}
	return true
}

// probeUsage is the shared implementation behind both entries: the
// default entry selects the host default locators, the selected entry
// pins one absolute config directory. The whole attempt derives from
// the caller under UsageProbeTimeout, and the status read behind the
// identity and the refusal is capped at hostLoginTimeout inside it.
func probeUsage(ctx context.Context, binary, configDir string, useDefault bool) (accountID string, probed agent.UsageLimits, names ScopedLimitNames) {
	now := time.Now()
	checkedAt := agent.ISOTime(now)
	failed := func() (string, agent.UsageLimits, ScopedLimitNames) {
		return "", agent.MakeUnavailableUsageLimits(checkedAt, agent.UsageUnavailableProbeFailed, "Claude did not answer the usage request."), ScopedLimitNames{}
	}
	if strings.TrimSpace(binary) == "" {
		return failed()
	}
	if ctx == nil {
		ctx = context.Background()
	}
	// A selected home that is not resolved and absolute is a caller bug,
	// not a login verdict: fail before launching any child.
	if !useDefault && (strings.TrimSpace(configDir) == "" || !filepath.IsAbs(configDir) || !filepath.IsAbs(binary)) {
		return failed()
	}
	// A selected home that is missing or unreadable answers no read: the
	// status child would report it logged out, which is not a refusal of
	// the selected login. Fail unanswered before launching any child.
	if !useDefault && !selectedConfigDirReadable(configDir) {
		return failed()
	}
	baseEnv, err := loginBaseEnv(configDir, useDefault)
	if err != nil {
		return failed()
	}
	probeCtx, cancel := context.WithTimeout(ctx, UsageProbeTimeout)
	defer cancel()
	raw, ok := readUsageControl(probeCtx, binary, baseEnv)
	if !ok {
		out := agent.MakeUnavailableUsageLimits(checkedAt, agent.UsageUnavailableProbeFailed, "Claude did not answer the usage request.")
		if loginRefused(probeCtx, binary, baseEnv) {
			out.Unavailable.Answered = true
		}
		return "", out, ScopedLimitNames{}
	}
	limits, scoped := UsageResponseToLimits(raw, checkedAt)
	return loginAccountID(probeCtx, binary, baseEnv), limits, scoped
}

// usageControlRequest is the single control request the probe sends
// after initialize. skip_behaviors keeps the answer to usage data.
const usageControlRequest = `{"type":"control_request","request":{"subtype":"get_usage","skip_behaviors":true}}`

// usageProbeStdin is the whole stdin the probe feeds the child: the
// initialize line, then the one control request, then EOF. No user
// message is ever sent, so no model turn runs.
func usageProbeStdin(cwd string) string {
	init, _ := json.Marshal(map[string]any{
		"type":     "initialize",
		"cwd":      cwd,
		"encoding": "utf-8",
	})
	return string(init) + "\n" + usageControlRequest + "\n"
}

// probeUsageArgv is the CLI invocation for a usage read. Every flag is
// load-bearing:
//
//   - -p with stream-json input/output frames the session so the child
//     accepts the initialize + control_request lines on stdin.
//   - --strict-mcp-config with no --mcp-config starts no MCP server.
//   - --no-session-persistence writes no session file for the probe.
//   - --safe-mode detaches the operator's hooks, plugins, CLAUDE.md
//     chain and custom settings, so a probe every 5 minutes fires no
//     SessionStart hook and reads no project state; auth, model
//     selection and built-ins keep working. --bare would instead refuse
//     to read the subscription login, so it must never be used here.
//
// No prompt, model or turn flag is passed: the answer comes from the
// control request, not from a turn.
var probeUsageArgv = []string{
	"-p",
	"--input-format", "stream-json",
	"--output-format", "stream-json",
	"--verbose",
	"--strict-mcp-config",
	"--no-session-persistence",
	"--safe-mode",
}

// usageProbeOutputMax bounds the stream-json output the probe reads.
// The control answer is a few kilobytes; anything past this is a
// runaway child, not a usage read.
const usageProbeOutputMax = 1 << 20

// boundedProbeOutput retains at most usageProbeOutputMax bytes of child
// output. Overflow is swallowed, never reported: the scan only needs
// the control answer, and a bound failure must not become a verdict.
type boundedProbeOutput struct{ buffer bytes.Buffer }

func (b *boundedProbeOutput) Write(data []byte) (int, error) {
	remaining := usageProbeOutputMax - b.buffer.Len()
	if remaining > 0 {
		if len(data) > remaining {
			data = data[:remaining]
		}
		b.buffer.Write(data)
	}
	return len(data), nil
}

// readUsageControl runs the usage child to its control answer and
// returns the inner usage body: the `response.response` object of the
// `control_response` line, which carries the rate_limits shape
// UsageResponseToLimits maps. The child inherits the given locator
// environment, so it answers as the same login the status read names.
// It reports false when the child never produced one — a missing
// binary, a timeout, an unreadable reply — which is never a login
// verdict by itself.
func readUsageControl(ctx context.Context, binary string, baseEnv []string) (json.RawMessage, bool) {
	probeDir, err := tempown.MkdirTemp("", tempown.PrefixClaudeUsage)
	if err != nil {
		return nil, false
	}
	defer func() { _ = os.RemoveAll(probeDir) }()
	// nolint:gosec // binary is the resolved CLI path the provider probed at construction.
	command := exec.CommandContext(ctx, binary, probeUsageArgv...)
	command.Dir = probeDir
	command.Env = baseEnv
	command.Stdin = strings.NewReader(usageProbeStdin(probeDir))
	// A hung child (a usage read that never answers) must not hold
	// the probe past the context: kill the process group on expiry so
	// Run returns instead of waiting out the child's own sleep.
	command.WaitDelay = 200 * time.Millisecond
	configureProbeProcessGroup(command)
	var stdout boundedProbeOutput
	command.Stdout = &stdout
	command.Stderr = &boundedProbeOutput{}
	_ = command.Run()
	return scanUsageControl(stdout.buffer.Bytes())
}

// scanUsageControl returns the usage body of the first
// `control_response` line whose inner subtype is "success". Other
// lines — hook events, init, error answers — are skipped, and an
// error-subtype answer decodes to no response: a refused read is for
// the login check to classify, never for the scanner to upgrade.
func scanUsageControl(output []byte) (json.RawMessage, bool) {
	for _, line := range bytes.Split(output, []byte("\n")) {
		line = bytes.TrimSpace(line)
		if len(line) == 0 {
			continue
		}
		var envelope struct {
			Type     string `json:"type"`
			Response *struct {
				Subtype  string          `json:"subtype"`
				Response json.RawMessage `json:"response"`
			} `json:"response"`
		}
		if err := json.Unmarshal(line, &envelope); err != nil {
			continue
		}
		if envelope.Type != "control_response" || envelope.Response == nil {
			continue
		}
		if envelope.Response.Subtype != "success" || len(envelope.Response.Response) == 0 {
			continue
		}
		return json.RawMessage(append([]byte(nil), envelope.Response.Response...)), true
	}
	return nil, false
}

// loginRefused reports whether the selected login answers with a refusal:
// an explicit logged-out response or the CLI's own "not logged in". A
// check that never ran is not a refusal.
func loginRefused(ctx context.Context, binary string, baseEnv []string) bool {
	if ctx == nil {
		ctx = context.Background()
	}
	// A probe context that already expired (a usage read that timed
	// out) never ran a check: fail fast instead of spending another
	// login timeout proving it, and never mistake the expiry for a
	// refusal.
	if ctx.Err() != nil {
		return false
	}
	checkCtx, cancel := context.WithTimeout(ctx, hostLoginTimeout)
	defer cancel()
	_, refused, _ := runLoginStatus(checkCtx, binary, baseEnv)
	return refused && checkCtx.Err() == nil
}

// loginAccountID names the probed account opaquely: a stable hash of the
// one owned login response. Only the hash leaves this function. A refused
// status names no account, so logged-out hosts never merge into one entry.
func loginAccountID(ctx context.Context, binary string, baseEnv []string) string {
	if strings.TrimSpace(binary) == "" {
		return ""
	}
	if ctx == nil {
		ctx = context.Background()
	}
	statusCtx, cancel := context.WithTimeout(ctx, hostLoginTimeout)
	defer cancel()
	raw, _, err := runLoginStatus(statusCtx, binary, baseEnv)
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(raw)
	return "claude-" + hex.EncodeToString(sum[:])
}
