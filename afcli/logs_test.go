package afcli

// Tests for `af logs analyze`.
//
// Strategy:
//   - Unit tests for signature matching (no network).
//   - Integration-style tests for the full analyze command path, with an
//     httptest.Server simulating the Linear GraphQL API for the posting path.
//   - Dry-run and JSON output tests verify the command layer.

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/RenseiAI/agentfactory-tui/afclient/logsignatures"
)

// ─── helpers ──────────────────────────────────────────────────────────────────

// runLogsCmd builds a fresh `logs` cobra tree, sets args, and returns stdout.
func runLogsCmd(t *testing.T, args ...string) (string, error) {
	t.Helper()
	root := newLogsCmd()
	root.SilenceErrors = true

	var buf bytes.Buffer
	root.SetOut(&buf)
	root.SetErr(&buf)
	root.SetArgs(args)

	err := root.Execute()
	return buf.String(), err
}

// writeTempLog creates a temporary log file with the given content and returns
// its path.  The file is removed when the test ends.
func writeTempLog(t *testing.T, content string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "agent.log")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil { //nolint:gosec // G306 -- test fixture
		t.Fatalf("write temp log: %v", err)
	}
	return path
}

// ─── signature matching unit tests ───────────────────────────────────────────

func TestDefaultSignatures_Compile(t *testing.T) {
	sigs := logsignatures.DefaultSignatures()
	if len(sigs) == 0 {
		t.Fatal("expected non-empty default signatures")
	}
	// All default signatures must compile successfully (enforced in DefaultSignatures).
	// Verify Match is callable on each.
	for _, s := range sigs {
		s := s // capture
		t.Run(s.ID, func(_ *testing.T) {
			// Just ensure calling Match does not panic.
			_ = logsignatures.Match("some benign log line", []logsignatures.Signature{s})
		})
	}
}

func TestMatch_ApprovalRequired(t *testing.T) {
	sigs := logsignatures.DefaultSignatures()
	line := "ERROR: This command requires approval before execution"
	mr := logsignatures.Match(line, sigs)
	if mr == nil {
		t.Fatal("expected a match, got nil")
	}
	if mr.Signature.ID != "approval-required" {
		t.Errorf("expected approval-required, got %q", mr.Signature.ID)
	}
	if mr.Signature.Severity != logsignatures.SeverityCritical {
		t.Errorf("expected critical severity, got %q", mr.Signature.Severity)
	}
}

func TestMatch_WriteBeforeRead(t *testing.T) {
	sigs := logsignatures.DefaultSignatures()
	line := "Error: File has not been read yet. Use the Read tool first."
	mr := logsignatures.Match(line, sigs)
	if mr == nil {
		t.Fatal("expected a match, got nil")
	}
	if mr.Signature.ID != "write-before-read" {
		t.Errorf("expected write-before-read, got %q", mr.Signature.ID)
	}
}

func TestMatch_PermissionDenied(t *testing.T) {
	sigs := logsignatures.DefaultSignatures()
	line := "open /etc/shadow: permission denied"
	mr := logsignatures.Match(line, sigs)
	if mr == nil {
		t.Fatal("expected a match, got nil")
	}
	if mr.Signature.Type != logsignatures.PatternPermission {
		t.Errorf("expected permission type, got %q", mr.Signature.Type)
	}
}

func TestMatch_RateLimit(t *testing.T) {
	sigs := logsignatures.DefaultSignatures()
	line := "HTTP 429 too many requests — rate limit hit"
	mr := logsignatures.Match(line, sigs)
	if mr == nil {
		t.Fatal("expected a match, got nil")
	}
	if mr.Signature.ID != "rate-limit" {
		t.Errorf("expected rate-limit, got %q", mr.Signature.ID)
	}
}

func TestMatch_NoMatch(t *testing.T) {
	sigs := logsignatures.DefaultSignatures()
	line := "INFO: session started successfully"
	mr := logsignatures.Match(line, sigs)
	if mr != nil {
		t.Errorf("expected no match for benign line, got %q", mr.Signature.ID)
	}
}

func TestMatch_CaseInsensitive(t *testing.T) {
	sigs := logsignatures.DefaultSignatures()
	line := "PERMISSION DENIED for file /var/run/agent.sock"
	mr := logsignatures.Match(line, sigs)
	if mr == nil {
		t.Fatal("expected a match for uppercased line")
	}
}

// ─── GenerateSignatureHash ────────────────────────────────────────────────────

func TestGenerateSignatureHash(t *testing.T) {
	h1 := logsignatures.GenerateSignatureHash(logsignatures.PatternPermission, "File permission denied")
	h2 := logsignatures.GenerateSignatureHash(logsignatures.PatternPermission, "File permission denied")
	if h1 != h2 {
		t.Error("signature hash must be deterministic")
	}
	if !strings.HasPrefix(h1, "agent-env-") {
		t.Errorf("expected prefix 'agent-env-', got %q", h1)
	}
	h3 := logsignatures.GenerateSignatureHash(logsignatures.PatternToolIssue, "File permission denied")
	if h1 == h3 {
		t.Error("different type should produce different hash")
	}
}

// ─── LoadCatalog ──────────────────────────────────────────────────────────────

func TestLoadCatalog_UserSignaturesFirst(t *testing.T) {
	dir := t.TempDir()
	catalog := filepath.Join(dir, "log-signatures.yaml")
	yaml := `signatures:
  - id: custom-error
    pattern: "MY_CUSTOM_ERROR"
    type: tool_issue
    severity: high
    title: "Custom error"
`
	if err := os.WriteFile(catalog, []byte(yaml), 0o600); err != nil { //nolint:gosec // G306 -- test fixture
		t.Fatalf("write catalog: %v", err)
	}
	sigs, err := logsignatures.LoadCatalog(catalog)
	if err != nil {
		t.Fatalf("load catalog: %v", err)
	}
	// First signature must be the custom one (user rules take priority).
	if sigs[0].ID != "custom-error" {
		t.Errorf("expected custom-error first, got %q", sigs[0].ID)
	}
	// Default signatures must still be present.
	found := false
	for _, s := range sigs {
		if s.ID == "rate-limit" {
			found = true
			break
		}
	}
	if !found {
		t.Error("expected default signature 'rate-limit' to be present after merge")
	}
}

func TestLoadCatalog_FileNotFound_FallsBackToDefaults(t *testing.T) {
	sigs, err := loadSignatures("/nonexistent/path/log-signatures.yaml")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(sigs) == 0 {
		t.Error("expected default signatures when catalog file is missing")
	}
}

// ─── analyze command: dry-run (no network) ────────────────────────────────────

func TestLogsAnalyze_DryRun_HumanOutput(t *testing.T) {
	logContent := strings.Join([]string{
		"INFO: starting session abc123",
		"ERROR: This command requires approval before execution",
		"INFO: retrying...",
		"ERROR: File has not been read yet. Use the Read tool first.",
		"INFO: done",
	}, "\n")

	path := writeTempLog(t, logContent)
	out, err := runLogsCmd(t, "analyze", "--input", path, "--dry-run")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if !strings.Contains(out, "Command requires approval") {
		t.Errorf("expected approval-required pattern in output, got:\n%s", out)
	}
	if !strings.Contains(out, "Write attempted before read") {
		t.Errorf("expected write-before-read pattern in output, got:\n%s", out)
	}
	if !strings.Contains(out, "DRY RUN") {
		t.Errorf("expected DRY RUN label in output, got:\n%s", out)
	}
}

func TestLogsAnalyze_DryRun_JSONOutput(t *testing.T) {
	logContent := strings.Join([]string{
		"ERROR: permission denied opening /var/run/agent.sock",
		"ERROR: ENOENT: no such file or directory, open '/tmp/agent.pid'",
	}, "\n")

	path := writeTempLog(t, logContent)
	out, err := runLogsCmd(t, "analyze", "--input", path, "--dry-run", "--json")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var result AnalysisResult
	if err := json.Unmarshal([]byte(out), &result); err != nil {
		t.Fatalf("invalid JSON output: %v\nraw:\n%s", err, out)
	}
	if result.LinesScanned != 2 {
		t.Errorf("expected 2 lines scanned, got %d", result.LinesScanned)
	}
	if len(result.Matches) == 0 {
		t.Errorf("expected matches, got none; output:\n%s", out)
	}
}

func TestLogsAnalyze_NoMatches(t *testing.T) {
	logContent := "INFO: session started\nINFO: tool result received\nINFO: done\n"
	path := writeTempLog(t, logContent)

	out, err := runLogsCmd(t, "analyze", "--input", path, "--dry-run", "--json")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var result AnalysisResult
	if err := json.Unmarshal([]byte(out), &result); err != nil {
		t.Fatalf("invalid JSON output: %v", err)
	}
	if len(result.Matches) != 0 {
		t.Errorf("expected no matches for benign log, got %d", len(result.Matches))
	}
	if len(result.DraftedIssues) != 0 {
		t.Errorf("expected no drafted issues for benign log, got %d", len(result.DraftedIssues))
	}
}

func TestLogsAnalyze_MissingFile(t *testing.T) {
	_, err := runLogsCmd(t, "analyze", "--input", "/nonexistent/agent.log")
	if err == nil {
		t.Error("expected error for missing file")
	}
}

// ─── analyze command: posting path (httptest) ─────────────────────────────────

// setupLogsLinearServer spins up a fake Linear GraphQL server that handles
// team, project, labels, and issue-creation queries.
func setupLogsLinearServer(t *testing.T) *httptest.Server {
	t.Helper()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")

		var body struct {
			Query string `json:"query"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		q := body.Query

		switch {
		case strings.Contains(q, "teams"):
			_, _ = fmt.Fprint(w, `{"data":{"teams":{"nodes":[{"id":"team-1","key":"ENG","name":"Engineering"}]}}}`)
		case strings.Contains(q, "projects"):
			_, _ = fmt.Fprint(w, `{"data":{"projects":{"nodes":[{"id":"proj-1","name":"Agent"}]}}}`)
		case strings.Contains(q, "issueLabels"):
			_, _ = fmt.Fprint(w, `{"data":{"issueLabels":{"nodes":[{"id":"lbl-1","name":"Agent"},{"id":"lbl-2","name":"Infrastructure"}]}}}`)
		case strings.Contains(q, "issueCreate"):
			_, _ = fmt.Fprint(w, `{"data":{"issueCreate":{"success":true,"issue":{"id":"iss-1","identifier":"ENG-42","title":"[Agent Environment] Tool execution failed","url":"https://linear.app/test/issue/ENG-42","state":{"name":"Backlog"},"team":{"id":"team-1","key":"ENG","name":"Engineering"},"labels":{"nodes":[]}}}}}`)
		default:
			_, _ = fmt.Fprint(w, `{"data":{}}`)
		}
	}))

	t.Cleanup(srv.Close)
	t.Setenv("LINEAR_API_KEY", "test-fixture-key-not-a-secret") //nolint:gosec // dummy fixture
	logsTestBaseURL = srv.URL
	t.Cleanup(func() { logsTestBaseURL = "" })
	return srv
}

func TestLogsAnalyze_PostToLinear(t *testing.T) {
	// Set up fake Linear server.
	_ = setupLogsLinearServer(t)

	// Log that contains high-severity tool_issue patterns.
	logContent := strings.Join([]string{
		"ERROR: tool execution failed: command not found",
		"ERROR: ECONNREFUSED connecting to redis:6379",
		"ERROR: ENOENT: no such file or directory /tmp/agent.pid",
	}, "\n")
	path := writeTempLog(t, logContent)

	out, err := runLogsCmd(t,
		"analyze",
		"--input", path,
		"--team", "Engineering",
		"--json",
	)
	if err != nil {
		t.Fatalf("unexpected error: %v\noutput:\n%s", err, out)
	}

	var result AnalysisResult
	if err := json.Unmarshal([]byte(out), &result); err != nil {
		t.Fatalf("invalid JSON: %v\nraw:\n%s", err, out)
	}

	if len(result.DraftedIssues) == 0 {
		t.Fatalf("expected drafted issues, got none")
	}

	// At least one issue should be marked as posted.
	posted := false
	for _, d := range result.DraftedIssues {
		if d.Posted {
			posted = true
			if d.Identifier == "" {
				t.Error("posted issue must have a non-empty identifier")
			}
		}
	}
	if !posted {
		t.Error("expected at least one issue to be posted to Linear")
	}
}

// ─── draft content assertions ──────────────────────────────────────────────────

func TestBuildDrafts_TitleIncludesCategoryPrefix(t *testing.T) {
	matches := []PatternMatch{
		{
			SignatureID: "rate-limit",
			Type:        logsignatures.PatternPerformance,
			Severity:    logsignatures.SeverityHigh,
			Title:       "Rate limit exceeded",
			Occurrences: 2,
			Examples:    []string{"HTTP 429 too many requests"},
		},
	}
	drafts := buildDrafts(matches)
	if len(drafts) == 0 {
		t.Fatal("expected a draft, got none")
	}
	if !strings.Contains(drafts[0].Title, "[Agent Environment]") {
		t.Errorf("expected '[Agent Environment]' prefix in title, got %q", drafts[0].Title)
	}
}

func TestBuildDrafts_ToolMisusePrefix(t *testing.T) {
	matches := []PatternMatch{
		{
			SignatureID: "write-before-read",
			Type:        logsignatures.PatternToolMisuse,
			Severity:    logsignatures.SeverityHigh,
			Title:       "Write attempted before read",
			Occurrences: 1,
			Examples:    []string{"File has not been read yet"},
		},
	}
	drafts := buildDrafts(matches)
	if len(drafts) == 0 {
		t.Fatal("expected a draft, got none")
	}
	if !strings.Contains(drafts[0].Title, "[Agent Behavior]") {
		t.Errorf("expected '[Agent Behavior]' prefix in title, got %q", drafts[0].Title)
	}
}

func TestBuildDrafts_DescriptionContainsSummary(t *testing.T) {
	matches := []PatternMatch{
		{
			SignatureID: "sandbox-permission",
			Type:        logsignatures.PatternPermission,
			Severity:    logsignatures.SeverityHigh,
			Title:       "Sandbox permission error",
			Occurrences: 3,
			Examples:    []string{"operation not permitted"},
		},
	}
	drafts := buildDrafts(matches)
	if len(drafts) == 0 {
		t.Fatal("expected a draft")
	}
	if !strings.Contains(drafts[0].Description, "## Summary") {
		t.Errorf("description should contain '## Summary' section")
	}
	if !strings.Contains(drafts[0].Description, "## Examples") {
		t.Errorf("description should contain '## Examples' section")
	}
}

// ─── OSS parity verification ──────────────────────────────────────────────────

// TestOSSParity_SignatureCount verifies the Go port has at least as many
// signatures as the TS reference PATTERN_RULES array (17 rules).
func TestOSSParity_SignatureCount(t *testing.T) {
	const tsRuleCount = 17
	sigs := logsignatures.DefaultSignatures()
	if len(sigs) < tsRuleCount {
		t.Errorf("expected at least %d signatures (TS rule count), got %d", tsRuleCount, len(sigs))
	}
}

// TestOSSParity_AllTSPatternIDs verifies each TS rule ID has a corresponding
// Go signature by exercising the documented representative lines.
func TestOSSParity_AllTSPatternIDs(t *testing.T) {
	sigs := logsignatures.DefaultSignatures()

	cases := []struct {
		tsPattern string
		line      string
		expectID  string
	}{
		{"requires approval", "This command requires approval", "approval-required"},
		{"File has not been read yet", "File has not been read yet", "write-before-read"},
		{"File does not exist", "File does not exist", "file-does-not-exist"},
		{"Path does not exist", "Path does not exist", "path-does-not-exist"},
		{"Unknown JSON field", "Unknown JSON field 'foo'", "invalid-tool-param"},
		{"Glob patterns in write", "Glob patterns are not allowed in write operations", "glob-in-write"},
		{"tool_use_error", "<tool_use_error>oops</tool_use_error>", "tool-api-error"},
		{"exceeds maximum allowed tokens", "File exceeds maximum allowed tokens", "file-too-large"},
		{"cd blocked", "cd in /etc was blocked by sandbox", "dir-blocked"},
		{"sandbox not allowed", "sandbox: operation not permitted", "sandbox-permission"},
		{"permission denied", "open /foo: permission denied", "file-permission-denied"},
		{"ENOENT", "ENOENT: no such file or directory", "file-not-found"},
		{"timeout", "ETIMEDOUT: connection timed out", "network-timeout"},
		{"rate limit", "429 too many requests — rate limit", "rate-limit"},
		{"ECONNREFUSED", "ECONNREFUSED connecting to localhost:6379", "network-connection"},
		{"worktree conflict", "is already used by worktree at /tmp/wt", "worktree-conflict"},
		{"command failed", "tool execution failed: command failed", "tool-execution-failed"},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.tsPattern, func(_ *testing.T) {
			mr := logsignatures.Match(tc.line, sigs)
			if mr == nil {
				t.Errorf("line %q: expected match for TS pattern %q, got nil", tc.line, tc.tsPattern)
				return
			}
			if mr.Signature.ID != tc.expectID {
				t.Errorf("line %q: expected sig %q, got %q", tc.line, tc.expectID, mr.Signature.ID)
			}
		})
	}
}
