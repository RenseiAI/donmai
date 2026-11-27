package daemon

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/RenseiAI/donmai/agent"
	"github.com/RenseiAI/donmai/internal/testexec"
)

// writeClaudeMarkerCLI writes a stand-in claude binary that answers for
// the login stored in $CLAUDE_CONFIG_DIR/marker. Every usage child logs
// "usage <marker>", so a test sees which directory the kernel actually
// opened rather than which path string was passed. A home it cannot read
// (no marker) reports logged out, as a CLI that finds no credentials would;
// a "slow" marker delays the usage answer.
func writeClaudeMarkerCLI(t *testing.T, usageLog string) string {
	t.Helper()
	t.Setenv("USER", "probe-operator")
	script := "#!/bin/sh\n" +
		"m=$(cat \"$CLAUDE_CONFIG_DIR/marker\" 2>/dev/null)\n" +
		"case \" $* \" in *\" auth status --json \"*)\n" +
		"  if [ -z \"$m\" ]; then printf '{\"loggedIn\":false,\"authMethod\":\"claude.ai\",\"apiProvider\":\"firstParty\"}\\n'; exit 0; fi\n" +
		"  printf '{\"loggedIn\":true,\"authMethod\":\"claude.ai\",\"apiProvider\":\"firstParty\",\"m\":\"%s\"}\\n' \"$m\"; exit 0;;\n" +
		"esac\n" +
		"echo \"usage $m\" >> \"" + usageLog + "\"\n" +
		"case \"$m\" in *slow*) sleep 3;; esac\n" +
		"[ -n \"$m\" ] || exit 1\n" +
		"printf '{\"type\":\"control_response\",\"response\":{\"subtype\":\"success\",\"response\":{\"rate_limits_available\":true,\"rate_limits\":{\"five_hour\":{\"utilization\":10,\"resets_at\":\"2030-01-01T00:00:00Z\"},\"seven_day\":{\"utilization\":20,\"resets_at\":\"2030-01-01T00:00:00Z\"}}}}}\\n'\n"
	path := filepath.Join(t.TempDir(), "fake-claude-marker")
	if err := testexec.WriteFile(path, []byte(script), 0o700); err != nil {
		t.Fatalf("write marker CLI: %v", err)
	}
	return path
}

func writeClaudeMarkerHome(t *testing.T, dir, marker string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "marker"), []byte(marker), 0o600); err != nil {
		t.Fatal(err)
	}
}

func skipWithoutShell(t *testing.T) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("fixture CLIs use /bin/sh")
	}
}

// TestQuotaPoller_TrailingSpaceHomeReadsItsOwnLogin pins that a declared
// home whose name ends in a space is read verbatim: its sibling without the
// space holds a different login and must never answer under its key.
func TestQuotaPoller_TrailingSpaceHomeReadsItsOwnLogin(t *testing.T) {
	skipWithoutShell(t)
	root := t.TempDir()
	def := filepath.Join(root, "def")
	declared := filepath.Join(root, "x ")
	sibling := filepath.Join(root, "x")

	writeCodexHome(t, def, "default")
	writeCodexHome(t, declared, "home-a")
	writeCodexHome(t, sibling, "home-b")
	codexLog := filepath.Join(root, "codex.log")
	d := driveCodexScan(t, writeCodexCountingCLI(t, codexLog), def, []string{declared})
	got, ok := d.quota.codexSlotAttempt(normalizeCodexProbeHome(declared))
	if !ok || got.accountID != "fixture-codex-a" {
		t.Fatalf("codex trailing-space home: account %q (recorded %v), want fixture-codex-a; probes %v", got.accountID, ok, codexProbeLines(t, codexLog))
	}

	writeClaudeMarkerHome(t, def, "default")
	writeClaudeMarkerHome(t, declared, "declared")
	writeClaudeMarkerHome(t, sibling, "sibling")
	claudeLog := filepath.Join(root, "claude.log")
	driveClaudeScan(t, writeClaudeMarkerCLI(t, claudeLog), def, []string{declared})
	if lines := slotUsageLines(t, claudeLog); strings.Join(lines, ",") != "usage default,usage declared" {
		t.Fatalf("claude trailing-space home usage reads = %v, want default then declared", lines)
	}
}

// TestQuotaPoller_CaseAliasOfDefaultProbesOnce pins identity dedup: on a
// case-insensitive filesystem a declared spelling that differs from the
// default only in case opens the same directory and must not be read twice.
func TestQuotaPoller_CaseAliasOfDefaultProbesOnce(t *testing.T) {
	skipWithoutShell(t)
	root := t.TempDir()
	def := filepath.Join(root, "def")
	alias := filepath.Join(root, "DEF")
	writeCodexHome(t, def, "default")
	writeClaudeMarkerHome(t, def, "default")
	if _, err := os.Stat(alias); err != nil {
		t.Skip("filesystem is case-sensitive; no case alias to dedup")
	}

	codexLog := filepath.Join(root, "codex.log")
	driveCodexScan(t, writeCodexCountingCLI(t, codexLog), def, []string{alias})
	if got := codexProbeLines(t, codexLog); len(got) != 1 {
		t.Errorf("codex probes with a case alias of the default = %v, want 1", got)
	}
	claudeLog := filepath.Join(root, "claude.log")
	driveClaudeScan(t, writeClaudeMarkerCLI(t, claudeLog), def, []string{alias})
	if got := slotUsageLines(t, claudeLog); len(got) != 1 {
		t.Errorf("claude usage reads with a case alias of the default = %v, want 1", got)
	}
}

// TestQuotaPoller_SymlinkParentDotDotReadsKernelPath pins that a declared
// path of the form logical/selected/../home-a, where selected links to
// physical/current, reads physical/home-a (what the kernel opens) and never
// the undeclared logical/home-a a lexical clean would select.
func TestQuotaPoller_SymlinkParentDotDotReadsKernelPath(t *testing.T) {
	skipWithoutShell(t)
	root := t.TempDir()
	def := filepath.Join(root, "def")
	physical := filepath.Join(root, "physical")
	logical := filepath.Join(root, "logical")
	if err := os.MkdirAll(filepath.Join(physical, "current"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(logical, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(physical, "current"), filepath.Join(logical, "selected")); err != nil {
		t.Fatal(err)
	}
	declared := filepath.Join(logical, "selected") + "/../home-a"
	writeCodexHome(t, def, "default")
	writeCodexHome(t, filepath.Join(physical, "home-a"), "home-a")
	writeCodexHome(t, filepath.Join(logical, "home-a"), "home-b")

	codexLog := filepath.Join(root, "codex.log")
	d := driveCodexScan(t, writeCodexCountingCLI(t, codexLog), def, []string{declared})
	for _, l := range codexProbeLines(t, codexLog) {
		if strings.Contains(l, "home-b") {
			t.Fatalf("codex probed undeclared logical/home-a: %v", codexProbeLines(t, codexLog))
		}
	}
	if got, ok := d.quota.codexSlotAttempt(normalizeCodexProbeHome(declared)); !ok || got.accountID != "fixture-codex-a" {
		t.Fatalf("codex declared home account %q (recorded %v), want fixture-codex-a", got.accountID, ok)
	}

	writeClaudeMarkerHome(t, def, "default")
	writeClaudeMarkerHome(t, filepath.Join(physical, "home-a"), "physical")
	writeClaudeMarkerHome(t, filepath.Join(logical, "home-a"), "logical")
	claudeLog := filepath.Join(root, "claude.log")
	driveClaudeScan(t, writeClaudeMarkerCLI(t, claudeLog), def, []string{declared})
	if lines := slotUsageLines(t, claudeLog); strings.Join(lines, ",") != "usage default,usage physical" {
		t.Fatalf("claude usage reads = %v, want default then physical", lines)
	}
}

// TestQuotaPoller_ClaudeUnreadableHomeIsUnanswered pins that a declared
// home whose permissions were revoked stays an unanswered local attempt:
// the CLI cannot read it and reports logged out, which is not a refusal of
// that login, and no child is launched for it.
func TestQuotaPoller_ClaudeUnreadableHomeIsUnanswered(t *testing.T) {
	skipWithoutShell(t)
	root := t.TempDir()
	def := filepath.Join(root, "def")
	revoked := filepath.Join(root, "revoked")
	writeClaudeMarkerHome(t, def, "default")
	writeClaudeMarkerHome(t, revoked, "revoked")
	if err := os.Chmod(revoked, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(revoked, 0o700) }) //nolint:gosec // restores search permission on a test directory so TempDir cleanup can remove it

	claudeLog := filepath.Join(root, "claude.log")
	d := driveClaudeScan(t, writeClaudeMarkerCLI(t, claudeLog), def, []string{revoked})
	got, ok := d.quota.claudeSlotAttempt(normalizeClaudeProbeHome(revoked))
	if !ok {
		t.Fatal("revoked home recorded no attempt")
	}
	if got.limits.Unavailable == nil || got.limits.Unavailable.Answered || got.auth != nil {
		t.Fatalf("permission-denied home: unavailable=%+v auth=%+v, want unanswered and no verdict", got.limits.Unavailable, got.auth)
	}
	if lines := slotUsageLines(t, claudeLog); strings.Join(lines, ",") != "usage default" {
		t.Fatalf("usage reads = %v, want only the default", lines)
	}
}

// TestScanObservations_DefaultOnlyIgnoresSlowExtras pins that the on-demand
// observation scan reads only the default home: extras are never published,
// so a slow extra must not push the scan past its deadline and downgrade a
// verified default to scan_timeout.
func TestScanObservations_DefaultOnlyIgnoresSlowExtras(t *testing.T) {
	skipWithoutShell(t)
	root := t.TempDir()
	def := filepath.Join(root, "def")
	writeClaudeMarkerHome(t, def, "default")
	var extras []string
	for _, name := range []string{"slow-1", "slow-2"} {
		dir := filepath.Join(root, name)
		writeClaudeMarkerHome(t, dir, name)
		extras = append(extras, dir)
	}
	claudeLog := filepath.Join(root, "claude.log")
	bin := writeClaudeMarkerCLI(t, claudeLog)
	t.Setenv("CLAUDE_CONFIG_DIR", def)
	d := New(Options{QuotaProbeBinaries: map[string]string{agent.UsageHarnessClaude: bin}})
	d.config = &Config{QuotaProbeHomes: map[string][]string{agent.UsageHarnessClaude: extras}}
	d.observationScan.timeout = 2 * time.Second

	result, err := d.ScanObservations(context.Background(), nil)
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	raw, err := json.Marshal(result.Report)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "scan_timeout") {
		t.Errorf("slow extras downgraded the default report: %s", raw)
	}
	if lines := slotUsageLines(t, claudeLog); strings.Join(lines, ",") != "usage default" {
		t.Errorf("on-demand scan usage reads = %v, want only the default", lines)
	}
}

// TestLoadConfig_QuotaProbeHomesRejectsAuthoredEmptyElements pins that a
// bare list dash or explicit null inside a harness's home list is refused
// at load: the typed decoder would otherwise drop the slot silently. The
// error names the slot, never a path.
func TestLoadConfig_QuotaProbeHomesRejectsAuthoredEmptyElements(t *testing.T) {
	base := "machine:\n  id: m\norchestrator:\n  url: https://example.test\n"
	for name, body := range map[string]string{
		"claude bare dash":  "quotaProbeHomes:\n  claude:\n    - /fixture-home-kept-12345\n    -\n",
		"claude null":       "quotaProbeHomes:\n  claude: [/fixture-home-kept-12345, null]\n",
		"codex bare dash":   "quotaProbeHomes:\n  codex:\n    -\n",
		"codex tilde null":  "quotaProbeHomes:\n  codex: [~]\n",
		"claude blank text": "quotaProbeHomes:\n  claude: ['   ']\n",
	} {
		p := filepath.Join(t.TempDir(), "daemon.yaml")
		if err := os.WriteFile(p, []byte(base+body), 0o600); err != nil {
			t.Fatal(err)
		}
		_, err := LoadConfig(p)
		if err == nil {
			t.Errorf("%s: authored empty element accepted", name)
			continue
		}
		if strings.Contains(err.Error(), "fixture-home-kept-12345") {
			t.Errorf("%s: rejection echoed a path: %v", name, err)
		}
	}
	ok := filepath.Join(t.TempDir(), "daemon.yaml")
	if err := os.WriteFile(ok, []byte(base+"quotaProbeHomes:\n  claude:\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadConfig(ok); err != nil {
		t.Errorf("omitted (null) list rejected: %v", err)
	}
}
