package claude

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/RenseiAI/donmai/agent"
)

// fakeUsageCLI writes a /bin/sh script standing in for the claude
// CLI: it prints one usage_response line shaped like the SDK
// `get_usage` response, then a terminal result. argv[0] selects the
// variant: "ok" answers the usage read, "silent" emits no usage
// response, "refused" exits nonzero like a logged-out CLI.
func fakeUsageCLI(t *testing.T, variant string) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("fake CLI uses /bin/sh; skip on windows")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "fake-claude-usage.sh")
	var script string
	switch variant {
	case "ok":
		script = "#!/bin/sh\n" +
			`printf '{"type":"usage_response","rate_limits_available":true,"rate_limits":{"five_hour":{"utilization":54,"resets_at":"2026-07-18T14:39:00Z"},"seven_day":{"utilization":18.4,"resets_at":"2026-07-24T08:59:00+00:00"}}}\n'` + "\n" +
			`printf '{"type":"result","subtype":"success","is_error":false,"num_turns":1}\n'` + "\n"
	case "silent":
		script = "#!/bin/sh\n" +
			`printf '{"type":"result","subtype":"success","is_error":false,"num_turns":0}\n'` + "\n"
	case "refused":
		script = "#!/bin/sh\n" +
			`printf 'not logged in\n' >&2` + "\n" +
			"exit 1\n"
	default:
		t.Fatalf("unknown fake variant %q", variant)
	}
	if err := os.WriteFile(path, []byte(script), 0o600); err != nil {
		t.Fatalf("write fake cli: %v", err)
	}
	if err := os.Chmod(path, 0o700); err != nil { //nolint:gosec // test fixture script needs exec bit
		t.Fatalf("chmod fake cli: %v", err)
	}
	return path
}

// TestProbeUsage_MapsFakeReadThroughSharedMapper drives the
// production probe entry point (ProbeUsage) against a fake CLI: the
// usage response maps through UsageResponseToLimits onto the
// probe-stable window ids, and the account identity is opaque (never
// an address).
func TestProbeUsage_MapsFakeReadThroughSharedMapper(t *testing.T) {
	t.Parallel()

	accountID, probed, names := ProbeUsage(t.Context(), fakeUsageCLI(t, "ok"))
	if probed.Unavailable != nil {
		t.Fatalf("probed = %+v, want windows", probed.Unavailable)
	}
	if len(probed.Windows) != 2 {
		t.Fatalf("windows = %+v, want the session and weekly rows", probed.Windows)
	}
	byID := map[string]agent.UsageWindow{}
	for _, w := range probed.Windows {
		byID[w.ID] = w
	}
	if byID["five_hour"].UsedPercent != 54 || byID["seven_day"].UsedPercent != 18.4 {
		t.Errorf("windows = %+v, want 54 and 18.4 from the fake read", probed.Windows)
	}
	if names != (ScopedLimitNames{}) {
		t.Errorf("scoped names = %+v, want none (the fake read names no scoped bucket)", names)
	}
	if accountID == "" {
		t.Fatal("account id is empty; the probe must name the account it read")
	}
	for _, leak := range []string{"@", ".", "example", "test"} {
		if strings.Contains(strings.ToLower(accountID), leak) {
			t.Errorf("account id %q looks like an address, want an opaque value", accountID)
		}
	}
}

// TestProbeUsage_SilentReadIsAnUnansweredFailure pins the unreachable
// posture: a CLI that answers without a usage response yields a
// probe-failed snapshot that carries no login verdict.
func TestProbeUsage_SilentReadIsAnUnansweredFailure(t *testing.T) {
	t.Parallel()

	accountID, probed, _ := ProbeUsage(t.Context(), fakeUsageCLI(t, "silent"))
	if probed.Unavailable == nil || probed.Unavailable.Reason != agent.UsageUnavailableProbeFailed {
		t.Fatalf("probed = %+v, want probeFailed", probed.Unavailable)
	}
	if probed.Unavailable.Answered {
		t.Errorf("silent read marked answered; a read that never reached usage carries no verdict")
	}
	if accountID != "" {
		t.Errorf("account id = %q, want empty when the read produced nothing", accountID)
	}
}

// TestProbeUsage_EmptyBinaryIsAnUnansweredFailure covers the
// not-installed host: no CLI, no read, no verdict.
func TestProbeUsage_EmptyBinaryIsAnUnansweredFailure(t *testing.T) {
	t.Parallel()

	_, probed, _ := ProbeUsage(t.Context(), "")
	if probed.Unavailable == nil || probed.Unavailable.Reason != agent.UsageUnavailableProbeFailed {
		t.Fatalf("probed = %+v, want probeFailed", probed.Unavailable)
	}
	if probed.Unavailable.Answered {
		t.Errorf("missing binary marked answered; nothing ran to refuse")
	}
}

// TestScanUsageResponse_PicksFirstUsageLine pins the scanner: the
// first usage_response line wins, and anything else decodes to no
// response.
func TestScanUsageResponse_PicksFirstUsageLine(t *testing.T) {
	t.Parallel()

	raw := []byte("{\"type\":\"assistant\",\"message\":{}}\n" +
		"{\"type\":\"usage_response\",\"rate_limits_available\":true,\"rate_limits\":{}}\n" +
		"{\"type\":\"usage_response\",\"rate_limits_available\":false}\n")
	got, ok := scanUsageResponse(raw)
	if !ok {
		t.Fatal("no usage response found")
	}
	if !strings.Contains(string(got), `"rate_limits_available":true`) {
		t.Errorf("response = %s, want the first usage_response line", got)
	}
	if _, ok := scanUsageResponse([]byte("{\"type\":\"result\"}\nnot json\n")); ok {
		t.Error("non-usage output decoded as a usage response")
	}
}
