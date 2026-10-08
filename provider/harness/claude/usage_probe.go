package claude

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/RenseiAI/donmai/agent"
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
	probeCtx, cancel := context.WithTimeout(ctx, UsageProbeTimeout)
	defer cancel()
	raw, ok := readUsageControl(probeCtx, binary)
	if !ok {
		out := agent.MakeUnavailableUsageLimits(checkedAt, agent.UsageUnavailableProbeFailed, "Claude did not answer the usage request.")
		if loginRefused(probeCtx, binary) {
			out.Unavailable.Answered = true
		}
		return "", out, ScopedLimitNames{}
	}
	limits, scoped := UsageResponseToLimits(raw, checkedAt)
	return loginAccountID(probeCtx, binary), limits, scoped
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

// probeUsageEnv carries the operator's own login locators to the usage
// child: the real home (the read is authenticated as the host login),
// its config overrides, and USER (the keychain login lookup needs it
// on macOS). Unrelated process credentials are never inherited: only
// these names cross, never tokens or keys.
func probeUsageEnv() []string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		home = os.Getenv("HOME")
	}
	env := []string{"HOME=" + home, "PATH=" + os.Getenv("PATH"), "NO_COLOR=1"}
	if user, present := os.LookupEnv("USER"); present && user != "" {
		env = append(env, "USER="+user)
	}
	for _, name := range []string{"CLAUDE_CONFIG_DIR", "XDG_CONFIG_HOME"} {
		if value, present := os.LookupEnv(name); present && value != "" {
			env = append(env, name+"="+value)
		}
	}
	return env
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
// UsageResponseToLimits maps. It reports false when the child never
// produced one — a missing binary, a timeout, an unreadable reply —
// which is never a login verdict by itself.
func readUsageControl(ctx context.Context, binary string) (json.RawMessage, bool) {
	probeDir, err := os.MkdirTemp("", "donmai-claude-usage-")
	if err != nil {
		return nil, false
	}
	defer func() { _ = os.RemoveAll(probeDir) }()
	// nolint:gosec // binary is the resolved CLI path the provider probed at construction.
	command := exec.CommandContext(ctx, binary, probeUsageArgv...)
	command.Dir = probeDir
	command.Env = probeUsageEnv()
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

// loginRefused reports whether the host login check answers with a
// refusal: the CLI is installed and runs, but no valid login backs
// it. A check that never ran (missing binary, timeout, unreadable
// reply) is not a refusal.
func loginRefused(ctx context.Context, binary string) bool {
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
	return CheckHostSessionLogin(checkCtx, binary) != nil && checkCtx.Err() == nil
}

// loginAccountID names the probed account opaquely: a stable hash of
// the login response. The raw response carries the operator's address
// and organization name; only the hash leaves this function, so the
// quota entry the platform hashes never carries who signed in.
// A status the login check itself refuses names no account: hashing a
// refusal would mint a stable id for "nobody signed in" and merge
// every logged-out host into one phantom account.
func loginAccountID(ctx context.Context, binary string) string {
	if strings.TrimSpace(binary) == "" {
		return ""
	}
	if ctx == nil {
		ctx = context.Background()
	}
	checkCtx, cancel := context.WithTimeout(ctx, hostLoginTimeout)
	defer cancel()
	if CheckHostSessionLogin(checkCtx, binary) != nil {
		return ""
	}
	statusCtx, cancel := context.WithTimeout(ctx, hostLoginTimeout)
	defer cancel()
	// nolint:gosec // binary is the resolved CLI path the provider probed at construction.
	command := exec.CommandContext(statusCtx, binary, "auth", "status", "--json")
	command.Env = loginStatusEnv()
	var stdout boundedHostLoginOutput
	command.Stdout = &stdout
	command.Stderr = &boundedHostLoginOutput{}
	if err := command.Run(); err != nil {
		return ""
	}
	sum := sha256.Sum256(stdout.buffer.Bytes())
	return "claude-" + hex.EncodeToString(sum[:])
}

// loginStatusEnv carries the same login locators as the usage child
// (real home, config overrides, USER) to the status child, so the two
// agree on what login backs the host. See probeUsageEnv.
func loginStatusEnv() []string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		home = os.Getenv("HOME")
	}
	env := []string{"HOME=" + home, "PATH=" + os.Getenv("PATH"), "NO_COLOR=1"}
	if user, present := os.LookupEnv("USER"); present && user != "" {
		env = append(env, "USER="+user)
	}
	for _, name := range []string{"CLAUDE_CONFIG_DIR", "XDG_CONFIG_HOME"} {
		if value, present := os.LookupEnv(name); present && value != "" {
			env = append(env, name+"="+value)
		}
	}
	return env
}
