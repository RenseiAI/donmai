// Package daemon — kit_skill_sources.go: surfaces the per-kit skill sources the
// runner needs to activate kit-provided SKILLS at runtime.
//
// The kits pivot (commit e5c5aee4) wired KitDetector + KitTargetOS into the
// runner-construction site but explicitly left runner.Options.KitSkillSources
// unwired because no registry method surfaced per-kit skill sources. This
// file closes that gap: SkillSourcesForRepo mirrors DetectForRepo's
// applicability rules (active + OS restriction + [detect] matchers — it reuses
// DetectForRepo for exactly that) and, for each applicable kit, surfaces its
// manifest path plus the skill files it declares under [provide.skills] as a
// []kit.KitSkillSource.
//
// internal/kit.LoadSkills (the runner's consumption layer, loop.go step 5a)
// reads those sources, resolves each skill file relative to its manifest
// directory, and injects the bodies + tool-disallow rules into the agent.
package daemon

import (
	"github.com/RenseiAI/donmai/internal/kit"
)

// SkillSourcesForRepo returns the kit skill sources for every active kit that
// applies to the repo rooted at repoRoot for the given targetOS. It mirrors
// DetectForRepo's applicability decision by reusing it, then — instead of
// toolchain demand — surfaces each applicable kit's manifest path and the
// skill files it declares under [provide.skills].
//
// The returned slice is handed to runner.Options.KitSkillSources at the
// runner-construction site so kit skills activate at loop step 5a. Kits that
// declare no skills are omitted. Ordering follows DetectForRepo (foundation →
// framework → project); each element also carries the kit's declared Priority
// so LoadSkills can order the merged system-prompt append deterministically.
//
// targetOS gates [supports].os exactly as DetectForRepo: pass the SANDBOX OS
// (linux for cloud); an empty targetOS falls back to the host OS.
//
// A detection error (e.g. a foundation-kit conflict, ErrKitFoundationConflict)
// is surfaced to the caller; on error the returned slice is nil. A repo with
// no applicable kits — or no applicable kits that declare skills — returns
// (nil, nil), which the runner treats as "no kit skills".
func (r *KitRegistry) SkillSourcesForRepo(repoRoot, targetOS string) ([]kit.KitSkillSource, error) {
	views, err := r.DetectForRepo(repoRoot, targetOS)
	if err != nil {
		return nil, err
	}
	if len(views) == 0 {
		return nil, nil
	}

	// Map kit.id → manifest index so we can resolve each detected kit's
	// [provide.skills] entries and the on-disk manifest path they resolve
	// relative to. scanWithPaths applies the same later-path-wins override
	// semantics DetectForRepo relies on, keeping the two views consistent.
	manifests, paths := r.scanWithPaths()
	byID := make(map[string]int, len(manifests))
	for i, m := range manifests {
		byID[m.Kit.ID] = i
	}

	out := make([]kit.KitSkillSource, 0, len(views))
	for _, v := range views {
		idx, ok := byID[v.ID]
		if !ok {
			continue
		}
		m := manifests[idx]
		if len(m.Provide.Skills) == 0 {
			continue
		}
		files := make([]string, 0, len(m.Provide.Skills))
		for _, s := range m.Provide.Skills {
			if s.File != "" {
				files = append(files, s.File)
			}
		}
		if len(files) == 0 {
			continue
		}
		out = append(out, kit.KitSkillSource{
			ID:           m.Kit.ID,
			Priority:     m.Kit.Priority,
			ManifestPath: paths[idx],
			SkillFiles:   files,
		})
	}
	if len(out) == 0 {
		return nil, nil
	}
	return out, nil
}

// PromptFragmentSourcesForRepo returns the kit prompt-fragment sources for
// every active kit that applies to the repo rooted at repoRoot for the given
// targetOS. It mirrors SkillSourcesForRepo's applicability decision (reuses
// DetectForRepo) but surfaces each applicable kit's manifest path and the
// [provide.prompt_fragments] entries — including their [when] workType filters.
//
// The returned slice is handed to runner.Options.KitPromptFragmentSources so
// the runner can inject workType-filtered fragment bodies into the system
// prompt at step 5a (alongside skill bodies). Kits that declare no
// prompt_fragments are omitted. Ordering follows DetectForRepo
// (foundation → framework → project).
//
// A detection error is surfaced to the caller; on error the returned slice is
// nil. A repo with no applicable kits — or no applicable kits that declare
// prompt_fragments — returns (nil, nil).
func (r *KitRegistry) PromptFragmentSourcesForRepo(repoRoot, targetOS string) ([]kit.KitPromptFragmentSource, error) {
	views, err := r.DetectForRepo(repoRoot, targetOS)
	if err != nil {
		return nil, err
	}
	if len(views) == 0 {
		return nil, nil
	}

	manifests, paths := r.scanWithPaths()
	byID := make(map[string]int, len(manifests))
	for i, m := range manifests {
		byID[m.Kit.ID] = i
	}

	out := make([]kit.KitPromptFragmentSource, 0, len(views))
	for _, v := range views {
		idx, ok := byID[v.ID]
		if !ok {
			continue
		}
		m := manifests[idx]
		if len(m.Provide.PromptFragments) == 0 {
			continue
		}
		frags := make([]kit.PromptFragmentEntry, 0, len(m.Provide.PromptFragments))
		for _, pf := range m.Provide.PromptFragments {
			if pf.File == "" {
				continue
			}
			frags = append(frags, kit.PromptFragmentEntry{
				Partial: pf.Partial,
				When:    copyStrings(pf.When),
				File:    pf.File,
			})
		}
		if len(frags) == 0 {
			continue
		}
		out = append(out, kit.KitPromptFragmentSource{
			ID:           m.Kit.ID,
			Priority:     m.Kit.Priority,
			ManifestPath: paths[idx],
			Fragments:    frags,
		})
	}
	if len(out) == 0 {
		return nil, nil
	}
	return out, nil
}
