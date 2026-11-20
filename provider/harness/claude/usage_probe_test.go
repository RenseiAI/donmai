package claude

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/RenseiAI/donmai/agent"
)

// fakeUsageCLI writes a /bin/sh script standing in for the claude
// CLI. It speaks the probing protocol the production probe drives:
// argv carries the probe argv, stdin carries the initialize line and
// the get_usage control request, and stdout carries stream-json lines.
// The variant selects the answer: "ok" answers the control request
// with a usage body shaped like the real CLI's get_usage answer, then
// a terminal exit; "silent" answers nothing; "refused" exits nonzero
// like a CLI whose login the control request rejected.
//
// The scripts assert the production argv: a probe that regresses to
// sending a user prompt (a model turn every 5 minutes) or that drops
// the safe-mode/strict/persistence flags fails the fixture instead of
// silently spending quota or firing hooks.
func fakeUsageCLI(t *testing.T, variant string) string {
	t.Helper()
	return writeFakeUsageCLI(t, variant)
}

// writeFakeUsageCLI writes the fake CLI through the shared dispatcher
// (see writeFakeCLI in oneshot_test.go): the per-test path is a hard
// link to an immutable dispatcher whose sidecar carries the script,
// so an inherited writer can never make execve fail with ETXTBSY.
func writeFakeUsageCLI(t *testing.T, variant string) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("fake CLI uses /bin/sh; skip on windows")
	}
	if fakeCLIDispatcherErr != "" {
		t.Fatalf("prepare fake CLI dispatcher: %s", fakeCLIDispatcherErr)
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "fake-claude-usage.sh")
	usageBody := `{"subscription_type":"max","rate_limits_available":true,"rate_limits":{"five_hour":{"utilization":54,"resets_at":"2026-07-18T14:39:00Z"},"seven_day":{"utilization":18.4,"resets_at":"2026-07-24T08:59:00+00:00"}}}`
	// The login-status branch answers the probe's `auth status --json`
	// children on a signed-in host: the status child reports its login
	// (the account-id read hashes it; the refusal check accepts it).
	// The "silent" and "refused" fixtures model hosts where the
	// status child sees no login — under the test HOME it refuses — so
	// they carry no auth branch and fall through to their answer below.
	// The auth branch matches on "$*" (the full command line), not
	// $1: the dispatcher execs the fixture as `sh <sidecar> <args>`,
	// so $1 is empty and the subcommand rides in $*.
	authBranch := "case \" $* \" in *\" auth status --json \"*)\n" +
		"  case \"$USER\" in \"\") echo 'status child has no USER' >&2; exit 27;; esac\n" +
		`  printf '{"loggedIn":true,"authMethod":"claude.ai","apiProvider":"firstParty"}\n'` + "\n" +
		"  exit 0;;\n" +
		"esac\n"
	var script string
	switch variant {
	case "ok":
		// The auth branch comes first: the probe's `auth status`
		// children carry no probe flags, so the argv assertions below
		// must not see them. The status child must also see the login
		// the probe promises: without USER the keychain lookup misses,
		// the status child refuses, and the account stays unnamed.
		// The guard lives inside the auth branch so the usage path
		// still answers when USER is missing — only the account id is
		// lost, exactly the macOS failure this pins.
		script = "#!/bin/sh\n" + authBranch +
			"case \" $* \" in *\" --safe-mode \"*) ;; *) echo 'missing --safe-mode' >&2; exit 21;; esac\n" +
			"case \" $* \" in *\" --strict-mcp-config \"*) ;; *) echo 'missing --strict-mcp-config' >&2; exit 22;; esac\n" +
			"case \" $* \" in *\" --no-session-persistence \"*) ;; *) echo 'missing --no-session-persistence' >&2; exit 23;; esac\n" +
			"case \" $* \" in *\" --input-format stream-json \"*) ;; *) echo 'missing stream-json input' >&2; exit 24;; esac\n" +
			"input=$(cat)\n" +
			"case \"$input\" in *'\"subtype\":\"get_usage\"'*) ;; *) echo 'no get_usage control request' >&2; exit 25;; esac\n" +
			// A user prompt on stdin would mean the probe sends a model
			// turn: fail the fixture loudly rather than spending one.
			"case \"$input\" in *'\"type\":\"user\"'*) echo 'probe sent a user turn' >&2; exit 26;; esac\n" +
			`printf '{"type":"control_response","response":{"subtype":"success","response":` + usageBody + `}}\n'` + "\n" +
			"exit 0\n"
	case "silent":
		// A signed-in host whose control answer carries no usage
		// body: the status child reports the login (so the probe can
		// name the account), and the empty control answer yields a
		// probe-failed snapshot with no verdict of its own. This is
		// the real CLI's shape when the account answers but has no
		// usage to report.
		script = "#!/bin/sh\n" + authBranch +
			`printf '{"type":"system","subtype":"init"}\n'` + "\n" +
			"exit 0\n"
	case "refused":
		script = "#!/bin/sh\n" +
			`printf 'not logged in\n' >&2` + "\n" +
			"exit 1\n"
	default:
		t.Fatalf("unknown fake variant %q", variant)
	}
	if err := writeFakeCLIFile(path+".fixture", script); err != nil {
		t.Fatalf("write fake cli fixture: %v", err)
	}
	if err := os.Link(fakeCLIDispatcher, path); err != nil { //nolint:gosec // atomically publishes a hard-linked test fixture
		t.Fatalf("publish fake cli dispatcher: %v", err)
	}
	return path
}

// TestProbeUsage_MapsFakeReadThroughSharedMapper drives the
// production probe entry point (ProbeUsage) against a fake CLI: the
// control answer maps through UsageResponseToLimits onto the
// probe-stable window ids, and the account identity is opaque (never
// an address).
func TestProbeUsage_MapsFakeReadThroughSharedMapper(t *testing.T) {
	// The fake status child answers only when USER reaches it (the
	// macOS keychain rule the probe pins). Set it here rather than
	// inherit it: containers and service managers can start the test
	// binary without USER, and the account id must not depend on that.
	t.Setenv("USER", "probe-operator")

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

// TestProbeUsage_RefusalIsAnswered pins the logged-out posture:
// a CLI that refuses the usage read yields a probe-failed snapshot
// marked answered, so the daemon records ok:false beside the last
// good windows.
func TestProbeUsage_RefusalIsAnswered(t *testing.T) {
	t.Parallel()

	accountID, probed, _ := ProbeUsage(t.Context(), fakeUsageCLI(t, "refused"))
	if probed.Unavailable == nil || probed.Unavailable.Reason != agent.UsageUnavailableProbeFailed {
		t.Fatalf("probed = %+v, want probeFailed", probed.Unavailable)
	}
	if !probed.Unavailable.Answered {
		t.Error("refusal not marked answered; the usage read refused the probe")
	}
	if accountID != "" {
		t.Errorf("account id = %q, want empty when the read produced nothing", accountID)
	}
}

// TestProbeUsage_UnsupportedReadIsAnswered pins the signed-out shape
// the real CLI answers when no login backs it: a success-subtype
// control answer whose rate_limits_available is false maps to the
// unsupported snapshot (an answered verdict, ok:false with no rows),
// never to a silent failure.
func TestProbeUsage_UnsupportedReadIsAnswered(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "fake-claude-usage.sh")
	script := "#!/bin/sh\n" +
		`printf '{"type":"control_response","response":{"subtype":"success","response":{"subscription_type":null,"rate_limits_available":false,"rate_limits":null}}}\n'` + "\n" +
		"exit 0\n"
	// Publish through the immutable dispatcher, never as a freshly
	// written executable: a sibling test that forks while this file's
	// writer is open hands the writer to its child, and execve of the
	// script then fails with ETXTBSY. The failed read fell to the
	// refusal check and reported probeFailed instead of unsupported.
	if fakeCLIDispatcherErr != "" {
		t.Fatalf("prepare fake CLI dispatcher: %s", fakeCLIDispatcherErr)
	}
	if err := writeFakeCLIFile(path+".fixture", script); err != nil {
		t.Fatalf("write fake cli fixture: %v", err)
	}
	if err := os.Link(fakeCLIDispatcher, path); err != nil { //nolint:gosec // atomically publishes a hard-linked test fixture
		t.Fatalf("publish fake cli dispatcher: %v", err)
	}
	_, probed, _ := ProbeUsage(t.Context(), path)
	if probed.Unavailable == nil || probed.Unavailable.Reason != agent.UsageUnavailableUnsupported {
		t.Fatalf("probed = %+v, want unsupported", probed.Unavailable)
	}
}

// TestProbeUsage_HangingReadIsAnUnansweredFailure pins the unreachable
// posture at the production entry point: a CLI that never answers the
// control request (probe timeout expires) yields a probe-failed
// snapshot that carries no login verdict, because the refusal check
// runs against the same expired context and can never mistake the
// timeout for a refusal.
func TestProbeUsage_HangingReadIsAnUnansweredFailure(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "fake-claude-usage.sh")
	script := "#!/bin/sh\n" +
		"sleep 30\n" +
		"exit 0\n"
	if err := writeFakeCLIFile(path+".fixture", script); err != nil {
		t.Fatalf("write fake cli fixture: %v", err)
	}
	if err := os.Link(fakeCLIDispatcher, path); err != nil { //nolint:gosec // atomically publishes a hard-linked test fixture
		t.Fatalf("publish fake cli dispatcher: %v", err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	accountID, probed, _ := ProbeUsage(ctx, path)
	if probed.Unavailable == nil || probed.Unavailable.Reason != agent.UsageUnavailableProbeFailed {
		t.Fatalf("probed = %+v, want probeFailed", probed.Unavailable)
	}
	if probed.Unavailable.Answered {
		t.Errorf("timed-out read marked answered; a read that never answered carries no verdict")
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

// TestLoginRefused_FalseOnCancelledContext pins the conjunct that
// keeps an unreachable probe unanswered: a login check that never
// ran because its context was already expired is not a refusal, so
// the daemon records no new verdict instead of ok:false.
func TestLoginRefused_FalseOnCancelledContext(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if loginRefused(ctx, "/nonexistent-claude-binary") {
		t.Error("loginRefused = true on a cancelled context; an expired check never answered")
	}
}

// TestScanUsageControl_PicksFirstSuccessAnswer pins the scanner: the
// first success-subtype control answer wins; hook lines, init lines,
// error answers and non-JSON noise decode to no response.
func TestScanUsageControl_PicksFirstSuccessAnswer(t *testing.T) {
	t.Parallel()

	raw := []byte("{\"type\":\"system\",\"subtype\":\"hook_started\"}\n" +
		"not json\n" +
		"{\"type\":\"control_response\",\"response\":{\"subtype\":\"error\",\"response\":{}}}\n" +
		"{\"type\":\"control_response\",\"response\":{\"subtype\":\"success\",\"response\":{\"rate_limits_available\":true}}}\n" +
		"{\"type\":\"control_response\",\"response\":{\"subtype\":\"success\",\"response\":{\"rate_limits_available\":false}}}\n")
	got, ok := scanUsageControl(raw)
	if !ok {
		t.Fatal("no usage answer found")
	}
	if !strings.Contains(string(got), `"rate_limits_available":true`) {
		t.Errorf("response = %s, want the first success answer", got)
	}
	if _, ok := scanUsageControl([]byte("{\"type\":\"result\"}\nnot json\n")); ok {
		t.Error("non-usage output decoded as a usage answer")
	}
}

// TestProbeUsage_SendsNoUserTurn pins the quota contract at the
// production entry point: the fake CLI exits nonzero when stdin
// carries a user turn, so a regression that appends a prompt fails
// here instead of spending a model turn every probe interval.
func TestProbeUsage_SendsNoUserTurn(t *testing.T) {
	t.Parallel()

	// The "ok" fixture already asserts this: it exits 26 on a user
	// turn. A successful probe through it proves no turn was sent.
	if _, probed, _ := ProbeUsage(t.Context(), fakeUsageCLI(t, "ok")); probed.Unavailable != nil {
		t.Fatalf("probed = %+v, want windows (fixture rejects user turns)", probed.Unavailable)
	}
}

func fakeUsageCLIWithBody(t *testing.T, body string) string {
	t.Helper()
	if fakeCLIDispatcherErr != "" {
		t.Fatalf("prepare fake CLI dispatcher: %s", fakeCLIDispatcherErr)
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "fake-claude-evidence.sh")
	auth := "case \" $* \" in *\" auth status --json \"*)\n  printf '{\"loggedIn\":true}\n'\n  exit 0;;\nesac\n"
	script := "#!/bin/sh\n" + auth +
		"input=$(cat)\n" +
		"case \"$input\" in *'\"subtype\":\"get_usage\"'*) ;; *) exit 25;; esac\n" +
		`printf '{"type":"control_response","response":{"subtype":"success","response":` + body + `}}\n'` + "\n" +
		"exit 0\n"
	if err := writeFakeCLIFile(path+".fixture", script); err != nil {
		t.Fatalf("write fake cli fixture: %v", err)
	}
	if err := os.Link(fakeCLIDispatcher, path); err != nil {
		t.Fatalf("publish fake cli dispatcher: %v", err)
	}
	return path
}

func TestProbeUsagePreservesRawEvidence(t *testing.T) {
	t.Setenv("USER", "probe-operator")
	body := `{"subscription_type":"max","billing_mode":"allowance","account_id":"ws-shared-1","scope":"account","rate_limits_available":true,"rate_limits":{"five_hour":{"utilization":12.5,"resets_at":"2026-07-18T14:39:00Z","limit":100},"seven_day":{"utilization":62.5,"used_tokens":1250,"limit":2000,"resets_at":"2026-07-24T08:59:00+00:00"}}}`
	_, probed, _ := ProbeUsage(t.Context(), fakeUsageCLIWithBody(t, body))
	if probed.Unavailable != nil {
		t.Fatalf("probed=%+v want windows", probed.Unavailable)
	}
	if len(probed.Evidence) != 2 {
		t.Fatalf("evidence=%+v want five_hour+seven_day", probed.Evidence)
	}
	five, seven := probed.Evidence[0], probed.Evidence[1]
	if five.Window != "five_hour" || seven.Window != "seven_day" {
		t.Fatalf("order=%+v want five_hour then seven_day", probed.Evidence)
	}
	if five.Provider != "claude" || five.Plan != "max" || five.Allowance != "five_hour" || five.Account != "ws-shared-1" || five.User != "" {
		t.Errorf("five=%+v want claude/max/ws-shared-1/no user", five)
	}
	if five.Unit != agent.UsageUnitPercent || five.Used != 12.5 || five.Limit == nil || *five.Limit != 100 || five.ResetsAt == nil {
		t.Errorf("five=%+v want percent 12.5 limit 100 reset", five)
	}
	if five.DurationMins != nil {
		t.Errorf("five duration=%v want absent despite display fallback", five.DurationMins)
	}
	if seven.Unit != agent.UsageUnitTokens || seven.Used != 1250 || seven.Limit == nil || *seven.Limit != 2000 || seven.ResetsAt == nil {
		t.Errorf("seven=%+v want tokens 1250 limit 2000 (clamping fails)", seven)
	}
	if seven.Billing != agent.UsageBillingAllowance {
		t.Errorf("billing=%q want allowance", seven.Billing)
	}
	bare := `{"subscription_type":"max","account_id":"ws-shared-1","rate_limits_available":true,"rate_limits":{"five_hour":{"utilization":12.5},"seven_day":{"utilization":62.5,"used_tokens":1250}}}`
	_, probed2, _ := ProbeUsage(t.Context(), fakeUsageCLIWithBody(t, bare))
	if len(probed2.Evidence) != 2 || probed2.Evidence[1].Limit != nil || probed2.Evidence[1].ResetsAt != nil {
		t.Errorf("bare seven=%+v want absent limit reset (nil not zero)", probed2.Evidence)
	}
	if len(probed2.Windows) != 2 || probed2.Windows[0].WindowDurationMins == nil {
		t.Errorf("display=%+v want fallback durations", probed2.Windows)
	}
	billOnly := `{"subscription_type":"max","billing_mode":"credits","credit_balance":0,"account_id":"ws-shared-1","rate_limits_available":true,"rate_limits":{}}`
	_, billProbed, _ := ProbeUsage(t.Context(), fakeUsageCLIWithBody(t, billOnly))
	if len(billProbed.Windows) != 0 || len(billProbed.Evidence) != 1 || billProbed.Evidence[0].Billing != agent.UsageBillingCredits {
		t.Fatalf("billing-only=%+v want one credits evidence no windows", billProbed)
	}
	if billProbed.Evidence[0].Balance == nil || *billProbed.Evidence[0].Balance != 0 {
		t.Fatalf("zero balance=%v want explicit zero not absent", billProbed.Evidence[0].Balance)
	}
}
