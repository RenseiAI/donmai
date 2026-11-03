package agent

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/RenseiAI/donmai/stagewire"
)

// Skill delivery: getting a work item's skills (stagewire.Skill) to the exact
// harness natively, per ADR-2026-11-02-stages-roles-and-stage-cards.md D8.
//
// It splits the way every other adaptation channel here does:
//
//   - PlanSkillDelivery is PURE. From the harness's declared
//     SkillDeliveryProfile, the session mode, the execution-security levels
//     and the skills themselves it decides, per skill, native or prompt
//     delivery (or neither, with a reason), validates every package it would
//     deliver natively, and renders the repository-skill policy as far as
//     the harness allows. The host compile and the child spawn run it on the
//     same input and get the same plan; the plan joins the harness authority
//     digest (prepared_harness.go, field "skills").
//   - A SkillMaterializer, one per harness adapter, writes the plan's native
//     entries in that harness's layout and reads every file back against the
//     plan's sha256 before the child starts (the TOCTOU rule
//     extension_delivery.go applies to extensions), then hands the adapter
//     the argv the harness needs.
//   - Skills the plan leaves on the prompt are the residue the runner folds
//     into the system prompt exactly as before (runner foldInlineSkills),
//     under a size cap.

// SkillDeliveryContractVersion identifies the plan shape.
const SkillDeliveryContractVersion = "skill-delivery/v1"

// Caps applied while planning. A package over a cap is not delivered
// natively; a prompt residue over MaxPromptSkillBytes drops the skills that
// do not fit.
const (
	// MaxSkillFiles caps the files in one package.
	MaxSkillFiles = 256
	// MaxSkillBytes caps one package's decoded content.
	MaxSkillBytes = 4 << 20
	// MaxSessionSkillBytes caps the decoded content of every package one
	// session delivers natively.
	MaxSessionSkillBytes = 16 << 20
	// MaxSkillPathBytes caps one file path inside a package.
	MaxSkillPathBytes = 256
	// MaxSkillPathDepth caps the directory depth of a file path.
	MaxSkillPathDepth = 8
	// MaxPromptSkillBytes caps the skill bodies the prompt fallback appends.
	// The system prompt travels as ONE argv element for some harnesses, and
	// Linux refuses an element over 128 KiB; this leaves room for the rest
	// of the system prompt.
	MaxPromptSkillBytes = 64 << 10
)

// InTreeSkillDirPrefix is the reserved directory-name prefix for a skill the
// runner writes inside the working directory (.agents/skills/donmai-<id>).
// The checkout's exclude file and the backstop both key on it
// (runtime/harnessstate), so such a directory never enters a commit.
const InTreeSkillDirPrefix = "donmai-"

// SkillMarkerFile is written beside a skill the runner materializes inside
// the working directory. A later materialization may replace a directory
// that carries it (a stale copy from an earlier run); a directory without it
// is repository content and is never overwritten.
const SkillMarkerFile = ".donmai-skill"

// SkillLayout names where a harness's materializer writes skills.
type SkillLayout string

// The layouts. The zero value means the harness has no native delivery:
// every skill goes to the prompt.
const (
	// SkillLayoutPluginDir is a per-session plugin directory outside the
	// working directory, passed to the harness by flag.
	SkillLayoutPluginDir SkillLayout = "plugin_dir"
	// SkillLayoutStateDir is one directory per skill under the harness's
	// session state root, each passed to the harness by flag.
	SkillLayoutStateDir SkillLayout = "state_dir"
	// SkillLayoutWorkspace is one reserved .agents/skills/donmai-<id>
	// directory per skill inside the working directory, discovered by the
	// harness once the workspace is trusted.
	SkillLayoutWorkspace SkillLayout = "workspace"
)

// RepoSkillsControl names how far a harness can govern repository skills.
type RepoSkillsControl string

// The controls. The zero value means the harness discovers repository
// skills and cannot be told not to: the achieved policy is always all.
const (
	// RepoSkillsByFlag means the harness can turn discovery off and load
	// named skill directories by path, so every policy is achievable.
	RepoSkillsByFlag RepoSkillsControl = "flag"
	// RepoSkillsByTrust means repository skills load exactly when the
	// workspace is trusted, so the achieved policy is all or none.
	RepoSkillsByTrust RepoSkillsControl = "workspace_trust"
)

// SkillDeliveryProfile is a harness manifest's skill declaration.
type SkillDeliveryProfile struct {
	// Layout is where native skills go; empty for prompt-only harnesses.
	Layout SkillLayout `json:"layout,omitempty"`
	// RepoSkills is how far the harness can govern repository skills.
	RepoSkills RepoSkillsControl `json:"repoSkills,omitempty"`
	// WorkspaceTrusted reports a harness instance that trusts every
	// workspace already (an operator option), which loads repository
	// skills regardless of the plan. Only meaningful with RepoSkillsByTrust.
	WorkspaceTrusted bool `json:"workspaceTrusted,omitempty"`
}

// SkillDeliveryMethod is how one skill reaches the harness.
type SkillDeliveryMethod string

// The methods.
const (
	SkillDeliveredNative SkillDeliveryMethod = "native"
	SkillDeliveredPrompt SkillDeliveryMethod = "prompt"
	SkillNotDelivered    SkillDeliveryMethod = "omitted"
)

// SkillDeliveryReason explains a skill not delivered natively. Closed set.
type SkillDeliveryReason string

// The reasons.
const (
	SkillReasonPromptOnly        SkillDeliveryReason = "prompt_only"
	SkillReasonHarnessPromptOnly SkillDeliveryReason = "harness_prompt_only"
	SkillReasonModePromptOnly    SkillDeliveryReason = "session_mode_prompt_only"
	SkillReasonNoFiles           SkillDeliveryReason = "no_files"
	SkillReasonInvalidPackage    SkillDeliveryReason = "invalid_package"
	SkillReasonSessionCap        SkillDeliveryReason = "session_cap_exceeded"
	SkillReasonDuplicateID       SkillDeliveryReason = "duplicate_id"
	SkillReasonNoBody            SkillDeliveryReason = "no_body"
	SkillReasonPromptCap         SkillDeliveryReason = "prompt_cap_exceeded"
)

// SkillPlannedFile is one file a native entry writes.
type SkillPlannedFile struct {
	Path string `json:"path"`
	// Mode is the permission the file is written with: 0644, or 0755 for
	// an executable the session may run.
	Mode   uint32 `json:"mode"`
	SHA256 string `json:"sha256"`
}

// Executable reports whether the planned file is written executable.
func (f SkillPlannedFile) Executable() bool { return f.Mode&0o111 != 0 }

// SkillDeliveryEntry is the plan for one skill. It carries identity and
// digests only, never content.
type SkillDeliveryEntry struct {
	ID         string                    `json:"id,omitempty"`
	Name       string                    `json:"name,omitempty"`
	Version    string                    `json:"version,omitempty"`
	Digest     string                    `json:"digest,omitempty"`
	Invocation stagewire.SkillInvocation `json:"invocation,omitempty"`
	Requested  stagewire.SkillDelivery   `json:"requested"`
	Delivery   SkillDeliveryMethod       `json:"delivery"`
	Reason     SkillDeliveryReason       `json:"reason,omitempty"`
	// Detail names what made a package invalid (never content).
	Detail string `json:"detail,omitempty"`
	// Dir is the directory name a native entry is written to.
	Dir string `json:"dir,omitempty"`
	// Files are the files a native entry writes.
	Files []SkillPlannedFile `json:"files,omitempty"`
	// WithheldScripts are package files not written because the session's
	// execution-security level does not let it run them.
	WithheldScripts []string `json:"withheldScripts,omitempty"`
	// PromptBytes is the body length a prompt entry appends.
	PromptBytes int `json:"promptBytes,omitempty"`
}

// RepoSkillsReceipt records the repository-skill policy asked for and the
// policy the harness actually achieves (D8.4: best-effort per harness).
type RepoSkillsReceipt struct {
	Requested stagewire.RepoSkillsPolicy `json:"requested"`
	Allow     []string                   `json:"allow,omitempty"`
	Achieved  stagewire.RepoSkillsPolicy `json:"achieved"`
}

// SkillDeliveryPlan is the pure delivery decision for one session.
type SkillDeliveryPlan struct {
	ContractVersion string               `json:"contractVersion"`
	Harness         HarnessName          `json:"harness"`
	Layout          SkillLayout          `json:"layout,omitempty"`
	ScriptsAllowed  bool                 `json:"scriptsAllowed"`
	Entries         []SkillDeliveryEntry `json:"entries"`
	RepoSkills      RepoSkillsReceipt    `json:"repoSkills"`
	// TrustWorkspace is set when native delivery needs the harness to
	// trust the workspace (SkillLayoutWorkspace with a native entry).
	TrustWorkspace bool `json:"trustWorkspace,omitempty"`
}

// SkillDelivery rides on Spec.Skills: the plan, plus the content of every
// skill the plan delivers natively.
type SkillDelivery struct {
	Plan     SkillDeliveryPlan `json:"plan"`
	Packages []stagewire.Skill `json:"packages,omitempty"`
}

// NativeSkill pairs a native plan entry with its package.
type NativeSkill struct {
	Entry   SkillDeliveryEntry
	Package stagewire.Skill
}

// Native returns the native entries in plan order with their packages. A
// native entry without its package is an error: the plan and the content
// travelled apart.
func (d *SkillDelivery) Native() ([]NativeSkill, error) {
	if d == nil {
		return nil, nil
	}
	byID := make(map[string]stagewire.Skill, len(d.Packages))
	for _, pkg := range d.Packages {
		byID[pkg.ID] = pkg
	}
	var out []NativeSkill
	for _, entry := range d.Plan.Entries {
		if entry.Delivery != SkillDeliveredNative {
			continue
		}
		pkg, ok := byID[entry.ID]
		if !ok {
			return nil, fmt.Errorf("agent: skill %q is planned native but its package is missing", entry.ID)
		}
		out = append(out, NativeSkill{Entry: entry, Package: pkg})
	}
	return out, nil
}

// HasNative reports whether any skill is delivered natively.
func (d *SkillDelivery) HasNative() bool {
	if d == nil {
		return false
	}
	for _, entry := range d.Plan.Entries {
		if entry.Delivery == SkillDeliveredNative {
			return true
		}
	}
	return false
}

// Clone returns a deep copy.
func (d *SkillDelivery) Clone() *SkillDelivery {
	if d == nil {
		return nil
	}
	out := &SkillDelivery{Plan: d.Plan, Packages: stagewire.CloneSkills(d.Packages)}
	out.Plan.Entries = make([]SkillDeliveryEntry, len(d.Plan.Entries))
	for i, entry := range d.Plan.Entries {
		entry.Files = append([]SkillPlannedFile(nil), entry.Files...)
		entry.WithheldScripts = append([]string(nil), entry.WithheldScripts...)
		out.Plan.Entries[i] = entry
	}
	out.Plan.RepoSkills.Allow = append([]string(nil), d.Plan.RepoSkills.Allow...)
	return out
}

// SkillMaterializer is the per-harness half of skill delivery. An adapter
// whose manifest declares a SkillLayout implements it; MaterializeSkills
// writes the native entries of spec.Skills in the harness's layout, verifies
// every file against the plan after writing it, and returns the argv the
// harness needs. It is called on every Spawn and Resume, so a resumed
// session re-verifies (and, where the layout allows, rewrites) what it
// loads rather than trusting an earlier run.
type SkillMaterializer interface {
	MaterializeSkills(spec Spec) (MaterializedSkills, error)
}

// MaterializedSkills is what a materializer hands its adapter.
type MaterializedSkills struct {
	// Args is the argv fragment the harness needs, in order.
	Args []string
	// TrustWorkspace asks the adapter to trust the workspace.
	TrustWorkspace bool
	// Cleanup removes what was written; nil when nothing needs removing.
	Cleanup func() error
}

// SkillDeliveryError refuses a spawn whose skills cannot be delivered as
// the dispatch requires. It names the skill and the reason, never content.
type SkillDeliveryError struct {
	SkillID string
	Reason  SkillDeliveryReason
	Detail  string
}

func (e *SkillDeliveryError) Error() string {
	msg := fmt.Sprintf("agent: skill %q requires native delivery but %s", e.SkillID, e.Reason)
	if e.Detail != "" {
		msg += ": " + e.Detail
	}
	return msg
}

// SkillPlanInput is everything PlanSkillDelivery reads.
type SkillPlanInput struct {
	Harness    HarnessName
	Profile    SkillDeliveryProfile
	Mode       PromptSessionMode
	Levels     ExecutionSecurityLevels
	Skills     []stagewire.Skill
	RepoSkills *stagewire.RepoSkills
	// PromptBudget caps the prompt residue; zero means MaxPromptSkillBytes.
	PromptBudget int
}

// PlanSkillDelivery decides how each skill reaches the harness. It returns
// nil when the work item carries neither skills nor a repository policy, and
// otherwise the delivery for Spec.Skills plus the prompt residue: the skills,
// in wire order, whose Body the runner appends to the system prompt. A skill
// whose delivery is native_required and which cannot be delivered natively
// refuses the spawn with a *SkillDeliveryError. Pure: no I/O.
func PlanSkillDelivery(in SkillPlanInput) (*SkillDelivery, []stagewire.Skill, error) {
	if len(in.Skills) == 0 && in.RepoSkills == nil {
		return nil, nil, nil
	}
	nativeLane := in.Profile.Layout != "" && in.Mode != PromptModeHumanControlled
	plan := SkillDeliveryPlan{
		ContractVersion: SkillDeliveryContractVersion,
		Harness:         in.Harness,
		ScriptsAllowed:  SkillScriptsAllowed(in.Levels),
		Entries:         make([]SkillDeliveryEntry, 0, len(in.Skills)),
	}
	if nativeLane {
		plan.Layout = in.Profile.Layout
	}
	budget := in.PromptBudget
	if budget <= 0 {
		budget = MaxPromptSkillBytes
	}
	delivery := &SkillDelivery{}
	var residue []stagewire.Skill
	seen := make(map[string]bool, len(in.Skills))
	promptBytes, sessionBytes := 0, 0
	for _, skill := range in.Skills {
		entry := SkillDeliveryEntry{
			ID: skill.ID, Name: skill.Name, Version: skill.Version, Digest: skill.Digest,
			Invocation: skill.Invocation, Requested: skill.Delivery.Effective(),
		}
		if skill.ID != "" && seen[skill.ID] {
			entry.Delivery, entry.Reason = SkillNotDelivered, SkillReasonDuplicateID
			plan.Entries = append(plan.Entries, entry)
			continue
		}
		seen[skill.ID] = true
		reason, detail := nativeRefusal(skill, entry.Requested, nativeLane, in.Profile.Layout)
		if reason == "" {
			files, withheld, size, err := validateSkillPackage(skill, plan.ScriptsAllowed)
			switch {
			case err != nil:
				reason, detail = SkillReasonInvalidPackage, err.Error()
			case sessionBytes+size > MaxSessionSkillBytes:
				reason = SkillReasonSessionCap
			default:
				sessionBytes += size
				entry.Delivery = SkillDeliveredNative
				entry.Dir = SkillDirName(in.Profile.Layout, skill.ID)
				entry.Files, entry.WithheldScripts = files, withheld
				delivery.Packages = append(delivery.Packages, skill.Clone())
				plan.Entries = append(plan.Entries, entry)
				continue
			}
		}
		if entry.Requested == stagewire.SkillDeliveryNativeRequired {
			return nil, nil, &SkillDeliveryError{SkillID: skill.ID, Reason: reason, Detail: detail}
		}
		entry.Reason, entry.Detail = reason, detail
		cost := len(skill.Body)
		if promptBytes > 0 {
			cost += len("\n\n")
		}
		switch {
		case strings.TrimSpace(skill.Body) == "":
			entry.Delivery, entry.Reason = SkillNotDelivered, SkillReasonNoBody
		case promptBytes+cost > budget:
			entry.Delivery, entry.Reason = SkillNotDelivered, SkillReasonPromptCap
		default:
			promptBytes += cost
			entry.Delivery, entry.PromptBytes = SkillDeliveredPrompt, len(skill.Body)
			residue = append(residue, skill)
		}
		plan.Entries = append(plan.Entries, entry)
	}
	plan.TrustWorkspace = plan.Layout == SkillLayoutWorkspace && len(delivery.Packages) > 0
	plan.RepoSkills = repoSkillsReceipt(in.Profile, in.RepoSkills, nativeLane, plan.TrustWorkspace)
	delivery.Plan = plan
	return delivery, residue, nil
}

// nativeRefusal returns why skill is not delivered natively before its
// package is examined, or "" when it may be.
func nativeRefusal(skill stagewire.Skill, requested stagewire.SkillDelivery, nativeLane bool, layout SkillLayout) (SkillDeliveryReason, string) {
	switch {
	case requested == stagewire.SkillDeliveryPromptOnly:
		return SkillReasonPromptOnly, ""
	case layout == "":
		return SkillReasonHarnessPromptOnly, ""
	case !nativeLane:
		return SkillReasonModePromptOnly, ""
	case len(skill.Files) == 0:
		return SkillReasonNoFiles, ""
	}
	return "", ""
}

// repoSkillsReceipt renders the repository-skill policy as far as the
// harness allows (D8.4). trusted reports a plan that trusts the workspace.
func repoSkillsReceipt(profile SkillDeliveryProfile, policy *stagewire.RepoSkills, nativeLane, trusted bool) RepoSkillsReceipt {
	receipt := RepoSkillsReceipt{Requested: stagewire.RepoSkillsAll}
	if policy != nil {
		receipt.Requested = policy.Policy.Effective()
		if receipt.Requested == stagewire.RepoSkillsAllowlist {
			receipt.Allow = normalizeRepoSkillAllow(policy.Allow)
		}
	}
	switch {
	case profile.RepoSkills == RepoSkillsByTrust:
		receipt.Achieved = stagewire.RepoSkillsNone
		if trusted || profile.WorkspaceTrusted {
			receipt.Achieved = stagewire.RepoSkillsAll
		}
	case profile.RepoSkills == RepoSkillsByFlag && nativeLane:
		receipt.Achieved = receipt.Requested
	default:
		receipt.Achieved = stagewire.RepoSkillsAll
	}
	return receipt
}

// normalizeRepoSkillAllow keeps the allowlist names that are safe path
// segments, sorted and de-duplicated. A name that could not be a skill
// directory can never match one.
func normalizeRepoSkillAllow(in []string) []string {
	seen := make(map[string]bool, len(in))
	out := make([]string, 0, len(in))
	for _, name := range in {
		if !skillIDPattern.MatchString(name) || seen[name] {
			continue
		}
		seen[name] = true
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// SkillDirName is the directory a native skill is written to for layout.
func SkillDirName(layout SkillLayout, id string) string {
	if layout == SkillLayoutWorkspace {
		return InTreeSkillDirPrefix + id
	}
	return id
}

// SkillScriptsAllowed reports whether a session at levels may receive a
// skill's executable files. A session whose tool approval is allow-list or
// stricter has had what it runs narrowed by its operator, so a skill cannot
// bring unreviewed executables into it; at deny-list or bypass the agent can
// already run any command, and the scripts add nothing it could not write.
func SkillScriptsAllowed(levels ExecutionSecurityLevels) bool {
	index, ok := ExecutionSecurityLevelIndex(ExecutionSecurityToolApproval, levels.ToolApproval)
	if !ok {
		// An unset level is a work item without the section: index 0.
		return levels.ToolApproval == ""
	}
	return index <= 1
}

// skillIDPattern is a safe single path segment.
var skillIDPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)

// SkillPackageDigest is the package digest a control plane sends in
// Skill.Digest: "sha256:" + lowercase hex sha256 over one line per file,
// sorted by path (byte order),
//
//	<sha256 of content> <"755" when any executable bit is set, else "644"> <path>\n
//
// The line names each file's content and executability, so a package whose
// content, set of paths or executable bits change has a different digest.
func SkillPackageDigest(files []stagewire.SkillFile) string {
	sorted := append([]stagewire.SkillFile(nil), files...)
	sort.SliceStable(sorted, func(i, j int) bool { return sorted[i].Path < sorted[j].Path })
	var b strings.Builder
	for _, file := range sorted {
		mode := "644"
		if file.Mode&0o111 != 0 {
			mode = "755"
		}
		b.WriteString(file.SHA256 + " " + mode + " " + file.Path + "\n")
	}
	sum := sha256.Sum256([]byte(b.String()))
	return "sha256:" + hex.EncodeToString(sum[:])
}

// ValidateSkillPath reports why p is not a safe file path inside a skill
// directory, or nil. Paths are relative and forward-slash separated, with no
// empty, ".", ".." or ".git" segment, no backslash, colon or control
// character, at most MaxSkillPathBytes long and MaxSkillPathDepth deep.
func ValidateSkillPath(p string) error {
	switch {
	case p == "":
		return errors.New("empty path")
	case len(p) > MaxSkillPathBytes:
		return fmt.Errorf("path longer than %d bytes", MaxSkillPathBytes)
	case !utf8.ValidString(p):
		return errors.New("path is not valid UTF-8")
	case strings.HasPrefix(p, "/"):
		return errors.New("absolute path")
	case strings.ContainsAny(p, "\\:"):
		return errors.New("path contains a backslash or colon")
	}
	for _, r := range p {
		if r < 0x20 || r == 0x7f {
			return errors.New("path contains a control character")
		}
	}
	segments := strings.Split(p, "/")
	if len(segments) > MaxSkillPathDepth {
		return fmt.Errorf("path deeper than %d segments", MaxSkillPathDepth)
	}
	for _, segment := range segments {
		switch {
		case segment == "" || segment == ".":
			return errors.New("path has an empty or '.' segment")
		case segment == "..":
			return errors.New("path traverses out of the skill directory")
		case strings.EqualFold(segment, ".git"):
			return errors.New("path names a .git entry")
		case segment == SkillMarkerFile:
			return errors.New("path names the reserved marker file")
		}
	}
	return nil
}

// fileTypeMask and regularFileType read a git-style mode (0100644).
const (
	fileTypeMask    = 0o170000
	regularFileType = 0o100000
	symlinkFileType = 0o120000
)

// validateSkillPackage checks one package for native delivery and returns
// the files to write (sorted by path), the scripts withheld, and the decoded
// size. Every file's content must hash to its declared sha256 and the set
// must hash to the package digest.
func validateSkillPackage(skill stagewire.Skill, scriptsAllowed bool) ([]SkillPlannedFile, []string, int, error) {
	if !skillIDPattern.MatchString(skill.ID) || strings.Contains(skill.ID, "..") {
		return nil, nil, 0, errors.New("id is not a safe directory name")
	}
	if len(skill.Files) > MaxSkillFiles {
		return nil, nil, 0, fmt.Errorf("more than %d files", MaxSkillFiles)
	}
	if want := SkillPackageDigest(skill.Files); skill.Digest != want {
		return nil, nil, 0, errors.New("package digest does not match its files")
	}
	folded := make(map[string]string, len(skill.Files))
	size := 0
	hasSkillMD := false
	var files []SkillPlannedFile
	var withheld []string
	for _, file := range skill.Files {
		if err := ValidateSkillPath(file.Path); err != nil {
			return nil, nil, 0, fmt.Errorf("file %q: %w", file.Path, err)
		}
		switch file.Mode & fileTypeMask {
		case 0, regularFileType:
		case symlinkFileType:
			return nil, nil, 0, fmt.Errorf("file %q: symbolic links are not delivered", file.Path)
		default:
			return nil, nil, 0, fmt.Errorf("file %q: not a regular file", file.Path)
		}
		key := strings.ToLower(file.Path)
		if _, dup := folded[key]; dup {
			return nil, nil, 0, fmt.Errorf("file %q: duplicate path", file.Path)
		}
		folded[key] = file.Path
		content, err := base64.StdEncoding.DecodeString(file.ContentBase64)
		if err != nil {
			return nil, nil, 0, fmt.Errorf("file %q: content is not base64: %w", file.Path, err)
		}
		size += len(content)
		if size > MaxSkillBytes {
			return nil, nil, 0, fmt.Errorf("content larger than %d bytes", MaxSkillBytes)
		}
		sum := sha256.Sum256(content)
		if !sha256HexPattern.MatchString(file.SHA256) || hex.EncodeToString(sum[:]) != file.SHA256 {
			return nil, nil, 0, fmt.Errorf("file %q: content does not match its sha256", file.Path)
		}
		if file.Path == "SKILL.md" {
			hasSkillMD = true
		}
		executable := file.Mode&0o111 != 0
		if (executable || strings.HasPrefix(file.Path, "scripts/")) && !scriptsAllowed {
			withheld = append(withheld, file.Path)
			continue
		}
		mode := uint32(0o644)
		if executable {
			mode = 0o755
		}
		files = append(files, SkillPlannedFile{Path: file.Path, Mode: mode, SHA256: file.SHA256})
	}
	if !hasSkillMD {
		return nil, nil, 0, errors.New("package has no SKILL.md")
	}
	// A file may not sit where another file needs a directory ("a" and
	// "a/b"): the second write could only replace the first.
	for key := range folded {
		for dir := parentSkillPath(key); dir != ""; dir = parentSkillPath(dir) {
			if original, clash := folded[dir]; clash {
				return nil, nil, 0, fmt.Errorf("file %q: is also a directory", original)
			}
		}
	}
	sort.Slice(files, func(i, j int) bool { return files[i].Path < files[j].Path })
	sort.Strings(withheld)
	return files, withheld, size, nil
}

func parentSkillPath(p string) string {
	i := strings.LastIndex(p, "/")
	if i < 0 {
		return ""
	}
	return p[:i]
}

// SkillFileContent returns the decoded content of path in pkg.
func SkillFileContent(pkg stagewire.Skill, path string) ([]byte, error) {
	for _, file := range pkg.Files {
		if file.Path == path {
			content, err := base64.StdEncoding.DecodeString(file.ContentBase64)
			if err != nil {
				return nil, fmt.Errorf("agent: skill %q file %q: %w", pkg.ID, path, err)
			}
			return content, nil
		}
	}
	return nil, fmt.Errorf("agent: skill %q has no file %q", pkg.ID, path)
}

// SkillDisallowedTools is the session-level union of every skill's
// disallowed tools, in wire order, however each skill is delivered.
func SkillDisallowedTools(skills []stagewire.Skill) []string {
	var out []string
	for _, skill := range skills {
		out = append(out, skill.DisallowedTools...)
	}
	return out
}

// NewSkillFile builds a package file from its content: base64 content,
// permission bits and sha256. For callers composing a package locally (a
// standalone dispatch, a fixture); SealSkillPackage then sets the digest.
func NewSkillFile(path string, content []byte, mode uint32) stagewire.SkillFile {
	sum := sha256.Sum256(content)
	return stagewire.SkillFile{
		Path:          path,
		ContentBase64: base64.StdEncoding.EncodeToString(content),
		Mode:          mode,
		SHA256:        hex.EncodeToString(sum[:]),
	}
}

// SealSkillPackage returns skill with Digest set from its files.
func SealSkillPackage(skill stagewire.Skill) stagewire.Skill {
	skill.Digest = SkillPackageDigest(skill.Files)
	return skill
}

// Summary renders the plan as deterministic text: one header line, then one
// line per skill. Identity, delivery and layout only, never content; for
// logs, receipts read by people, and golden tests.
//
//	harness=pi layout=state_dir scripts=allowed trust=false repo=allowlist->allowlist allow=a,b
//	review native dir=review files=SKILL.md:0644,scripts/run.sh:0755
//	legacy prompt reason=no_files
func (p SkillDeliveryPlan) Summary() string {
	var b strings.Builder
	scripts := "allowed"
	if !p.ScriptsAllowed {
		scripts = "withheld"
	}
	fmt.Fprintf(&b, "harness=%s layout=%s scripts=%s trust=%t repo=%s->%s", p.Harness, p.Layout, scripts, p.TrustWorkspace, p.RepoSkills.Requested, p.RepoSkills.Achieved)
	if len(p.RepoSkills.Allow) > 0 {
		b.WriteString(" allow=" + strings.Join(p.RepoSkills.Allow, ","))
	}
	for _, entry := range p.Entries {
		b.WriteString("\n" + entry.ID + " " + string(entry.Delivery))
		if entry.Reason != "" {
			b.WriteString(" reason=" + string(entry.Reason))
		}
		if entry.Dir != "" {
			b.WriteString(" dir=" + entry.Dir)
		}
		if len(entry.Files) > 0 {
			files := make([]string, len(entry.Files))
			for i, file := range entry.Files {
				files[i] = fmt.Sprintf("%s:%04o", file.Path, file.Mode)
			}
			b.WriteString(" files=" + strings.Join(files, ","))
		}
		if len(entry.WithheldScripts) > 0 {
			b.WriteString(" withheld=" + strings.Join(entry.WithheldScripts, ","))
		}
	}
	return b.String()
}
