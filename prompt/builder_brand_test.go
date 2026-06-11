package prompt_test

import (
	"strings"
	"testing"

	"github.com/RenseiAI/donmai/prompt"
	"github.com/RenseiAI/donmai/runtime/statehome"
	"github.com/RenseiAI/donmai/templates"
)

// brandFixture returns a representative QueuedWork for the brand-rendering
// assertions. It mirrors the canonical session fixture but is self-contained so
// the brand tests do not depend on the parallel golden suite's fixture state.
func brandFixture(workType prompt.WorkType) prompt.QueuedWork {
	return prompt.QueuedWork{
		SessionID:       "0b5e88d9-32d0-4aca-9f8c-caf82f2b399c",
		IssueIdentifier: "DEV-1",
		ProjectName:     "smoke-alpha",
		OrganizationID:  "org_ejkmv9ojdyifipydw5l1",
		Repository:      "github.com/RenseiAI/rensei-smokes-alpha",
		Ref:             "main",
		WorkType:        string(workType),
		PromptContext:   "<issue identifier=\"DEV-1\"><title>brand check</title></issue>",
	}
}

// TestBuilderBuild_Brand_OSSDefault asserts the OSS default brand seam
// (statehome brand "donmai") renders the donmai binary's own brand in both the
// system identity line and the Linear CLI command examples — never the closed
// "Rensei"/"rensei" brand. This is the OSS-hygiene contract: a bare donmai
// build must not leak the platform brand.
//
// It is NON-parallel and resets the process-global statehome seam because it
// shares that seam with TestBuilderBuild_Brand_PlatformContract; Go runs all
// non-parallel tests to completion before resuming any t.Parallel() test, so
// these brand tests never overlap the parallel golden suite.
func TestBuilderBuild_Brand_OSSDefault(t *testing.T) {
	statehome.ResetForTest()
	t.Cleanup(statehome.ResetForTest)

	// No SetBrand call — the default OSS brand ("donmai") applies.
	b := prompt.NewBuilder()
	system, user, err := b.Build(brandFixture(prompt.WorkTypeDevelopment))
	if err != nil {
		t.Fatalf("Build error: %v", err)
	}

	if !strings.Contains(system, "autonomous Donmai agent") {
		t.Errorf("system prompt missing OSS brand display %q\nsystem=%q", "autonomous Donmai agent", system)
	}
	if !strings.Contains(system, "`donmai linear`") {
		t.Errorf("system prompt missing OSS brand CLI %q\nsystem=%q", "`donmai linear`", system)
	}
	if !strings.Contains(user, "`donmai linear create-comment`") {
		t.Errorf("user prompt missing OSS brand CLI %q\nuser=%q", "`donmai linear create-comment`", user)
	}

	// The platform brand must NOT leak into a default OSS render.
	for _, leak := range []string{"autonomous Rensei agent", "rensei linear"} {
		if strings.Contains(system, leak) {
			t.Errorf("OSS render leaked closed brand %q\nsystem=%q", leak, system)
		}
		if strings.Contains(user, leak) {
			t.Errorf("OSS render leaked closed brand %q\nuser=%q", leak, user)
		}
	}
}

// TestBuilderBuild_Brand_PlatformContract is the load-bearing platform
// invariant: when the closed rensei binary configures the statehome seam with
// brand "rensei" (statehome.SetBrand("rensei")), the donmai runner's default
// templates render byte-identically to the pre-brand-seam templates — i.e.
// "autonomous Rensei agent" and "rensei linear". The platform's fallback prompt
// path (system_base.tmpl when no SystemPromptOverride is set) and the
// fail-loud guard depend on this exact wording, so the brand seam must be a
// no-op in effect for the platform.
//
// NON-parallel + statehome reset for the same process-global reason as
// TestBuilderBuild_Brand_OSSDefault.
func TestBuilderBuild_Brand_PlatformContract(t *testing.T) {
	statehome.ResetForTest()
	t.Cleanup(statehome.ResetForTest)

	statehome.SetBrand("rensei")

	t.Run("legacy_path", func(t *testing.T) {
		b := prompt.NewBuilder()
		system, user, err := b.Build(brandFixture(prompt.WorkTypeDevelopment))
		if err != nil {
			t.Fatalf("Build error: %v", err)
		}
		assertPlatformBrand(t, system, user)
	})

	t.Run("raymond_path", func(t *testing.T) {
		reg, err := templates.New()
		if err != nil {
			t.Fatalf("templates.New() error: %v", err)
		}
		b := &prompt.Builder{Registry: reg}
		system, user, err := b.Build(brandFixture(prompt.WorkTypeDevelopment))
		if err != nil {
			t.Fatalf("raymond Build error: %v", err)
		}
		assertPlatformBrand(t, system, user)
	})
}

// assertPlatformBrand checks the closed-brand contract on a rendered
// (system, user) pair: the platform wording is present, the OSS wording is
// absent, and the Wave-2a rule-6 / WORK_RESULT:blocked decline content is
// intact (the runner's scanBlocked detector keys off it, so the brand seam
// must not perturb it).
func assertPlatformBrand(t *testing.T, system, user string) {
	t.Helper()

	if !strings.Contains(system, "autonomous Rensei agent") {
		t.Errorf("platform render missing %q\nsystem=%q", "autonomous Rensei agent", system)
	}
	if !strings.Contains(system, "`rensei linear`") {
		t.Errorf("platform render missing %q\nsystem=%q", "`rensei linear`", system)
	}
	if !strings.Contains(user, "`rensei linear create-comment`") {
		t.Errorf("platform render missing %q\nuser=%q", "`rensei linear create-comment`", user)
	}

	// The OSS default brand must NOT appear on the platform path.
	for _, leak := range []string{"autonomous Donmai agent", "donmai linear"} {
		if strings.Contains(system, leak) {
			t.Errorf("platform render leaked OSS brand %q\nsystem=%q", leak, system)
		}
		if strings.Contains(user, leak) {
			t.Errorf("platform render leaked OSS brand %q\nuser=%q", leak, user)
		}
	}

	// Wave-2a rule-6 / blocked-decline content must survive the brand seam.
	if !strings.Contains(system, "WORK_RESULT:blocked") {
		t.Errorf("rule-6 WORK_RESULT:blocked missing from system prompt: %q", system)
	}
	if !strings.Contains(system, "AGENT_BLOCKED:") {
		t.Errorf("rule-6 AGENT_BLOCKED marker missing from system prompt: %q", system)
	}
	for _, marker := range []string{"WORK_RESULT:passed", "WORK_RESULT:failed", "WORK_RESULT:blocked"} {
		if !strings.Contains(user, marker) {
			t.Errorf("development contract marker %q missing from user prompt: %q", marker, user)
		}
	}
}
