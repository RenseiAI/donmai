package codex

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"sort"
	"strings"

	"github.com/RenseiAI/donmai/agent"
	"github.com/RenseiAI/donmai/runtime/skillfs"
	"github.com/RenseiAI/donmai/stagewire"
)

// Native skill delivery for Codex: one reserved directory per skill in the
// skills root of the Provider's private CODEX_HOME.
//
//	$CODEX_HOME/skills/donmai-<id>/SKILL.md (+ the rest of the package)
//
// Codex discovers $CODEX_HOME/skills at user scope whatever the trust of the
// working directory, with no flag and no feature switch (measured against
// codex-cli 0.164.0). The home is created per Provider, mode 0700, outside
// the working directory, and the app-server confirms it at initialize; the
// session's own sandbox can read a skill there but cannot write one. Three
// rules hold the delivery to the plan:
//
//   - Written before the app-server starts. A skill written after start is
//     missing from listings until codex's file watcher catches up, so the
//     Provider writes the session's skills at its first headless
//     Spawn/Resume, before startLocked, reads every file back against the
//     plan's sha256 (runtime/skillfs), and pins the set. A later
//     Spawn/Resume on the same app-server with the identical set re-reads
//     the files instead of rewriting them. Any other set cannot be told
//     apart from what the running app-server already loaded, so every
//     native skill of that session falls back to the prompt.
//   - Confirmed before thread/start. skills/list {cwds:[cwd],
//     forceReload:true} must list each SKILL.md path at user scope and
//     enabled. A skill it does not list (a SKILL.md codex could not parse,
//     a listing that failed) falls back to the prompt.
//   - Nothing in the working directory. The home is not under it, so no
//     exclude entry or marker is needed and nothing can enter a commit.
//
// A skill that falls back has its body appended to developerInstructions on
// a fresh thread, or ahead of the first turn's input on a resumed one, after
// the plan's own prompt residue and under the same agent.MaxPromptSkillBytes
// cap. A native_required skill that cannot be confirmed refuses the spawn.
//
// Codex loads the repository's .agents/skills and .codex/skills whatever
// the workspace trust, and this adapter does not narrow them, so the
// manifest declares no repository-skill control: the plan records the
// achieved policy as all (best-effort, D8.4).

// codexSkillProfile is the manifest's skill declaration.
var codexSkillProfile = agent.SkillDeliveryProfile{Layout: agent.SkillLayoutHarnessHome}

// codexSkillsRoot is the skills root under CODEX_HOME.
const codexSkillsRoot = "skills"

// skillMaterializer writes Spec.Skills' native entries into home, the
// Provider's private CODEX_HOME.
type skillMaterializer struct {
	home string
}

var _ agent.SkillMaterializer = skillMaterializer{}

// MaterializeSkills implements agent.SkillMaterializer: it writes and reads
// back every native entry and fails when any of them could not be. The
// Provider uses write and verify directly, so that a skill that fails falls
// back to the prompt on its own instead of refusing the session.
func (m skillMaterializer) MaterializeSkills(spec agent.Spec) (agent.MaterializedSkills, error) {
	native, err := nativeSkills(spec)
	if err != nil || len(native) == 0 {
		return agent.MaterializedSkills{}, err
	}
	written, fallback := m.write(native)
	cleanup := func() error { return m.remove(written) }
	if len(fallback) > 0 {
		errs := []error{cleanup()}
		for _, f := range fallback {
			errs = append(errs, fmt.Errorf("codex: skill %q: %s: %s", f.skill.Entry.ID, f.reason, f.detail))
		}
		return agent.MaterializedSkills{}, errors.Join(errs...)
	}
	return agent.MaterializedSkills{Cleanup: cleanup}, nil
}

// skillFallback is a native skill the adapter could not confirm, with why.
type skillFallback struct {
	skill  agent.NativeSkill
	reason agent.SkillDeliveryReason
	detail string
}

// stagedSkills is a session's native skills after staging in the home:
// written (or re-read) and awaiting the discovery check, or already
// falling back.
type stagedSkills struct {
	written  []agent.NativeSkill
	fallback []skillFallback
}

// nativeSkills returns spec's native entries with their packages, refusing
// a plan made for another harness's layout.
func nativeSkills(spec agent.Spec) ([]agent.NativeSkill, error) {
	native, err := spec.Skills.Native()
	if err != nil {
		return nil, fmt.Errorf("codex: %w", err)
	}
	if len(native) == 0 {
		return nil, nil
	}
	if layout := spec.Skills.Plan.Layout; layout != agent.SkillLayoutHarnessHome {
		return nil, fmt.Errorf("codex: skill plan layout %q is not this harness's", layout)
	}
	return native, nil
}

// skillRel is a native skill's directory relative to the home.
func skillRel(skill agent.NativeSkill) string {
	return codexSkillsRoot + "/" + skill.Entry.Dir
}

// skillManifestPath is the SKILL.md path codex lists for skill under home.
func skillManifestPath(home string, skill agent.NativeSkill) string {
	return filepath.Join(home, codexSkillsRoot, skill.Entry.Dir, "SKILL.md")
}

// write materializes each native skill and reads it back. A skill that
// fails the package checks, the write or the readback is left out, with
// whatever was created for it removed, and falls back.
func (m skillMaterializer) write(native []agent.NativeSkill) ([]agent.NativeSkill, []skillFallback) {
	var written []agent.NativeSkill
	var fallback []skillFallback
	sessionBytes := 0
	for _, skill := range native {
		size, reason, err := checkNativeSkill(skill, sessionBytes)
		if err != nil {
			fallback = append(fallback, skillFallback{skill: skill, reason: reason, detail: err.Error()})
			continue
		}
		if err := skillfs.Write(m.home, skillRel(skill), skill, skillfs.Options{Replace: true}); err != nil {
			fallback = append(fallback, skillFallback{skill: skill, reason: agent.SkillReasonNativeUnconfirmed, detail: "write: " + err.Error()})
			continue
		}
		sessionBytes += size
		written = append(written, skill)
	}
	return written, fallback
}

// verify re-reads skills an earlier launch wrote against the plan. A
// skill that is missing or changed is removed, so codex cannot keep
// loading the changed copy, and falls back.
func (m skillMaterializer) verify(native []agent.NativeSkill) ([]agent.NativeSkill, []skillFallback) {
	var verified []agent.NativeSkill
	var fallback []skillFallback
	for _, skill := range native {
		if err := skillfs.Verify(m.home, skillRel(skill), skill.Entry); err != nil {
			detail := "readback: " + err.Error()
			if rmErr := skillfs.Remove(m.home, skillRel(skill)); rmErr != nil {
				detail += "; remove: " + rmErr.Error()
			}
			fallback = append(fallback, skillFallback{skill: skill, reason: agent.SkillReasonNativeUnconfirmed, detail: detail})
			continue
		}
		verified = append(verified, skill)
	}
	return verified, fallback
}

func (m skillMaterializer) remove(skills []agent.NativeSkill) error {
	var errs []error
	for _, skill := range skills {
		if err := skillfs.Remove(m.home, skillRel(skill)); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// checkNativeSkill applies the planner's package rules to one native entry
// again before anything is written: the directory is the reserved single
// segment for the skill's id, every file path is safe, and the package and
// the session stay under the size caps. The planner refuses such a package
// already; this keeps a plan whose content travelled apart from it out of
// the home. It returns the package's decoded size.
func checkNativeSkill(skill agent.NativeSkill, sessionBytes int) (int, agent.SkillDeliveryReason, error) {
	entry := skill.Entry
	if entry.Dir != agent.SkillDirName(agent.SkillLayoutHarnessHome, entry.ID) || strings.Contains(entry.Dir, "/") || agent.ValidateSkillPath(entry.Dir) != nil {
		return 0, agent.SkillReasonInvalidPackage, errors.New("directory is not the reserved name for the skill id")
	}
	if len(entry.Files) > agent.MaxSkillFiles {
		return 0, agent.SkillReasonInvalidPackage, fmt.Errorf("more than %d files", agent.MaxSkillFiles)
	}
	size := 0
	for _, file := range entry.Files {
		if err := agent.ValidateSkillPath(file.Path); err != nil {
			return 0, agent.SkillReasonInvalidPackage, fmt.Errorf("file %q: %w", file.Path, err)
		}
		content, err := agent.SkillFileContent(skill.Package, file.Path)
		if err != nil {
			return 0, agent.SkillReasonInvalidPackage, err
		}
		size += len(content)
		if size > agent.MaxSkillBytes {
			return 0, agent.SkillReasonInvalidPackage, fmt.Errorf("content larger than %d bytes", agent.MaxSkillBytes)
		}
	}
	if sessionBytes+size > agent.MaxSessionSkillBytes {
		return 0, agent.SkillReasonSessionCap, fmt.Errorf("session content larger than %d bytes", agent.MaxSessionSkillBytes)
	}
	return size, "", nil
}

// skillSetDigest identifies a native skill set by each entry's directory and
// planned files (path, mode, sha256), in directory order.
func skillSetDigest(native []agent.NativeSkill) string {
	type pinned struct {
		Dir   string                   `json:"dir"`
		Files []agent.SkillPlannedFile `json:"files"`
	}
	set := make([]pinned, 0, len(native))
	for _, skill := range native {
		set = append(set, pinned{Dir: skill.Entry.Dir, Files: skill.Entry.Files})
	}
	sort.Slice(set, func(i, j int) bool { return set[i].Dir < set[j].Dir })
	raw, _ := json.Marshal(set)
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

// stageSkillsLocked writes or re-reads spec's native skills in the private
// home. startMu must be held, and it must run before startLocked: only a
// set written before the app-server started is one it is known to load.
func (p *Provider) stageSkillsLocked(spec agent.Spec) (stagedSkills, error) {
	native, err := nativeSkills(spec)
	if err != nil {
		return stagedSkills{}, err
	}
	set := skillSetDigest(native)
	if !p.started {
		p.skillSetPinned, p.pinnedSkillSet = true, set
		if len(native) == 0 {
			return stagedSkills{}, nil
		}
		written, fallback := skillMaterializer{home: p.config.home}.write(native)
		return stagedSkills{written: written, fallback: fallback}, nil
	}
	if len(native) == 0 {
		return stagedSkills{}, nil
	}
	if !p.skillSetPinned || set != p.pinnedSkillSet {
		return stagedSkills{fallback: fallbackAll(native, "the app-server was started with a different skill set")}, nil
	}
	verified, fallback := skillMaterializer{home: p.config.home}.verify(native)
	return stagedSkills{written: verified, fallback: fallback}, nil
}

func fallbackAll(native []agent.NativeSkill, detail string) []skillFallback {
	out := make([]skillFallback, 0, len(native))
	for _, skill := range native {
		out = append(out, skillFallback{skill: skill, reason: agent.SkillReasonNativeUnconfirmed, detail: detail})
	}
	return out
}

// deliverSkills confirms the staged skills with the app-server and puts
// every skill it cannot confirm on the prompt. It runs after the app-server
// started and before thread/start or thread/resume.
func (p *Provider) deliverSkills(ctx context.Context, spec agent.Spec, staged stagedSkills, resume bool) (agent.Spec, error) {
	confirmed, unlisted := p.confirmSkillDiscovery(ctx, spec.Cwd, staged.written)
	if len(confirmed) > 0 {
		slog.Info("codex: native skills delivered", "skills", skillIDs(confirmed))
	}
	return applySkillFallback(spec, append(append([]skillFallback(nil), staged.fallback...), unlisted...), resume)
}

// confirmSkillDiscovery asks the app-server to rescan its skill roots for
// cwd and splits staged into the skills it lists as loaded user skills and
// those it does not. A failed listing confirms nothing.
func (p *Provider) confirmSkillDiscovery(ctx context.Context, cwd string, staged []agent.NativeSkill) ([]agent.NativeSkill, []skillFallback) {
	if len(staged) == 0 {
		return nil, nil
	}
	params := map[string]any{"forceReload": true}
	if cwd != "" {
		params["cwds"] = []string{cwd}
	}
	raw, err := p.client.RequestWithRetry(ctx, "skills/list", params, p.opts.RPCTimeout)
	var listed []string
	if err == nil {
		listed, err = loadedUserSkillPaths(raw)
	}
	if err != nil {
		return nil, fallbackAll(staged, skillListFailureDetail(err))
	}
	var confirmed []agent.NativeSkill
	var unlisted []skillFallback
	for _, skill := range staged {
		want := skillManifestPath(p.config.home, skill)
		found := false
		for _, path := range listed {
			if sameResolvedPath(path, want) {
				found = true
				break
			}
		}
		if !found {
			unlisted = append(unlisted, skillFallback{skill: skill, reason: agent.SkillReasonNativeUnconfirmed, detail: "skills/list did not list it as an enabled user skill"})
			continue
		}
		confirmed = append(confirmed, skill)
	}
	return confirmed, unlisted
}

// loadedUserSkillPaths returns the SKILL.md path of every enabled
// user-scope skill in a skills/list response.
func loadedUserSkillPaths(raw json.RawMessage) ([]string, error) {
	var response struct {
		Data []struct {
			Skills []struct {
				Path    string `json:"path"`
				Enabled bool   `json:"enabled"`
				Scope   string `json:"scope"`
			} `json:"skills"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &response); err != nil {
		return nil, fmt.Errorf("decode skills/list: %w", err)
	}
	var out []string
	for _, entry := range response.Data {
		for _, skill := range entry.Skills {
			if skill.Enabled && skill.Scope == "user" && skill.Path != "" {
				out = append(out, skill.Path)
			}
		}
	}
	return out, nil
}

// skillListFailureDetail names a failed listing without the server's
// message, which may echo request values.
func skillListFailureDetail(err error) string {
	var rpc *RPCError
	if errors.As(err, &rpc) {
		return fmt.Sprintf("skills/list rejected by JSON-RPC code %d", rpc.Code)
	}
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		return "skills/list did not answer"
	}
	return "skills/list failed"
}

// applySkillFallback puts the body of every skill that could not be
// confirmed natively on the prompt, in plan order: appended to
// developerInstructions (SystemPromptAppend) for a fresh thread, ahead of
// the first turn's input (InitialContext) for a resumed one, whose
// developer instructions were fixed when it started. The bodies share
// agent.MaxPromptSkillBytes with the plan's own prompt residue; a skill
// that does not fit, or has no body, is not delivered. A native_required
// skill refuses the spawn instead.
func applySkillFallback(spec agent.Spec, fallback []skillFallback, resume bool) (agent.Spec, error) {
	if len(fallback) == 0 || spec.Skills == nil {
		return spec, nil
	}
	order := make(map[string]int, len(spec.Skills.Plan.Entries))
	for i, entry := range spec.Skills.Plan.Entries {
		order[entry.ID] = i
	}
	sort.SliceStable(fallback, func(i, j int) bool {
		return order[fallback[i].skill.Entry.ID] < order[fallback[j].skill.Entry.ID]
	})
	for _, f := range fallback {
		if f.skill.Entry.Requested == stagewire.SkillDeliveryNativeRequired {
			return spec, &agent.SkillDeliveryError{SkillID: f.skill.Entry.ID, Reason: f.reason, Detail: f.detail}
		}
	}
	used := promptResidueBytes(spec.Skills.Plan)
	var bodies []string
	for _, f := range fallback {
		body := f.skill.Package.Body
		cost := len(body)
		if used > 0 {
			cost += len("\n\n")
		}
		args := []any{"skill", f.skill.Entry.ID, "reason", string(f.reason), "detail", f.detail}
		switch {
		case strings.TrimSpace(body) == "":
			slog.Warn("codex: native skill not delivered", append(args, "fallback", string(agent.SkillReasonNoBody))...)
		case used+cost > agent.MaxPromptSkillBytes:
			slog.Warn("codex: native skill not delivered", append(args, "fallback", string(agent.SkillReasonPromptCap))...)
		default:
			used += cost
			bodies = append(bodies, body)
			slog.Warn("codex: native skill falls back to the prompt", args...)
		}
	}
	if len(bodies) == 0 {
		return spec, nil
	}
	text := strings.Join(bodies, "\n\n")
	if resume {
		spec.InitialContext = joinPromptParts(text, spec.InitialContext)
	} else {
		spec.SystemPromptAppend = joinPromptParts(spec.SystemPromptAppend, text)
	}
	return spec, nil
}

// promptResidueBytes is what the plan's own prompt entries already spent of
// agent.MaxPromptSkillBytes, counted the way the planner counts it.
func promptResidueBytes(plan agent.SkillDeliveryPlan) int {
	used := 0
	for _, entry := range plan.Entries {
		if entry.Delivery != agent.SkillDeliveredPrompt {
			continue
		}
		if used > 0 {
			used += len("\n\n")
		}
		used += entry.PromptBytes
	}
	return used
}

func joinPromptParts(first, second string) string {
	switch {
	case first == "":
		return second
	case second == "":
		return first
	}
	return first + "\n\n" + second
}

func skillIDs(skills []agent.NativeSkill) []string {
	out := make([]string, len(skills))
	for i, skill := range skills {
		out[i] = skill.Entry.ID
	}
	return out
}
