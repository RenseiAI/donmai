package agent

// This file declares the card-skill delivery seam for harnesses that load
// skills from a trusted workspace (currently muse;
// provider/harness/muse). It is the skill analogue of the additional-
// extension delivery seam in extension_delivery.go, minus the digest
// verification: card-skill bytes arrive inline on the already-trusted
// spawn spec and are written verbatim into the session workspace, where
// the host's own discovery (not donmai) loads them. A harness without a
// workspace-skill surface ignores this field, exactly like every other
// capability-gated Spec field.

// CardSkill is one agent-card skill delivered on the spawn spec for
// workspace materialization. It mirrors prompt.SkillSpec's wire shape
// (ID + Body) without importing the prompt package: providers consume
// agent types, never the runner's prompt layer.
type CardSkill struct {
	// ID is the skill's canonical id (e.g. "spring-debugging"). It names
	// the materialized directory (under a harness-owned prefix) and the
	// skill's frontmatter name. Required; unique within one Spec's
	// CardSkills after the harness's name normalization.
	ID string `json:"id,omitempty"`

	// Body is the skill's markdown body, written verbatim below the
	// synthesized frontmatter. It may be empty only when the card
	// carries no guidance for this skill; the harness still writes a
	// loadable file so the skill's presence is explicit.
	Body string `json:"body,omitempty"`
}
