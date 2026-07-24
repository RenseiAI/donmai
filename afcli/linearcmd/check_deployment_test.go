package linearcmd

// check_deployment_test.go — table-driven tests for the native
// checkDeployment implementation (Part B of GO-3).
//
// Tests mock the runGhAPI var to avoid real gh CLI / network calls.
// Table-driven, stdlib testing, no testify.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

// ── helpers ───────────────────────────────────────────────────────────────────

// mockGhAPI replaces runGhAPI for the duration of a test, restoring it on
// cleanup. The mock maps "endpoint → (rawJSON, error)"; when an endpoint is
// not in the map it returns an error.
func mockGhAPI(t *testing.T, responses map[string]mockGhResponse) func() {
	t.Helper()
	original := runGhAPI
	runGhAPI = func(_ context.Context, endpoint string, _ ...string) ([]byte, error) {
		resp, ok := responses[endpoint]
		if !ok {
			return nil, fmt.Errorf("mock: unexpected endpoint %q", endpoint)
		}
		if resp.err != nil {
			return nil, resp.err
		}
		return resp.body, nil
	}
	return func() { runGhAPI = original }
}

type mockGhResponse struct {
	body []byte
	err  error
}

// prJSON builds a minimal GitHub PR JSON payload.
func prJSON(sha string) []byte {
	return []byte(fmt.Sprintf(`{"number":42,"head":{"sha":%q},"html_url":"https://github.com/owner/repo/pull/42"}`, sha))
}

// statusJSON builds a minimal GitHub Commit Status JSON payload.
func statusJSON(state string, statuses []map[string]string) []byte {
	ss, _ := json.Marshal(statuses)
	return []byte(fmt.Sprintf(`{"state":%q,"statuses":%s}`, state, ss))
}

// ── checkDeployment tests ─────────────────────────────────────────────────────

func TestCheckDeployment_HappyPath(t *testing.T) {
	const (
		owner   = "owner"
		repo    = "repo"
		prNum   = 42
		headSHA = "abc1234567890abcdef"
	)

	prEndpoint := fmt.Sprintf("repos/%s/%s/pulls/%d", owner, repo, prNum)
	statusEndpoint := fmt.Sprintf("repos/%s/%s/commits/%s/status", owner, repo, headSHA)

	statusData := []map[string]string{
		{"context": "vercel/my-app (production)", "state": "success", "description": "Deployment passed", "target_url": "https://my-app.vercel.app"},
		{"context": "vercel/my-api (preview)", "state": "success", "description": "Deployment passed", "target_url": "https://my-api-abc.vercel.app"},
		{"context": "ci/lint", "state": "success", "description": "Lint passed"}, // non-Vercel, must be excluded
	}

	restore := mockGhAPI(t, map[string]mockGhResponse{
		prEndpoint:     {body: prJSON(headSHA)},
		statusEndpoint: {body: statusJSON("success", statusData)},
	})
	defer restore()

	result, err := checkDeployment(context.Background(), owner, repo, prNum)
	if err != nil {
		t.Fatalf("checkDeployment: %v", err)
	}

	if result.PRNumber != prNum {
		t.Errorf("PRNumber = %d, want %d", result.PRNumber, prNum)
	}
	if result.SHA != headSHA {
		t.Errorf("SHA = %q, want %q", result.SHA, headSHA)
	}
	if result.AnyFailed {
		t.Errorf("AnyFailed = true, want false")
	}
	// Only Vercel contexts; ci/lint must be excluded.
	if len(result.Deployments) != 2 {
		t.Fatalf("len(Deployments) = %d, want 2", len(result.Deployments))
	}
	if result.Deployments[0].App != "my-app" {
		t.Errorf("Deployments[0].App = %q, want my-app", result.Deployments[0].App)
	}
	if result.Deployments[0].State != "success" {
		t.Errorf("Deployments[0].State = %q, want success", result.Deployments[0].State)
	}
}

func TestCheckDeployment_AnyFailed(t *testing.T) {
	const (
		owner   = "owner"
		repo    = "repo"
		prNum   = 7
		headSHA = "deadbeef1234"
	)

	prEndpoint := fmt.Sprintf("repos/%s/%s/pulls/%d", owner, repo, prNum)
	statusEndpoint := fmt.Sprintf("repos/%s/%s/commits/%s/status", owner, repo, headSHA)

	statusData := []map[string]string{
		{"context": "vercel/frontend", "state": "failure", "description": "Build failed"},
		{"context": "vercel/backend", "state": "success", "description": "OK"},
	}

	restore := mockGhAPI(t, map[string]mockGhResponse{
		prEndpoint:     {body: prJSON(headSHA)},
		statusEndpoint: {body: statusJSON("failure", statusData)},
	})
	defer restore()

	result, err := checkDeployment(context.Background(), owner, repo, prNum)
	if err != nil {
		t.Fatalf("checkDeployment: %v", err)
	}
	if !result.AnyFailed {
		t.Errorf("AnyFailed = false, want true (frontend failed)")
	}
}

func TestCheckDeployment_NoVercelStatuses(t *testing.T) {
	const (
		owner   = "owner"
		repo    = "repo"
		prNum   = 99
		headSHA = "cafebabe0000"
	)

	prEndpoint := fmt.Sprintf("repos/%s/%s/pulls/%d", owner, repo, prNum)
	statusEndpoint := fmt.Sprintf("repos/%s/%s/commits/%s/status", owner, repo, headSHA)

	// Only non-Vercel statuses.
	statusData := []map[string]string{
		{"context": "ci/tests", "state": "success"},
		{"context": "security/snyk", "state": "success"},
	}

	restore := mockGhAPI(t, map[string]mockGhResponse{
		prEndpoint:     {body: prJSON(headSHA)},
		statusEndpoint: {body: statusJSON("success", statusData)},
	})
	defer restore()

	result, err := checkDeployment(context.Background(), owner, repo, prNum)
	if err != nil {
		t.Fatalf("checkDeployment: %v", err)
	}
	if result.AnyFailed {
		t.Errorf("AnyFailed = true, want false (no Vercel statuses)")
	}
	if len(result.Deployments) != 0 {
		t.Errorf("len(Deployments) = %d, want 0", len(result.Deployments))
	}
}

func TestCheckDeployment_PRNotFound(t *testing.T) {
	const (
		owner = "owner"
		repo  = "repo"
		prNum = 9999
	)
	prEndpoint := fmt.Sprintf("repos/%s/%s/pulls/%d", owner, repo, prNum)

	restore := mockGhAPI(t, map[string]mockGhResponse{
		prEndpoint: {err: fmt.Errorf("gh api %s: exit 1: Not Found", prEndpoint)},
	})
	defer restore()

	_, err := checkDeployment(context.Background(), owner, repo, prNum)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "9999") {
		t.Errorf("error %q does not mention PR number", err.Error())
	}
}

func TestCheckDeployment_EmptySHA(t *testing.T) {
	const (
		owner = "owner"
		repo  = "repo"
		prNum = 1
	)
	prEndpoint := fmt.Sprintf("repos/%s/%s/pulls/%d", owner, repo, prNum)

	// Return a PR with an empty SHA.
	restore := mockGhAPI(t, map[string]mockGhResponse{
		prEndpoint: {body: prJSON("")},
	})
	defer restore()

	_, err := checkDeployment(context.Background(), owner, repo, prNum)
	if err == nil {
		t.Fatal("expected error for empty SHA, got nil")
	}
}

// ── extractVercelApp tests ────────────────────────────────────────────────────

func TestExtractVercelApp(t *testing.T) {
	tests := []struct {
		context string
		want    string
	}{
		{"vercel", "vercel"},
		{"vercel/my-app", "my-app"},
		{"vercel/my-app (production)", "my-app"},
		{"vercel/my-api (preview)", "my-api"},
		{"vercel/dashboard (staging)", "dashboard"},
		{"vercel/no-env-suffix", "no-env-suffix"},
		{"Vercel/Mixed-Case", "Mixed-Case"}, // context is passed as-is; extraction is after prefix check
	}
	for _, tc := range tests {
		tc := tc
		t.Run(tc.context, func(t *testing.T) {
			got := extractVercelApp(tc.context)
			if got != tc.want {
				t.Errorf("extractVercelApp(%q) = %q, want %q", tc.context, got, tc.want)
			}
		})
	}
}

// ── parseGitRemote tests ──────────────────────────────────────────────────────

func TestParseGitRemote(t *testing.T) {
	tests := []struct {
		remote    string
		wantOwner string
		wantRepo  string
		wantErr   bool
	}{
		{"https://github.com/owner/repo", "owner", "repo", false},
		{"https://github.com/owner/repo.git", "owner", "repo", false},
		{"git@github.com:owner/repo.git", "owner", "repo", false},
		{"git@github.com:owner/repo", "owner", "repo", false},
		{"https://github.com/myorg/my-project", "myorg", "my-project", false},
		{"https://github.com/foo/bar.git", "foo", "bar", false},
		{"git@gitlab.com:foo/bar.git", "foo", "bar", false},
		{"not-a-url", "", "", true},
	}
	for _, tc := range tests {
		tc := tc
		t.Run(tc.remote, func(t *testing.T) {
			owner, repo, err := parseGitRemote(tc.remote)
			if tc.wantErr {
				if err == nil {
					t.Errorf("expected error, got owner=%q repo=%q", owner, repo)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if owner != tc.wantOwner {
				t.Errorf("owner = %q, want %q", owner, tc.wantOwner)
			}
			if repo != tc.wantRepo {
				t.Errorf("repo = %q, want %q", repo, tc.wantRepo)
			}
		})
	}
}

// ── formatDeploymentMarkdown tests ───────────────────────────────────────────

func TestFormatDeploymentMarkdown(t *testing.T) {
	result := &deploymentCheckResult{
		PRNumber:  42,
		SHA:       "abc123",
		AnyFailed: false,
		Deployments: []deploymentStatus{
			{App: "frontend", State: "success", Description: "Ready", TargetURL: "https://frontend.vercel.app"},
		},
	}
	got := formatDeploymentMarkdown(result)
	if !strings.Contains(got, "PR #42") {
		t.Errorf("markdown does not mention PR #42: %s", got)
	}
	if !strings.Contains(got, "frontend") {
		t.Errorf("markdown does not mention app: %s", got)
	}
	if !strings.Contains(got, "success") {
		t.Errorf("markdown does not mention state: %s", got)
	}
}

func TestFormatDeploymentMarkdown_AnyFailed(t *testing.T) {
	result := &deploymentCheckResult{
		PRNumber:  7,
		SHA:       "dead",
		AnyFailed: true,
		Deployments: []deploymentStatus{
			{App: "backend", State: "failure"},
		},
	}
	got := formatDeploymentMarkdown(result)
	if !strings.Contains(got, "FAILED") {
		t.Errorf("markdown does not say FAILED: %s", got)
	}
}

func TestFormatDeploymentMarkdown_Empty(t *testing.T) {
	result := &deploymentCheckResult{
		PRNumber:    1,
		SHA:         "abc",
		AnyFailed:   false,
		Deployments: []deploymentStatus{},
	}
	got := formatDeploymentMarkdown(result)
	if !strings.Contains(got, "No Vercel deployment") {
		t.Errorf("markdown missing no-deployments message: %s", got)
	}
}

// ── JSON output shape test ────────────────────────────────────────────────────

// TestCheckDeploymentJSONShape verifies the JSON output of a successful
// check-deployment command matches the expected envelope shape.
func TestCheckDeploymentJSONShape(t *testing.T) {
	const (
		owner   = "acme"
		repo    = "platform"
		prNum   = 10
		headSHA = "feedface1234"
	)

	prEndpoint := fmt.Sprintf("repos/%s/%s/pulls/%d", owner, repo, prNum)
	statusEndpoint := fmt.Sprintf("repos/%s/%s/commits/%s/status", owner, repo, headSHA)

	statusData := []map[string]string{
		{"context": "vercel/web", "state": "success", "description": "Deployed", "target_url": "https://acme.vercel.app"},
	}

	restore := mockGhAPI(t, map[string]mockGhResponse{
		prEndpoint:     {body: prJSON(headSHA)},
		statusEndpoint: {body: statusJSON("success", statusData)},
	})
	defer restore()

	result, err := checkDeployment(context.Background(), owner, repo, prNum)
	if err != nil {
		t.Fatalf("checkDeployment: %v", err)
	}

	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetIndent("", "  ")
	if err := enc.Encode(result); err != nil {
		t.Fatalf("encode: %v", err)
	}

	var decoded deploymentCheckResult
	if err := json.Unmarshal(buf.Bytes(), &decoded); err != nil {
		t.Fatalf("decode round-trip: %v", err)
	}
	if decoded.PRNumber != prNum {
		t.Errorf("prNumber = %d, want %d", decoded.PRNumber, prNum)
	}
	if decoded.SHA != headSHA {
		t.Errorf("sha = %q, want %q", decoded.SHA, headSHA)
	}
	if len(decoded.Deployments) != 1 {
		t.Fatalf("len(deployments) = %d, want 1", len(decoded.Deployments))
	}
	if decoded.Deployments[0].App != "web" {
		t.Errorf("deployments[0].app = %q, want web", decoded.Deployments[0].App)
	}
}
