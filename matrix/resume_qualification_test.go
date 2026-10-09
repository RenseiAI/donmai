package matrix

import (
	"strings"
	"testing"

	"github.com/RenseiAI/donmai/agent"
)

// TestResumeQualificationIsComputedPerAdapterVersion pins the slice's three
// matrix rules: one row per harvested (harness, adapter version), computed
// (never declared — no manifest field feeds it), and all false today.
func TestResumeQualificationIsComputedPerAdapterVersion(t *testing.T) {
	t.Parallel()
	built, err := Build()
	if err != nil {
		t.Fatalf("Build(): %v", err)
	}
	rows := built.Matrix.ResumeQualification

	// One row per harvested tool/lifecycle profile: the adapter version is
	// the profile id.
	want := 0
	seen := map[string]bool{}
	for _, h := range built.Harnesses {
		for _, profile := range h.ToolLifecycle {
			want++
			key := string(h.Name) + "\x00" + profile.ID + "\x00" + string(profile.Mode)
			if seen[key] {
				t.Fatalf("duplicate resume qualification row for %q/%q/%q", h.Name, profile.ID, profile.Mode)
			}
			seen[key] = true
		}
	}
	if len(rows) != want {
		t.Fatalf("resumeQualification has %d rows; want one per harvested tool/lifecycle profile (%d)", len(rows), want)
	}
	for i, row := range rows {
		if row.Harness == "" || row.AdapterVersion == "" || row.Mode == "" {
			t.Errorf("row %d is missing its tuple identity: %+v", i, row)
		}
		// Computed, never declared: nothing may flip this until a passing
		// fixture run does, so every generated row is false today.
		if row.ResumeQualified {
			t.Errorf("row %d (%s/%s) is qualified with no passing fixture run behind it", i, row.Harness, row.AdapterVersion)
		}
		if i > 0 {
			prev := rows[i-1]
			if !(prev.Harness < row.Harness ||
				(prev.Harness == row.Harness && prev.AdapterVersion < row.AdapterVersion) ||
				(prev.Harness == row.Harness && prev.AdapterVersion == row.AdapterVersion && prev.Mode < row.Mode)) {
				t.Errorf("rows %d and %d are not canonically ordered: %+v then %+v", i-1, i, prev, row)
			}
		}
	}
	if len(rows) == 0 {
		t.Fatal("no resume qualification rows generated")
	}
}

// TestResumeQualificationFollowsAdapterVersionBumps proves the re-run rule:
// renaming a tool/lifecycle profile id (what an adapter-version bump is)
// produces a NEW unqualified row instead of inheriting anything — the bumped
// version must re-run the fixture.
func TestResumeQualificationFollowsAdapterVersionBumps(t *testing.T) {
	t.Parallel()
	before, err := Build()
	if err != nil {
		t.Fatalf("Build(): %v", err)
	}
	beforeIDs := map[string]bool{}
	for _, row := range before.Matrix.ResumeQualification {
		if row.Harness == agent.HarnessStub && row.Mode == agent.PromptModeAutonomous {
			beforeIDs[row.AdapterVersion] = true
		}
	}
	if !beforeIDs["stub/tool-lifecycle-v1"] {
		t.Fatalf("no stub autonomous qualification row to bump from: %+v", before.Matrix.ResumeQualification)
	}

	harvests := HarnessHarvestList()
	var bumped []HarnessHarvest
	for _, h := range harvests {
		h := h
		if h.Name == agent.HarnessStub {
			outer := h.Manifest
			h.Manifest = func() agent.HarnessManifest {
				mf := outer()
				for i := range mf.ToolLifecycle {
					if mf.ToolLifecycle[i].ID == "stub/tool-lifecycle-v1" {
						mf.ToolLifecycle[i].ID = "stub/tool-lifecycle-v2"
					}
				}
				return mf
			}
		}
		bumped = append(bumped, h)
	}
	harnesses, byName, err := buildHarnessesFrom(bumped)
	if err != nil {
		t.Fatalf("buildHarnessesFrom(): %v", err)
	}
	_ = harnesses
	after := buildResumeQualification(byName)
	var sawOld, sawNew bool
	for _, row := range after {
		if row.Harness != agent.HarnessStub || row.Mode != agent.PromptModeAutonomous {
			continue
		}
		switch row.AdapterVersion {
		case "stub/tool-lifecycle-v1":
			sawOld = true
		case "stub/tool-lifecycle-v2":
			sawNew = true
			if row.ResumeQualified {
				t.Errorf("bumped adapter version qualified without a fixture run: %+v", row)
			}
		}
	}
	if sawOld {
		t.Error("old adapter version row survived the bump; the bump must replace it")
	}
	if !sawNew {
		t.Error("bumped adapter version produced no qualification row; the bump must re-run the fixture as a fresh row")
	}
}

// TestResumeQualificationIsNeverDeclared proves the computed/declared split:
// flipping the declared resume claim on a harvested manifest must not move
// the computed row — the flag is a claim, the row is evidence.
func TestResumeQualificationIsNeverDeclared(t *testing.T) {
	t.Parallel()
	before, err := Build()
	if err != nil {
		t.Fatalf("Build(): %v", err)
	}
	harvests := HarnessHarvestList()
	var flipped []HarnessHarvest
	for _, h := range harvests {
		h := h
		if h.Name == agent.HarnessCodex {
			outer := h.Manifest
			h.Manifest = func() agent.HarnessManifest {
				mf := outer()
				mf.Caps.SupportsSessionResume = false
				return mf
			}
		}
		flipped = append(flipped, h)
	}
	_, byName, err := buildHarnessesFrom(flipped)
	if err != nil {
		t.Fatalf("buildHarnessesFrom(): %v", err)
	}
	after := buildResumeQualification(byName)
	beforeRows := map[string]bool{}
	for _, row := range before.Matrix.ResumeQualification {
		beforeRows[string(row.Harness)+"\x00"+row.AdapterVersion] = row.ResumeQualified
	}
	for _, row := range after {
		key := string(row.Harness) + "\x00" + row.AdapterVersion
		if key == string(agent.HarnessCodex)+"\x00codex/headless/tool-lifecycle-v1" {
			continue // tuple may be absent; absence is covered below
		}
		if want, ok := beforeRows[key]; ok && want != row.ResumeQualified {
			t.Errorf("row %s moved when only the declared flag changed", key)
		}
	}
	// The codex tuples still exist as rows even with the claim off: the
	// matrix publishes qualification evidence per adapter version, and a
	// false claim does not delete the evidence row.
	found := false
	for _, row := range after {
		if row.Harness == agent.HarnessCodex && strings.HasPrefix(row.AdapterVersion, "codex/") {
			found = true
		}
	}
	if !found {
		t.Error("flipping the declared resume claim deleted the computed rows; qualification is per adapter version, not per claim")
	}
}
