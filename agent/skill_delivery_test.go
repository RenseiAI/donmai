package agent_test

import (
	"encoding/base64"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/RenseiAI/donmai/agent"
	"github.com/RenseiAI/donmai/stagewire"
)

// sealedSkill builds a valid package: SKILL.md plus extra files.
func sealedSkill(id string, delivery stagewire.SkillDelivery, extra ...stagewire.SkillFile) stagewire.Skill {
	files := append([]stagewire.SkillFile{
		agent.NewSkillFile("SKILL.md", []byte("---\nname: "+id+"\ndescription: test skill\n---\nDo the thing.\n"), 0o644),
	}, extra...)
	return agent.SealSkillPackage(stagewire.Skill{
		ID: id, Name: id, Description: "test skill", Version: "1.0.0",
		Files: files, Delivery: delivery, Body: "body of " + id,
		DisallowedTools: []string{"Bash(rm:*)"},
	})
}

var stateDirProfile = agent.SkillDeliveryProfile{Layout: agent.SkillLayoutStateDir, RepoSkills: agent.RepoSkillsByFlag}

func planOne(t *testing.T, skill stagewire.Skill, levels agent.ExecutionSecurityLevels) (agent.SkillDeliveryEntry, []stagewire.Skill, error) {
	t.Helper()
	delivery, residue, err := agent.PlanSkillDelivery(agent.SkillPlanInput{
		Harness: agent.HarnessPi, Profile: stateDirProfile, Mode: agent.PromptModeAutonomous,
		Levels: levels, Skills: []stagewire.Skill{skill},
	})
	if err != nil {
		return agent.SkillDeliveryEntry{}, nil, err
	}
	return delivery.Plan.Entries[0], residue, nil
}

// TestPlanSkillDelivery_RejectsUnsafePackages covers every package defect
// the plan refuses to deliver natively: path traversal, absolute and .git
// paths, symbolic links, case-folded duplicates, a file that is also a
// directory, a missing SKILL.md, content/hash/digest mismatches and an
// unsafe id. native_preferred falls back to the prompt with the reason;
// native_required refuses the spawn with a typed error.
func TestPlanSkillDelivery_RejectsUnsafePackages(t *testing.T) {
	t.Parallel()
	file := func(path string) stagewire.SkillFile { return agent.NewSkillFile(path, []byte("x"), 0o644) }
	cases := []struct {
		name   string
		skill  func() stagewire.Skill
		detail string
	}{
		{"parent traversal", func() stagewire.Skill { return sealedSkill("s", "", file("../escape.md")) }, "traverses"},
		{"inner traversal", func() stagewire.Skill { return sealedSkill("s", "", file("refs/../../escape.md")) }, "traverses"},
		{"absolute path", func() stagewire.Skill { return sealedSkill("s", "", file("/etc/passwd")) }, "absolute"},
		{"git entry", func() stagewire.Skill { return sealedSkill("s", "", file(".GIT/config")) }, ".git"},
		{"backslash", func() stagewire.Skill { return sealedSkill("s", "", file(`refs\x.md`)) }, "backslash"},
		{"marker file", func() stagewire.Skill { return sealedSkill("s", "", file(agent.SkillMarkerFile)) }, "marker"},
		{"symlink mode", func() stagewire.Skill {
			f := file("link")
			f.Mode = 0o120777
			return sealedSkill("s", "", f)
		}, "symbolic link"},
		{"case-folded duplicate", func() stagewire.Skill { return sealedSkill("s", "", file("a.md"), file("A.md")) }, "duplicate"},
		{"file is also a directory", func() stagewire.Skill { return sealedSkill("s", "", file("refs"), file("refs/a.md")) }, "also a directory"},
		{"missing SKILL.md", func() stagewire.Skill {
			return agent.SealSkillPackage(stagewire.Skill{ID: "s", Files: []stagewire.SkillFile{file("README.md")}, Body: "b"})
		}, "no SKILL.md"},
		{"content does not match sha256", func() stagewire.Skill {
			s := sealedSkill("s", "")
			s.Files[0].ContentBase64 = base64.StdEncoding.EncodeToString([]byte("tampered"))
			return s
		}, "sha256"},
		{"digest does not match files", func() stagewire.Skill {
			s := sealedSkill("s", "")
			s.Digest = "sha256:" + strings.Repeat("0", 64)
			return s
		}, "package digest"},
		{"unsafe id", func() stagewire.Skill { return sealedSkill("../s", "") }, "safe directory name"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			entry, residue, err := planOne(t, tc.skill(), agent.ExecutionSecurityLevels{})
			if err != nil {
				t.Fatalf("native_preferred must fall back, got error %v", err)
			}
			if entry.Delivery != agent.SkillDeliveredPrompt || entry.Reason != agent.SkillReasonInvalidPackage {
				t.Fatalf("entry = %+v, want prompt delivery for an invalid package", entry)
			}
			if !strings.Contains(entry.Detail, tc.detail) {
				t.Fatalf("Detail = %q, want it to name %q", entry.Detail, tc.detail)
			}
			if len(residue) != 1 {
				t.Fatalf("residue = %d skills, want the fallback body", len(residue))
			}

			required := tc.skill()
			required.Delivery = stagewire.SkillDeliveryNativeRequired
			_, _, err = planOne(t, required, agent.ExecutionSecurityLevels{})
			var deliveryErr *agent.SkillDeliveryError
			if !errors.As(err, &deliveryErr) || deliveryErr.Reason != agent.SkillReasonInvalidPackage {
				t.Fatalf("native_required: err = %v, want *SkillDeliveryError(invalid_package)", err)
			}
		})
	}
}

// TestPlanSkillDelivery_NativeEntryAndScripts pins a valid native entry's
// plan (sorted files, written modes) and the execution-security rule for
// scripts: withheld at allow-list tool approval, executable at bypass.
func TestPlanSkillDelivery_NativeEntryAndScripts(t *testing.T) {
	t.Parallel()
	skill := sealedSkill("review", "",
		agent.NewSkillFile("scripts/check.sh", []byte("#!/bin/sh\n"), 0o755),
		agent.NewSkillFile("refs/guide.md", []byte("guide"), 0o644),
	)
	cases := []struct {
		name         string
		levels       agent.ExecutionSecurityLevels
		wantFiles    []string
		wantWithheld []string
		wantModes    map[string]uint32
	}{
		{
			"no section reads as bypass",
			agent.ExecutionSecurityLevels{},
			[]string{"SKILL.md", "refs/guide.md", "scripts/check.sh"},
			nil,
			map[string]uint32{"SKILL.md": 0o644, "refs/guide.md": 0o644, "scripts/check.sh": 0o755},
		},
		{
			"deny-list keeps scripts",
			agent.ExecutionSecurityLevels{ToolApproval: agent.ToolApprovalDenyList},
			[]string{"SKILL.md", "refs/guide.md", "scripts/check.sh"},
			nil,
			map[string]uint32{"scripts/check.sh": 0o755},
		},
		{"allow-list withholds scripts", agent.ExecutionSecurityLevels{ToolApproval: agent.ToolApprovalAllowList}, []string{"SKILL.md", "refs/guide.md"}, []string{"scripts/check.sh"}, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			entry, residue, err := planOne(t, skill, tc.levels)
			if err != nil {
				t.Fatal(err)
			}
			if entry.Delivery != agent.SkillDeliveredNative || entry.Dir != "review" || len(residue) != 0 {
				t.Fatalf("entry = %+v residue=%d, want a native entry in dir review", entry, len(residue))
			}
			var got []string
			for _, f := range entry.Files {
				got = append(got, f.Path)
				if want, ok := tc.wantModes[f.Path]; ok && f.Mode != want {
					t.Errorf("%s mode = %04o, want %04o", f.Path, f.Mode, want)
				}
			}
			if !reflect.DeepEqual(got, tc.wantFiles) || !reflect.DeepEqual(entry.WithheldScripts, tc.wantWithheld) {
				t.Fatalf("files=%v withheld=%v, want %v / %v", got, entry.WithheldScripts, tc.wantFiles, tc.wantWithheld)
			}
		})
	}
}

// TestPlanSkillDelivery_PromptFallbackCap proves the prompt residue is
// capped: skills are taken in wire order while their bodies (and the blank
// line between them) fit, and the rest are omitted with a reason.
func TestPlanSkillDelivery_PromptFallbackCap(t *testing.T) {
	t.Parallel()
	body := strings.Repeat("a", 40)
	skills := []stagewire.Skill{
		{ID: "one", Body: body}, {ID: "two", Body: body}, {ID: "three", Body: "short"},
	}
	_, residue, err := agent.PlanSkillDelivery(agent.SkillPlanInput{
		Harness: agent.HarnessCodex, Mode: agent.PromptModeAutonomous, Skills: skills, PromptBudget: 90,
	})
	if err != nil {
		t.Fatal(err)
	}
	// one (40) + "\n\n" + two (40) = 82 fits; three would make 89 — fits;
	// shrink the budget to prove the cut.
	if len(residue) != 3 {
		t.Fatalf("budget 90: residue = %d, want 3", len(residue))
	}
	delivery, residue, err := agent.PlanSkillDelivery(agent.SkillPlanInput{
		Harness: agent.HarnessCodex, Mode: agent.PromptModeAutonomous, Skills: skills, PromptBudget: 85,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(residue) != 2 || residue[0].ID != "one" || residue[1].ID != "two" {
		t.Fatalf("budget 85: residue = %+v, want one and two", residue)
	}
	last := delivery.Plan.Entries[2]
	if last.Delivery != agent.SkillNotDelivered || last.Reason != agent.SkillReasonPromptCap {
		t.Fatalf("third entry = %+v, want omitted for the prompt cap", last)
	}

	// The default cap applies when no budget is given.
	huge := []stagewire.Skill{{ID: "big", Body: strings.Repeat("b", agent.MaxPromptSkillBytes+1)}}
	_, residue, err = agent.PlanSkillDelivery(agent.SkillPlanInput{Harness: agent.HarnessCodex, Skills: huge})
	if err != nil || len(residue) != 0 {
		t.Fatalf("over-cap body: residue=%d err=%v, want it omitted", len(residue), err)
	}
}

// TestPlanSkillDelivery_Routing covers the delivery decisions that do not
// depend on the package: prompt_only, a prompt-only harness, an interactive
// session, a skill with no files, an empty body, a duplicate id, and
// native_required on a harness without native delivery.
func TestPlanSkillDelivery_Routing(t *testing.T) {
	t.Parallel()
	native := sealedSkill("native", "")
	cases := []struct {
		name    string
		profile agent.SkillDeliveryProfile
		mode    agent.PromptSessionMode
		skills  []stagewire.Skill
		want    []agent.SkillDeliveryMethod
		reasons []agent.SkillDeliveryReason
	}{
		{
			"prompt_only", stateDirProfile, agent.PromptModeAutonomous,
			[]stagewire.Skill{func() stagewire.Skill { s := native; s.Delivery = stagewire.SkillDeliveryPromptOnly; return s }()},
			[]agent.SkillDeliveryMethod{agent.SkillDeliveredPrompt},
			[]agent.SkillDeliveryReason{agent.SkillReasonPromptOnly},
		},
		{
			"prompt-only harness",
			agent.SkillDeliveryProfile{},
			agent.PromptModeAutonomous,
			[]stagewire.Skill{native},
			[]agent.SkillDeliveryMethod{agent.SkillDeliveredPrompt},
			[]agent.SkillDeliveryReason{agent.SkillReasonHarnessPromptOnly},
		},
		{
			"interactive session", stateDirProfile, agent.PromptModeHumanControlled,
			[]stagewire.Skill{native},
			[]agent.SkillDeliveryMethod{agent.SkillDeliveredPrompt},
			[]agent.SkillDeliveryReason{agent.SkillReasonModePromptOnly},
		},
		{
			"no files", stateDirProfile, agent.PromptModeAutonomous,
			[]stagewire.Skill{{ID: "legacy", Body: "b"}},
			[]agent.SkillDeliveryMethod{agent.SkillDeliveredPrompt},
			[]agent.SkillDeliveryReason{agent.SkillReasonNoFiles},
		},
		{
			"empty body",
			agent.SkillDeliveryProfile{},
			agent.PromptModeAutonomous,
			[]stagewire.Skill{{ID: "blank", Body: "  \n"}},
			[]agent.SkillDeliveryMethod{agent.SkillNotDelivered},
			[]agent.SkillDeliveryReason{agent.SkillReasonNoBody},
		},
		{
			"duplicate id", stateDirProfile, agent.PromptModeAutonomous,
			[]stagewire.Skill{native, native},
			[]agent.SkillDeliveryMethod{agent.SkillDeliveredNative, agent.SkillNotDelivered},
			[]agent.SkillDeliveryReason{"", agent.SkillReasonDuplicateID},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			delivery, _, err := agent.PlanSkillDelivery(agent.SkillPlanInput{Harness: agent.HarnessPi, Profile: tc.profile, Mode: tc.mode, Skills: tc.skills})
			if err != nil {
				t.Fatal(err)
			}
			for i, entry := range delivery.Plan.Entries {
				if entry.Delivery != tc.want[i] || entry.Reason != tc.reasons[i] {
					t.Errorf("entry %d = %s/%s, want %s/%s", i, entry.Delivery, entry.Reason, tc.want[i], tc.reasons[i])
				}
			}
		})
	}

	required := native
	required.Delivery = stagewire.SkillDeliveryNativeRequired
	_, _, err := agent.PlanSkillDelivery(agent.SkillPlanInput{Harness: agent.HarnessCodex, Skills: []stagewire.Skill{required}})
	var deliveryErr *agent.SkillDeliveryError
	if !errors.As(err, &deliveryErr) || deliveryErr.Reason != agent.SkillReasonHarnessPromptOnly || deliveryErr.SkillID != "native" {
		t.Fatalf("native_required on a prompt-only harness: err = %v", err)
	}

	if delivery, residue, err := agent.PlanSkillDelivery(agent.SkillPlanInput{Harness: agent.HarnessPi}); delivery != nil || residue != nil || err != nil {
		t.Fatalf("no skills and no policy must plan nothing, got %v %v %v", delivery, residue, err)
	}
}

// TestPlanSkillDelivery_RepoSkillsReceipt pins the achieved repository-skill
// policy per harness control (D8.4: best-effort, recorded).
func TestPlanSkillDelivery_RepoSkillsReceipt(t *testing.T) {
	t.Parallel()
	native := sealedSkill("native", "")
	allow := &stagewire.RepoSkills{Policy: stagewire.RepoSkillsAllowlist, Allow: []string{"zeta", "alpha", "alpha", "../bad"}}
	none := &stagewire.RepoSkills{Policy: stagewire.RepoSkillsNone}
	cases := []struct {
		name    string
		profile agent.SkillDeliveryProfile
		mode    agent.PromptSessionMode
		skills  []stagewire.Skill
		policy  *stagewire.RepoSkills
		want    agent.RepoSkillsReceipt
		trust   bool
	}{
		{
			"flag harness achieves allowlist", stateDirProfile, agent.PromptModeAutonomous, nil, allow,
			agent.RepoSkillsReceipt{Requested: "allowlist", Allow: []string{"alpha", "zeta"}, Achieved: "allowlist"},
			false,
		},
		{
			"flag harness achieves none", stateDirProfile, agent.PromptModeAutonomous, nil, none,
			agent.RepoSkillsReceipt{Requested: "none", Achieved: "none"},
			false,
		},
		{
			"flag harness interactive cannot", stateDirProfile, agent.PromptModeHumanControlled, nil, none,
			agent.RepoSkillsReceipt{Requested: "none", Achieved: "all"},
			false,
		},
		{
			"uncontrolled harness records all",
			agent.SkillDeliveryProfile{Layout: agent.SkillLayoutPluginDir},
			agent.PromptModeAutonomous, nil, none,
			agent.RepoSkillsReceipt{Requested: "none", Achieved: "all"},
			false,
		},
		{
			"trust harness with native skill is all",
			agent.SkillDeliveryProfile{Layout: agent.SkillLayoutWorkspace, RepoSkills: agent.RepoSkillsByTrust},
			agent.PromptModeAutonomous,
			[]stagewire.Skill{native},
			none,
			agent.RepoSkillsReceipt{Requested: "none", Achieved: "all"},
			true,
		},
		{
			"trust harness without native skill is none",
			agent.SkillDeliveryProfile{Layout: agent.SkillLayoutWorkspace, RepoSkills: agent.RepoSkillsByTrust},
			agent.PromptModeAutonomous,
			nil, &stagewire.RepoSkills{},
			agent.RepoSkillsReceipt{Requested: "all", Achieved: "none"},
			false,
		},
		{
			"trusted host is all",
			agent.SkillDeliveryProfile{Layout: agent.SkillLayoutWorkspace, RepoSkills: agent.RepoSkillsByTrust, WorkspaceTrusted: true},
			agent.PromptModeAutonomous,
			nil, none,
			agent.RepoSkillsReceipt{Requested: "none", Achieved: "all"},
			false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			delivery, _, err := agent.PlanSkillDelivery(agent.SkillPlanInput{Harness: agent.HarnessMuse, Profile: tc.profile, Mode: tc.mode, Skills: tc.skills, RepoSkills: tc.policy})
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(delivery.Plan.RepoSkills, tc.want) || delivery.Plan.TrustWorkspace != tc.trust {
				t.Fatalf("receipt = %+v trust=%v, want %+v trust=%v", delivery.Plan.RepoSkills, delivery.Plan.TrustWorkspace, tc.want, tc.trust)
			}
		})
	}
}

// TestSkillPackageDigest_IsOrderIndependentAndBindsModes pins the digest
// form a control plane reproduces.
func TestSkillPackageDigest_IsOrderIndependentAndBindsModes(t *testing.T) {
	t.Parallel()
	a := agent.NewSkillFile("SKILL.md", []byte("s"), 0o644)
	b := agent.NewSkillFile("run.sh", []byte("r"), 0o755)
	if agent.SkillPackageDigest([]stagewire.SkillFile{a, b}) != agent.SkillPackageDigest([]stagewire.SkillFile{b, a}) {
		t.Fatal("digest depends on file order")
	}
	plain := b
	plain.Mode = 0o644
	if agent.SkillPackageDigest([]stagewire.SkillFile{a, b}) == agent.SkillPackageDigest([]stagewire.SkillFile{a, plain}) {
		t.Fatal("digest ignores the executable bit")
	}
	const want = "sha256:"
	if got := agent.SkillPackageDigest(nil); !strings.HasPrefix(got, want) || len(got) != len(want)+64 {
		t.Fatalf("digest form = %q", got)
	}
}

// TestSkillDelivery_NativeNeedsItsPackage proves a plan whose native entry
// lost its content is refused, never materialized half-way.
func TestSkillDelivery_NativeNeedsItsPackage(t *testing.T) {
	t.Parallel()
	delivery, _, err := agent.PlanSkillDelivery(agent.SkillPlanInput{
		Harness: agent.HarnessPi, Profile: stateDirProfile, Skills: []stagewire.Skill{sealedSkill("s", "")},
	})
	if err != nil {
		t.Fatal(err)
	}
	clone := delivery.Clone()
	clone.Packages = nil
	if _, err := clone.Native(); err == nil {
		t.Fatal("Native() accepted a native entry without its package")
	}
	if native, err := delivery.Native(); err != nil || len(native) != 1 {
		t.Fatalf("Native() = %d, %v", len(native), err)
	}
}
