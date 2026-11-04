package stagewire

import (
	"encoding/json"
	"fmt"
)

// Skills on the 1.0 wire (ADR-2026-11-02 D8, D9).
//
// A skill is a versioned package: a SKILL.md plus any files it references,
// carried with its content so the runner can deliver it to the harness
// natively (a skill directory the harness discovers or loads by path) rather
// than appending its text to the system prompt. Body stays as the prompt
// fallback for a harness, session mode or package that cannot be delivered
// natively, and is all a pre-1.0 control plane sends.
//
// Decoding is permissive about shape and strict about vocabulary: unknown
// invocation, delivery and repository-policy values are dropped to their
// defaults, as every other closed vocabulary in this package is. Paths,
// sizes and digests are NOT checked here; the runner validates a package
// when it plans delivery, so an invalid package falls back to the prompt
// (or refuses the spawn when its delivery is native_required) instead of
// failing the decode of the whole work item.

// SkillInvocation says who may invoke a natively delivered skill.
type SkillInvocation string

// The invocation modes.
const (
	// SkillInvocationModel lets the model load the skill when its
	// description matches the task.
	SkillInvocationModel SkillInvocation = "model"
	// SkillInvocationExplicit loads the skill only when it is named.
	SkillInvocationExplicit SkillInvocation = "explicit"
	// SkillInvocationBoth allows either.
	SkillInvocationBoth SkillInvocation = "both"
)

// UnmarshalJSON decodes an invocation mode, dropping unknown values.
func (i *SkillInvocation) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return fmt.Errorf("stagewire: skill invocation: %w", err)
	}
	switch v := SkillInvocation(s); v {
	case SkillInvocationModel, SkillInvocationExplicit, SkillInvocationBoth:
		*i = v
	default:
		*i = ""
	}
	return nil
}

// SkillDelivery is how the control plane wants a skill delivered. The empty
// value means SkillDeliveryNativePreferred.
type SkillDelivery string

// The delivery preferences.
const (
	// SkillDeliveryNativePreferred delivers natively where the harness and
	// session mode allow it and falls back to the prompt otherwise.
	SkillDeliveryNativePreferred SkillDelivery = "native_preferred"
	// SkillDeliveryNativeRequired refuses the spawn when the skill cannot
	// be delivered natively.
	SkillDeliveryNativeRequired SkillDelivery = "native_required"
	// SkillDeliveryPromptOnly always appends Body to the system prompt.
	SkillDeliveryPromptOnly SkillDelivery = "prompt_only"
)

// Effective returns d with the empty value resolved to the default.
func (d SkillDelivery) Effective() SkillDelivery {
	if d == "" {
		return SkillDeliveryNativePreferred
	}
	return d
}

// UnmarshalJSON decodes a delivery preference, dropping unknown values.
func (d *SkillDelivery) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return fmt.Errorf("stagewire: skill delivery: %w", err)
	}
	switch v := SkillDelivery(s); v {
	case SkillDeliveryNativePreferred, SkillDeliveryNativeRequired, SkillDeliveryPromptOnly:
		*d = v
	default:
		*d = ""
	}
	return nil
}

// SkillFile is one file of a skill package.
type SkillFile struct {
	// Path is the file's path inside the skill directory: relative,
	// forward-slash separated, for example "SKILL.md" or "scripts/run.sh".
	Path string `json:"path"`
	// ContentBase64 is the file content, standard base64 with padding.
	ContentBase64 string `json:"contentBase64"`
	// Mode is the file's permission bits as a JSON number (420 for 0644,
	// 493 for 0755). Only the executable bits are honoured; zero reads as
	// a plain file.
	Mode uint32 `json:"mode,omitempty"`
	// SHA256 is the lowercase hex sha256 of the decoded content.
	SHA256 string `json:"sha256"`
}

// Skill is one skill on the work item. Skills attach at several levels; the
// control plane merges them (the more specific copy winning on an id clash)
// before dispatch, so the runner sees one list.
type Skill struct {
	// ID is the skill's canonical id. A natively delivered skill is written
	// to a directory named from it, so it must be a safe path segment.
	ID string `json:"id,omitempty"`
	// Name and Description mirror the SKILL.md front matter.
	Name        string `json:"name,omitempty"`
	Description string `json:"description,omitempty"`
	// Version is the package version the dispatch resolved.
	Version string `json:"version,omitempty"`
	// Digest is "sha256:<hex>" over the package's files; see
	// SkillPackageDigest in the agent package for the exact form.
	Digest string `json:"digest,omitempty"`
	// Files is the package content. Empty for a prompt-only skill.
	Files []SkillFile `json:"files,omitempty"`
	// Invocation says who may invoke the skill once delivered.
	Invocation SkillInvocation `json:"invocation,omitempty"`
	// Delivery is the control plane's delivery preference.
	Delivery SkillDelivery `json:"delivery,omitempty"`
	// Body is the prompt fallback: markdown appended to the system prompt
	// when the skill is not delivered natively.
	Body string `json:"body,omitempty"`
	// DisallowedTools is the set of tool-call patterns this skill forbids.
	// The session applies the union over every skill, however each skill
	// is delivered, so a skill can only ever narrow the tool surface.
	DisallowedTools []string `json:"disallowedTools,omitempty"`
}

// Clone returns a deep copy.
func (s Skill) Clone() Skill {
	out := s
	if s.Files != nil {
		out.Files = append([]SkillFile(nil), s.Files...)
	}
	if s.DisallowedTools != nil {
		out.DisallowedTools = append([]string(nil), s.DisallowedTools...)
	}
	return out
}

// CloneSkills deep-copies a skill list; nil stays nil.
func CloneSkills(in []Skill) []Skill {
	if in == nil {
		return nil
	}
	out := make([]Skill, len(in))
	for i, s := range in {
		out[i] = s.Clone()
	}
	return out
}

// RepoSkillsPolicy governs skills committed in the target repository (and
// kit-detected skills, which are governed the same way).
type RepoSkillsPolicy string

// The repository-skill policies.
const (
	// RepoSkillsAll lets the harness discover repository skills as a
	// developer's session would. The default.
	RepoSkillsAll RepoSkillsPolicy = "all"
	// RepoSkillsAllowlist admits only the named repository skills.
	RepoSkillsAllowlist RepoSkillsPolicy = "allowlist"
	// RepoSkillsNone admits no repository skill: card and attached skills
	// only.
	RepoSkillsNone RepoSkillsPolicy = "none"
)

// Effective returns p with the empty value resolved to the default.
func (p RepoSkillsPolicy) Effective() RepoSkillsPolicy {
	if p == "" {
		return RepoSkillsAll
	}
	return p
}

// UnmarshalJSON decodes a policy. An unknown value drops to the default,
// which is the behaviour of a runner that predates the field.
func (p *RepoSkillsPolicy) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return fmt.Errorf("stagewire: repo skills policy: %w", err)
	}
	switch v := RepoSkillsPolicy(s); v {
	case RepoSkillsAll, RepoSkillsAllowlist, RepoSkillsNone:
		*p = v
	default:
		*p = ""
	}
	return nil
}

// RepoSkills is the repository-skill policy for the run. Allow names the
// admitted repository skills when Policy is allowlist and is ignored
// otherwise.
type RepoSkills struct {
	Policy RepoSkillsPolicy `json:"policy,omitempty"`
	Allow  []string         `json:"allow,omitempty"`
}

func (r RepoSkills) clone() RepoSkills {
	if r.Allow != nil {
		r.Allow = append([]string(nil), r.Allow...)
	}
	return r
}
