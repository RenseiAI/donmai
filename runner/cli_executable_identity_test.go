package runner

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/RenseiAI/donmai/agent"
	"github.com/RenseiAI/donmai/prompt"
	"github.com/RenseiAI/donmai/result"
	"github.com/RenseiAI/donmai/runtime/statehome"
	"github.com/RenseiAI/donmai/runtime/worktree"
)

func TestCLIExecutableIdentityConstructorsCaptureDefaultAndExplicit(t *testing.T) {
	statehome.ResetForTest()
	t.Cleanup(statehome.ResetForTest)
	base := t.TempDir()
	statehome.SetBaseHome(base)
	statehome.SetBrand("sample-local-a")

	newRunner := func(name string) *Runner {
		t.Helper()
		r, err := New(Options{Registry: NewRegistry(), WorktreeManager: &worktree.Manager{}, Poster: &result.Poster{}, CLIExecutableName: name})
		if err != nil {
			t.Fatal(err)
		}
		return r
	}
	explicitRunner := newRunner("sample-cli")
	explicitView, err := NewProviderViewWithOptions(NewRegistry(), ProviderViewOptions{CLIExecutableName: "sample-cli"})
	if err != nil {
		t.Fatal(err)
	}
	defaultRunner := newRunner("")
	defaultView, err := NewProviderViewWithOptions(NewRegistry(), ProviderViewOptions{})
	if err != nil {
		t.Fatal(err)
	}
	pathA := statehome.StateDir("sessions")
	statehome.SetBrand("sample-local-b")
	pathB := statehome.StateDir("sessions")

	if explicitRunner.cliExecutableName != "sample-cli" || explicitView.cliExecutableName != "sample-cli" {
		t.Fatalf("explicit identity changed: runner=%q view=%q", explicitRunner.cliExecutableName, explicitView.cliExecutableName)
	}
	if defaultRunner.cliExecutableName != "donmai" || defaultView.cliExecutableName != "donmai" {
		t.Fatalf("default identity changed: runner=%q view=%q", defaultRunner.cliExecutableName, defaultView.cliExecutableName)
	}
	if pathA == pathB || pathA != filepath.Join(base, ".sample-local-a", "sessions") || pathB != filepath.Join(base, ".sample-local-b", "sessions") {
		t.Fatalf("state paths = %q, %q", pathA, pathB)
	}
}

func TestCLIExecutableIdentityConstructorsRejectMalformedWithoutEcho(t *testing.T) {
	for _, name := range []string{"-sample", " sample", "sample ", "sample/name", `sample\name`, "sample;name", strings.Repeat("a", 129)} {
		t.Run(strings.NewReplacer(" ", "_", "/", "_").Replace(name), func(t *testing.T) {
			_, runnerErr := New(Options{Registry: NewRegistry(), WorktreeManager: &worktree.Manager{}, Poster: &result.Poster{}, CLIExecutableName: name})
			_, viewErr := NewProviderViewWithOptions(NewRegistry(), ProviderViewOptions{CLIExecutableName: name})
			for _, err := range []error{runnerErr, viewErr} {
				if err == nil || err.Error() != "runner: CLI executable name is malformed: must match [A-Za-z0-9][A-Za-z0-9._-]{0,127}" || strings.Contains(err.Error(), name) {
					t.Fatalf("malformed %q error = %v", name, err)
				}
			}
		})
	}
	if _, err := New(Options{CLIExecutableName: "bad/name"}); err == nil || err.Error() != "runner: Registry is required" {
		t.Fatalf("required collaborator error ordering changed: %v", err)
	}
}

func TestPreparedAndRetainedCLIExecutableIdentityAgreeOrRefuse(t *testing.T) {
	provider := mcpDeliveringHarness()
	harness := provider.(agent.HarnessProvider)
	selection := harnessSelection{Provider: provider}
	qw := platformGatewayWork("cli-identity-session")
	qw.Body = "exercise stable command identity"

	plan, source, err := compilePreparedHarnessWithProcessIdentity(qw, selection, nil, nil, "sample-platform", "sample-cli")
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(source)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(encoded), "sample-cli linear") || strings.Contains(string(encoded), "donmai linear") {
		t.Fatalf("prepared source has wrong command identity: %s", encoded)
	}

	recomputed, _, err := buildPreparedSourceSpecWithProcessIdentity(qw, selection, nil, "sample-platform", "sample-cli")
	if err != nil {
		t.Fatal(err)
	}
	recomputed.PreparedHarness = plan
	if _, err := agent.ApplyPreparedHarness(recomputed, harness.Manifest()); err != nil {
		t.Fatalf("matching retained identity refused: %v", err)
	}

	mismatch, _, err := buildPreparedSourceSpecWithProcessIdentity(qw, selection, nil, "sample-platform", "other-cli")
	if err != nil {
		t.Fatal(err)
	}
	mismatch.PreparedHarness = plan
	if _, err := agent.ApplyPreparedHarness(mismatch, harness.Manifest()); err == nil || !strings.Contains(err.Error(), "prompt") {
		t.Fatalf("mismatched retained identity was not refused with prompt drift: %v", err)
	}
}

func TestSteeringAndCodeIntelUseCapturedCLIExecutableIdentity(t *testing.T) {
	work := QueuedWork{}
	work.IssueIdentifier = "EX-3"
	work.WorkType = "development"
	steering := buildSteeringPrompt(work, streamObservation{}, "sample-cli")
	if !strings.Contains(steering, "`sample-cli linear create-comment`") || strings.Contains(steering, "donmai linear") {
		t.Fatalf("steering command identity = %q", steering)
	}
	partial := codeIntelUsagePartial(false, &prompt.CodeIntelWork{}, "sample-cli")
	if !strings.Contains(partial, "`sample-cli code ") || strings.Contains(partial, "`donmai code ") {
		t.Fatalf("code-intel guidance identity = %q", partial)
	}
	if got := codeIntelExecutableWithResolver("sample-cli", func() (string, error) { return "", errors.New("unavailable") }); got != "sample-cli" {
		t.Fatalf("code-intel fallback = %q", got)
	}
	if got := codeIntelExecutableWithResolver("sample-cli", func() (string, error) { return "/proc/self/exe", nil }); got != "/proc/self/exe" {
		t.Fatalf("real executable did not win: %q", got)
	}
	actual, err := os.Executable()
	if err == nil && actual != "" && codeIntelExecutable("sample-cli") != actual {
		t.Fatalf("production executable = %q, want %q", codeIntelExecutable("sample-cli"), actual)
	}
}
