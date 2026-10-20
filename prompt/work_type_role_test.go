package prompt_test

import (
	"testing"

	"github.com/RenseiAI/donmai/prompt"
	"github.com/RenseiAI/donmai/runner"
	"github.com/RenseiAI/donmai/templates"
)

// TestBuilderBuild_CustomWorkTypeFollowsRole pins the role-aware user
// template selection for organisation-defined names: a custom name with
// role review renders byte-identically to qa, with role intake to
// research, and with role inflight (or no/invalid role) to development.
// Named built-ins keep their template regardless of any role passed, so
// a stray role can never move a built-in off its legacy prompt. Both
// rendering paths (legacy text/template and the raymond shim) are
// covered.
func TestBuilderBuild_CustomWorkTypeFollowsRole(t *testing.T) {
	t.Parallel()

	reg, err := templates.New()
	if err != nil {
		t.Fatalf("templates.New: %v", err)
	}
	builders := []struct {
		name string
		new  func() *prompt.Builder
	}{
		{name: "legacy", new: prompt.NewBuilder},
		{name: "raymond", new: func() *prompt.Builder { return &prompt.Builder{Registry: reg} }},
	}

	// reference renders the named built-in verbatim; custom renders the
	// same session with an organisation-defined name plus a role. The
	// user prompts must be byte-identical.
	cases := []struct {
		name      string
		reference string // named built-in work type
		custom    string // organisation-defined name
		role      string
	}{
		{name: "review-renders-qa", reference: string(prompt.WorkTypeQA), custom: "peer-review", role: "review"},
		{name: "intake-renders-research", reference: string(prompt.WorkTypeResearch), custom: "triage-desk", role: "intake"},
		{name: "inflight-renders-development", reference: string(prompt.WorkTypeDevelopment), custom: "chief-of-staff", role: "inflight"},
		{name: "terminal-renders-development", reference: string(prompt.WorkTypeDevelopment), custom: "garden-tend", role: "terminal"},
		{name: "absent-role-renders-development", reference: string(prompt.WorkTypeDevelopment), custom: "chief-of-staff", role: ""},
		{name: "invalid-role-renders-development", reference: string(prompt.WorkTypeDevelopment), custom: "chief-of-staff", role: "not-a-role"},
	}
	for _, builder := range builders {
		for _, tc := range cases {
			t.Run(builder.name+"/"+tc.name, func(t *testing.T) {
				t.Parallel()
				reference := fixtureSession()
				reference.WorkType = tc.reference
				custom := fixtureSession()
				custom.WorkType = tc.custom
				custom.WorkTypeRole = tc.role

				_, wantUser, err := builder.new().Build(reference)
				if err != nil {
					t.Fatalf("Build reference: %v", err)
				}
				_, gotUser, err := builder.new().Build(custom)
				if err != nil {
					t.Fatalf("Build custom: %v", err)
				}
				if gotUser != wantUser {
					t.Errorf("custom work type %q with role %q differs from %q\n--- want ---\n%s\n--- got ---\n%s",
						tc.custom, tc.role, tc.reference, wantUser, gotUser)
				}
			})
		}
	}
}

// TestBuilderBuild_NamedWorkTypeIgnoresRole proves the built-in vocabulary
// is authoritative: passing any role alongside a compiled-in name renders
// byte-identically to the role-less build. It iterates every
// runner.AllWorkTypes entry (not just the four switch names) so a stray or
// mismatched role — including the platform-seeded intake/review roles on
// scanner and auditor types — can never move a built-in off its legacy
// prompt (the RED case is a built-in moving).
func TestBuilderBuild_NamedWorkTypeIgnoresRole(t *testing.T) {
	t.Parallel()

	reg, err := templates.New()
	if err != nil {
		t.Fatalf("templates.New: %v", err)
	}
	builders := []struct {
		name string
		new  func() *prompt.Builder
	}{
		{name: "legacy", new: prompt.NewBuilder},
		{name: "raymond", new: func() *prompt.Builder { return &prompt.Builder{Registry: reg} }},
	}
	// Roles are spelled through the canonical runner constants so a
	// renamed wire literal is exercised, not a stale copy.
	roles := []string{
		"",
		string(runner.WorkTypeRoleInflight),
		string(runner.WorkTypeRoleReview),
		string(runner.WorkTypeRoleIntake),
		string(runner.WorkTypeRoleTerminal),
		"not-a-role",
	}
	for _, builder := range builders {
		for _, workType := range runner.AllWorkTypes {
			for _, role := range roles {
				t.Run(builder.name+"/"+workType+"/role="+role, func(t *testing.T) {
					t.Parallel()
					plain := fixtureSession()
					plain.WorkType = workType
					withRole := fixtureSession()
					withRole.WorkType = workType
					withRole.WorkTypeRole = role

					_, wantUser, err := builder.new().Build(plain)
					if err != nil {
						t.Fatalf("Build plain: %v", err)
					}
					_, gotUser, err := builder.new().Build(withRole)
					if err != nil {
						t.Fatalf("Build with role: %v", err)
					}
					if gotUser != wantUser {
						t.Errorf("built-in work type %q with role %q moved off its legacy prompt", workType, role)
					}
				})
			}
		}
	}
}

// TestBuilderBuild_RoleLiteralsMatchCanonicalRoles pins the prompt's role
// wire literals equal to the runner's canonical WorkTypeRole constants: a
// custom name carrying string(runner.WorkTypeRoleReview) renders
// byte-identically to qa, Intake to research, and Inflight/Terminal to
// development. Driven through the canonical constants (never re-spelled
// literals), so renaming a constant on either side without updating the
// other goes RED. Both rendering paths are covered.
func TestBuilderBuild_RoleLiteralsMatchCanonicalRoles(t *testing.T) {
	t.Parallel()

	reg, err := templates.New()
	if err != nil {
		t.Fatalf("templates.New: %v", err)
	}
	builders := []struct {
		name string
		new  func() *prompt.Builder
	}{
		{name: "legacy", new: prompt.NewBuilder},
		{name: "raymond", new: func() *prompt.Builder { return &prompt.Builder{Registry: reg} }},
	}
	cases := []struct {
		name      string
		reference string
		role      runner.WorkTypeRole
	}{
		{name: "review", reference: string(prompt.WorkTypeQA), role: runner.WorkTypeRoleReview},
		{name: "intake", reference: string(prompt.WorkTypeResearch), role: runner.WorkTypeRoleIntake},
		{name: "inflight", reference: string(prompt.WorkTypeDevelopment), role: runner.WorkTypeRoleInflight},
		{name: "terminal", reference: string(prompt.WorkTypeDevelopment), role: runner.WorkTypeRoleTerminal},
	}
	for _, builder := range builders {
		for _, tc := range cases {
			t.Run(builder.name+"/"+tc.name, func(t *testing.T) {
				t.Parallel()
				reference := fixtureSession()
				reference.WorkType = tc.reference
				custom := fixtureSession()
				custom.WorkType = "peer-review-desk"
				custom.WorkTypeRole = string(tc.role)

				_, wantUser, err := builder.new().Build(reference)
				if err != nil {
					t.Fatalf("Build reference: %v", err)
				}
				_, gotUser, err := builder.new().Build(custom)
				if err != nil {
					t.Fatalf("Build custom: %v", err)
				}
				if gotUser != wantUser {
					t.Errorf("custom work type with canonical role %q differs from %q", string(tc.role), tc.reference)
				}
			})
		}
	}
}

// TestBuilderBuild_BuiltinLegacyTemplateValue pins the legacy template VALUE
// for every compiled-in work-type name: with no role stamped, qa and
// acceptance render byte-identically to the qa prompt, research to the
// research prompt, and every other built-in to the development prompt, on
// both rendering paths (legacy text/template and the raymond shim). The
// role-invariance test proves a stamped role cannot move a built-in; this
// test proves the role-less value itself cannot move (changing the known-name
// fallback from development to qa stays GREEN without it).
func TestBuilderBuild_BuiltinLegacyTemplateValue(t *testing.T) {
	t.Parallel()

	reg, err := templates.New()
	if err != nil {
		t.Fatalf("templates.New: %v", err)
	}
	builders := []struct {
		name string
		new  func() *prompt.Builder
	}{
		{name: "legacy", new: prompt.NewBuilder},
		{name: "raymond", new: func() *prompt.Builder { return &prompt.Builder{Registry: reg} }},
	}
	legacyReference := func(workType string) string {
		switch workType {
		case string(prompt.WorkTypeQA), string(prompt.WorkTypeAcceptance):
			return string(prompt.WorkTypeQA)
		case string(prompt.WorkTypeResearch):
			return string(prompt.WorkTypeResearch)
		default:
			return string(prompt.WorkTypeDevelopment)
		}
	}
	buildUser := func(t *testing.T, newBuilder func() *prompt.Builder, workType string) string {
		t.Helper()
		qw := fixtureSession()
		qw.WorkType = workType
		qw.WorkTypeRole = ""
		_, user, err := newBuilder().Build(qw)
		if err != nil {
			t.Fatalf("Build %q: %v", workType, err)
		}
		return user
	}
	for _, builder := range builders {
		t.Run(builder.name+"/representatives-distinct", func(t *testing.T) {
			t.Parallel()
			qa := buildUser(t, builder.new, string(prompt.WorkTypeQA))
			research := buildUser(t, builder.new, string(prompt.WorkTypeResearch))
			development := buildUser(t, builder.new, string(prompt.WorkTypeDevelopment))
			if qa == development {
				t.Errorf("qa and development user prompts are identical; legacy values collapsed")
			}
			if research == development {
				t.Errorf("research and development user prompts are identical; legacy values collapsed")
			}
			if qa == research {
				t.Errorf("qa and research user prompts are identical; legacy values collapsed")
			}
		})
		for _, workType := range runner.AllWorkTypes {
			t.Run(builder.name+"/"+workType, func(t *testing.T) {
				t.Parallel()
				got := buildUser(t, builder.new, workType)
				want := buildUser(t, builder.new, legacyReference(workType))
				if got != want {
					t.Errorf("built-in work type %q renders differently from its legacy %q template", workType, legacyReference(workType))
				}
			})
		}
	}
}
