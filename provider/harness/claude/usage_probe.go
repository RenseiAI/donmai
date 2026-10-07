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

// usageProbePrompt is the stdin prompt a usage-probe session sends. It
// asks for no model work: the child answers the usage response and
// ends its turn without spending one, so the probe costs no quota.
const usageProbePrompt = "Reply with exactly: ok"

// ProbeUsage runs one claude usage read: a short-lived CLI session in
// an isolated probe directory whose stream-json output is scanned for
// the usage response, mapped through the shared UsageResponseToLimits
// mapper so the rows carry the same window ids a streamed
// `rate_limit_event` lands on. It returns the opaque account identity
// the login check names, the mapped limits, and the scoped-bucket
// names the response carried for the event mapper to reuse.
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
	probeDir, err := os.MkdirTemp("", "donmai-claude-usage-")
	if err != nil {
		return failed()
	}
	defer func() { _ = os.RemoveAll(probeDir) }()
	if ctx == nil {
		ctx = context.Background()
	}
	probeCtx, cancel := context.WithTimeout(ctx, UsageProbeTimeout)
	defer cancel()
	// nolint:gosec // binary is the resolved CLI path the provider probed at construction.
	command := exec.CommandContext(probeCtx, binary,
		"-p", "--output-format", "stream-json", "--verbose",
		"--max-turns", "1", "--model", "haiku")
	command.Dir = probeDir
	command.Env = []string{"HOME=" + probeDir, "PATH=" + os.Getenv("PATH"), "NO_COLOR=1", "TMPDIR=" + probeDir, "XDG_CACHE_HOME=" + probeDir}
	command.Stdin = strings.NewReader(usageProbePrompt)
	var stdout boundedHostLoginOutput
	command.Stdout = &stdout
	command.Stderr = &boundedHostLoginOutput{}
	if err := command.Run(); err != nil {
		out := agent.MakeUnavailableUsageLimits(checkedAt, agent.UsageUnavailableProbeFailed, "Claude did not answer the usage request.")
		if loginRefused(probeCtx, binary) {
			out.Unavailable.Answered = true
		}
		return "", out, ScopedLimitNames{}
	}
	raw, ok := scanUsageResponse(stdout.buffer.Bytes())
	if !ok {
		return failed()
	}
	limits, scoped := UsageResponseToLimits(raw, checkedAt)
	return loginAccountID(probeCtx, binary), limits, scoped
}

// scanUsageResponse returns the first stream-json line whose type is
// "usage_response". The usage read reports every window at once as
// 0–100 percentages with ISO reset times — the same shape
// UsageResponseToLimits maps.
func scanUsageResponse(output []byte) (json.RawMessage, bool) {
	for _, line := range bytes.Split(output, []byte("\n")) {
		line = bytes.TrimSpace(line)
		if len(line) == 0 {
			continue
		}
		var head struct {
			Type string `json:"type"`
		}
		if err := json.Unmarshal(line, &head); err != nil || head.Type != "usage_response" {
			continue
		}
		return json.RawMessage(append([]byte(nil), line...)), true
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
	checkCtx, cancel := context.WithTimeout(ctx, hostLoginTimeout)
	defer cancel()
	return CheckHostSessionLogin(checkCtx, binary) != nil && checkCtx.Err() == nil
}

// loginAccountID names the probed account opaquely: a stable hash of
// the login response. The raw response carries the operator's address
// and organization name; only the hash leaves this function, so the
// quota entry the platform hashes never carries who signed in.
func loginAccountID(ctx context.Context, binary string) string {
	if strings.TrimSpace(binary) == "" {
		return ""
	}
	if ctx == nil {
		ctx = context.Background()
	}
	checkCtx, cancel := context.WithTimeout(ctx, hostLoginTimeout)
	defer cancel()
	// nolint:gosec // binary is the resolved CLI path the provider probed at construction.
	command := exec.CommandContext(checkCtx, binary, "auth", "status", "--json")
	command.Env = []string{"HOME=" + os.Getenv("HOME"), "PATH=" + os.Getenv("PATH"), "NO_COLOR=1"}
	var stdout boundedHostLoginOutput
	command.Stdout = &stdout
	command.Stderr = &boundedHostLoginOutput{}
	if err := command.Run(); err != nil {
		return ""
	}
	sum := sha256.Sum256(stdout.buffer.Bytes())
	return "claude-" + hex.EncodeToString(sum[:])
}
