package mergegroup

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDecisionIsNoRetry(t *testing.T) {
	if Decision != "no-retry" {
		t.Fatalf("Decision = %q, want no-retry", Decision)
	}
	if MaxAttempts != 1 {
		t.Fatalf("MaxAttempts = %d, want 1", MaxAttempts)
	}
}

func TestEventMatrixNeverRetries(t *testing.T) {
	events := []string{EventPush, EventPullRequest, EventMergeGroup}
	for _, event := range events {
		if got := MaxAttemptsFor(event); got != 1 {
			t.Errorf("MaxAttemptsFor(%q) = %d, want 1", event, got)
		}
		if AllowsRetry(event) {
			t.Errorf("AllowsRetry(%q) = true, want false", event)
		}
	}
}

func TestResolveNeverMasksFlake(t *testing.T) {
	cases := []struct {
		name string
		exit []int
		want bool
	}{
		{"fail then pass still fails", []int{1, 0}, false},
		{"fail fail fails", []int{1, 2}, false},
		{"pass passes", []int{0}, true},
		{"empty fails", nil, false},
	}
	for _, tc := range cases {
		got, attempts := Resolve(tc.exit)
		if got != tc.want {
			t.Errorf("%s: Resolve = %v, want %v", tc.name, got, tc.want)
		}
		if len(tc.exit) > 0 && attempts != 1 {
			t.Errorf("%s: attempts = %d, want 1", tc.name, attempts)
		}
	}
}

func repoWorkflow(t *testing.T) string {
	t.Helper()
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(root, ".github", "workflows", "ci.yml"))
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func TestValidateWorkflowAcceptsRepoWorkflow(t *testing.T) {
	if err := ValidateWorkflow(repoWorkflow(t)); err != nil {
		t.Fatalf("ValidateWorkflow(repo ci.yml) = %v, want nil", err)
	}
}

func TestValidateWorkflowRejectsRetry(t *testing.T) {
	base := repoWorkflow(t)
	withRetry := strings.Replace(base, "timeout-minutes: 30",
		"timeout-minutes: 30\n      continue-on-error: true", 1)
	if err := ValidateWorkflow(withRetry); err == nil {
		t.Fatal("ValidateWorkflow with continue-on-error = nil, want error")
	}
	withAttempts := strings.Replace(base, "timeout-minutes: 30",
		"timeout-minutes: 30\n      max-attempts: 3", 1)
	if err := ValidateWorkflow(withAttempts); err == nil {
		t.Fatal("ValidateWorkflow with max-attempts = nil, want error")
	}
}

func TestValidateWorkflowRejectsMissingDecision(t *testing.T) {
	base := repoWorkflow(t)
	stripped := strings.ReplaceAll(base, Marker, "")
	if err := ValidateWorkflow(stripped); err == nil {
		t.Fatal("ValidateWorkflow without decision marker = nil, want error")
	}
}

func TestValidateWorkflowRejectsMissingCoverage(t *testing.T) {
	base := repoWorkflow(t)
	stripped := strings.ReplaceAll(base, Inventory[0].Snippet, "removed-coverage")
	if err := ValidateWorkflow(stripped); err == nil {
		t.Fatal("ValidateWorkflow without coverage snippet = nil, want error")
	}
}
