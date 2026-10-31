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
		"exit-single": strings.Replace(base,
			"go test -race -coverprofile=coverage.out -json ./...",
			"go test -race -coverprofile=coverage.out -json ./...; exit 0", 1),
		"exit-tab": strings.Replace(base,
			"go test -race -coverprofile=coverage.out -json ./...",
			"go test -race -coverprofile=coverage.out -json ./...; exit\t0", 1),
		"exit-double-space": strings.Replace(base,
			"go test -race -coverprofile=coverage.out -json ./...",
			"go test -race -coverprofile=coverage.out -json ./...; exit  0", 1),
		"semicolon-bin-true": strings.Replace(base,
			"go test -race -coverprofile=coverage.out -json ./...",
			"go test -race -coverprofile=coverage.out -json ./...; /bin/true", 1),
		"semicolon-usr-bin-true": strings.Replace(base,
			"go test -race -coverprofile=coverage.out -json ./...",
			"go test -race -coverprofile=coverage.out -json ./...; /usr/bin/true", 1),
		"and-bin-true": strings.Replace(base,
			"go test -race -coverprofile=coverage.out -json ./...",
			"go test -race -coverprofile=coverage.out -json ./... && /bin/true", 1),
		"set-plus-e": strings.Replace(base,
			"set -o pipefail\n          go test -race -coverprofile=coverage.out -json ./...",
			"set -o pipefail\n          set +e\n          go test -race -coverprofile=coverage.out -json ./...", 1),
	}
	for name, txt := range cases {
		if err := ValidateWorkflow(txt); err == nil {
			t.Errorf("ValidateWorkflow(%s) = nil, want error", name)
		}
	}
}

func TestValidateWorkflowRejectsPipeMasking(t *testing.T) {
	base := repoWorkflow(t)
	cases := map[string]string{
		"pipe-cat": strings.Replace(base, "timeout-minutes: 30",
			"timeout-minutes: 30\n      - name: Extra pipe\n        run: go test -race -coverprofile=coverage.out ./... | cat", 1),
		"pipe-tee": strings.Replace(base, "timeout-minutes: 30",
			"timeout-minutes: 30\n      - name: Extra pipe\n        run: go test -race -coverprofile=coverage.out ./... | tee out.log", 1),
	}
	for name, txt := range cases {
		if err := ValidateWorkflow(txt); err == nil {
			t.Errorf("ValidateWorkflow(%s) = nil, want error", name)
		}
	}
}

func TestValidateWorkflowRejectsAlwaysRerun(t *testing.T) {
	base := repoWorkflow(t)
	withRerun := strings.Replace(base, "timeout-minutes: 30",
		"timeout-minutes: 30\n      - name: Follow-up checks\n        if: always()\n        run: go test -race -coverprofile=coverage.out -json ./...", 1)
	if err := ValidateWorkflow(withRerun); err == nil {
		t.Fatal("ValidateWorkflow with if: always() rerun = nil, want error")
	}
	// cancelled() does not run after a failure, so it must stay allowed.
	withCancelled := strings.Replace(base, "timeout-minutes: 30",
		"timeout-minutes: 30\n      - name: Cleanup\n        if: cancelled()\n        run: echo cleanup", 1)
	if err := ValidateWorkflow(withCancelled); err != nil {
		t.Fatalf("ValidateWorkflow with if: cancelled() cleanup = %v, want nil", err)
	}
}

func TestValidateWorkflowRejectsMakeTestRerun(t *testing.T) {
	base := repoWorkflow(t)
	cases := map[string]string{
		"failure-make-test": strings.Replace(base, "timeout-minutes: 30",
			"timeout-minutes: 30\n      - name: Follow-up checks\n        if: failure()\n        run: make test", 1),
		"always-make-test": strings.Replace(base, "timeout-minutes: 30",
			"timeout-minutes: 30\n      - name: Follow-up checks\n        if: always()\n        run: make test", 1),
	}
	for name, txt := range cases {
		if err := ValidateWorkflow(txt); err == nil {
			t.Errorf("ValidateWorkflow(%s) = nil, want error", name)
		}
	}
}

func TestValidateWorkflowRejectsConfinementRunRemoval(t *testing.T) {
	base := repoWorkflow(t)
	macosRun := "go test -race -count=1 -timeout 15m -v ./runtime/confinement/..."
	if !strings.Contains(base, macosRun) {
		t.Fatalf("repo workflow missing macos run %q", macosRun)
	}
	withoutMacosRun := strings.Replace(base, macosRun, "echo holding-for-macos", 1)
	if err := ValidateWorkflow(withoutMacosRun); err == nil {
		t.Error("ValidateWorkflow without macos confinement run = nil, want error")
	}
	linuxRun := "go test -race -count=1 -timeout 15m -v -run 'TestLinux_|TestLandlockStage_|TestLandlockRuleset_|TestCheck_' ./runtime/confinement/"
	if !strings.Contains(base, linuxRun) {
		t.Fatalf("repo workflow missing linux run %q", linuxRun)
	}
	withoutLinuxRun := strings.Replace(base, linuxRun, "echo holding-for-linux", 1)
	if err := ValidateWorkflow(withoutLinuxRun); err == nil {
		t.Error("ValidateWorkflow without linux confinement run = nil, want error")
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

func TestValidateWorkflowRejectsJobLevelRerun(t *testing.T) {
	base := repoWorkflow(t)
	// A rerun job gated on failure()/always() re-executes the suite even
	// though the condition sits at job level, away from any test chunk.
	cases := map[string]string{
		"failure-needs": base + "\n  rerun-tests:\n    needs: test\n    if: failure()\n    runs-on: ubuntu-latest\n    steps:\n      - name: Rerun tests\n        run: go test -race ./...\n",
		"always-needs":  base + "\n  rerun-tests:\n    needs: test\n    if: always()\n    runs-on: ubuntu-latest\n    steps:\n      - name: Rerun tests\n        run: go test -race ./...\n",
	}
	for name, txt := range cases {
		if err := ValidateWorkflow(txt); err == nil {
			t.Errorf("ValidateWorkflow(%s) = nil, want error", name)
		}
	}
}

func TestValidateWorkflowRejectsSetPlusAnywhere(t *testing.T) {
	base := repoWorkflow(t)
	// `set +` disarms errexit wherever it sits, even in a step that never
	// names the suite and even with non-single spacing.
	cases := map[string]string{
		"set-plus-e-alone": strings.Replace(base, "timeout-minutes: 30",
			"timeout-minutes: 30\n      - name: Loosen shell\n        run: set +e", 1),
		"set-plus-double-space": strings.Replace(base, "timeout-minutes: 30",
			"timeout-minutes: 30\n      - name: Loosen shell\n        run: set  +e", 1),
	}
	for name, txt := range cases {
		if err := ValidateWorkflow(txt); err == nil {
			t.Errorf("ValidateWorkflow(%s) = nil, want error", name)
		}
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
