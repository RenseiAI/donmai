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
	err := ValidateWorkflow(withRetry)
	if err == nil {
		t.Fatal("ValidateWorkflow with continue-on-error = nil, want error")
	}
	if !strings.Contains(strings.ToLower(err.Error()), "continue-on-error") {
		t.Fatalf("ValidateWorkflow with continue-on-error = %v, want continue-on-error error", err)
	}
	withAttempts := strings.Replace(base, "timeout-minutes: 30",
		"timeout-minutes: 30\n      max-attempts: 3", 1)
	err = ValidateWorkflow(withAttempts)
	if err == nil {
		t.Fatal("ValidateWorkflow with max-attempts = nil, want error")
	}
	if !strings.Contains(strings.ToLower(err.Error()), "max-attempts") {
		t.Fatalf("ValidateWorkflow with max-attempts = %v, want max-attempts error", err)
	}
}

func TestValidateWorkflowRejectsExitZeroMasking(t *testing.T) {
	base := repoWorkflow(t)
	cases := map[string]string{
		"semicolon-true": strings.Replace(base,
			"go test -race -coverprofile=coverage.out -json ./...",
			"go test -race -coverprofile=coverage.out -json ./...; true", 1),
		"semicolon-noop": strings.Replace(base,
			"go test -race -coverprofile=coverage.out -json ./...",
			"go test -race -coverprofile=coverage.out -json ./...; :", 1),
		"and-true": strings.Replace(base,
			"go test -race -coverprofile=coverage.out -json ./...",
			"go test -race -coverprofile=coverage.out -json ./... && true", 1),
	}
	for name, txt := range cases {
		if err := ValidateWorkflow(txt); err == nil {
			t.Errorf("ValidateWorkflow(%s) = nil, want error", name)
		}
	}
}

func TestValidateWorkflowRejectsFailureRerun(t *testing.T) {
	base := repoWorkflow(t)
	// Neutral step name: no retry/attempt word, only if: failure() + go test.
	withRerun := strings.Replace(base, "timeout-minutes: 30",
		"timeout-minutes: 30\n      - name: Follow-up checks\n        if: failure()\n        run: go test -race -coverprofile=coverage.out -json ./...", 1)
	if err := ValidateWorkflow(withRerun); err == nil {
		t.Fatal("ValidateWorkflow with if: failure() rerun = nil, want error")
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

func TestValidateWorkflowRejectsMissingTrigger(t *testing.T) {
	base := repoWorkflow(t)
	stripped := strings.ReplaceAll(base, "merge_group:", "removed-trigger:")
	if err := ValidateWorkflow(stripped); err == nil {
		t.Fatal("ValidateWorkflow without merge_group trigger = nil, want error")
	}
}

func TestValidateWorkflowRejectsRetryAction(t *testing.T) {
	base := repoWorkflow(t)
	withAction := strings.Replace(base, "timeout-minutes: 30",
		"timeout-minutes: 30\n      - uses: nick-fields/retry@v3", 1)
	if err := ValidateWorkflow(withAction); err == nil {
		t.Fatal("ValidateWorkflow with retry action = nil, want error")
	}
}

func TestValidateWorkflowRejectsRelocatedCoverage(t *testing.T) {
	base := repoWorkflow(t)
	removed := strings.Replace(base, Inventory[0].Snippet, "removed-coverage", 1)
	relocated := removed + "\n  extra-holder:\n    runs-on: ubuntu-latest\n    steps:\n      - run: " + Inventory[0].Snippet + "\n"
	if err := ValidateWorkflow(relocated); err == nil {
		t.Fatal("ValidateWorkflow with relocated coverage = nil, want error")
	}
}

func TestValidateWorkflowRejectsShadowBypasses(t *testing.T) {
	base := repoWorkflow(t)
	cases := map[string]string{
		"commented-trigger": strings.Replace(base, "  merge_group:", "  # merge_group:", 1),
		"disabled-confinement": strings.ReplaceAll(
			base, "live_darwin_test.go", "# live_darwin_test.go",
		),
		"expression-continue-on-error": strings.Replace(base, "timeout-minutes: 30",
			"timeout-minutes: 30\n      continue-on-error: ${{ true }}", 1),
		"shell-retry": strings.Replace(base,
			"go test -race -coverprofile=coverage.out -json ./...",
			"for i in 1 2; do go test -race -coverprofile=coverage.out -json ./... && break; sleep 1; done", 1),
	}
	for name, txt := range cases {
		if err := ValidateWorkflow(txt); err == nil {
			t.Errorf("ValidateWorkflow(%s) = nil, want error", name)
		}
	}
}
