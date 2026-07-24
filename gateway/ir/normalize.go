package ir

// Reasoning/thinking normalization (08 §4). A harness asks for thinking in its
// own provider-native vocabulary; the gateway parses that into ONE canonical
// ThinkingSpec on the way in and applies the nearest upstream-native
// representation on the way out, recording what it actually sent
// (AppliedThinking) so cost/eval sees what really ran.
//
// The three upstream vocabularies this maps between:
//   - Anthropic: thinking.budget_tokens (an explicit token budget)
//   - OpenAI:    reasoning_effort (minimal|low|medium|high — a coarse level)
//   - Gemini:    thinkingConfig.thinkingBudget (a token budget, -1 = dynamic)
//
// Thinking CONTENT crossing protocols is handled by the codecs per Emit: a
// surface that natively carries a thinking block gets one; otherwise a client
// that opted into Emit=EmitSummary gets a prefixed text fold; otherwise the
// thinking is dropped — never silently misrepresented as assistant text.

// ThinkingLevel is the canonical reasoning-intensity ladder.
type ThinkingLevel string

// ThinkingLevel constants — the canonical ladder (08 §4).
const (
	ThinkingOff     ThinkingLevel = "off"
	ThinkingMinimal ThinkingLevel = "minimal"
	ThinkingLow     ThinkingLevel = "low"
	ThinkingMedium  ThinkingLevel = "medium"
	ThinkingHigh    ThinkingLevel = "high"
	ThinkingMax     ThinkingLevel = "max"
)

// ThinkingEmit controls how thinking CONTENT is surfaced back to the client.
type ThinkingEmit string

// ThinkingEmit constants — how a client wants thinking content returned.
const (
	EmitHidden  ThinkingEmit = "hidden"  // do not return thinking content
	EmitSummary ThinkingEmit = "summary" // fold a summary into a prefixed text block if the surface has no native block
	EmitFull    ThinkingEmit = "full"    // return full thinking as the surface's native block
)

// ThinkingSpec is the canonical thinking request. Zero value = thinking off,
// hidden (the safe default: no reasoning requested, none surfaced).
type ThinkingSpec struct {
	Level ThinkingLevel `json:"level,omitempty"`
	// BudgetTokens, when set, is an explicit token budget that overrides the
	// level-derived budget for budget-shaped upstreams.
	BudgetTokens *int         `json:"budgetTokens,omitempty"`
	Emit         ThinkingEmit `json:"emit,omitempty"`
}

// AppliedThinking records the concrete thinking configuration the gateway
// applied to a given upstream, for the response metadata.
type AppliedThinking struct {
	Level        ThinkingLevel `json:"level"`
	BudgetTokens int           `json:"budgetTokens,omitempty"`
	Effort       string        `json:"effort,omitempty"`
}

// IsOff reports whether the spec requests no reasoning.
func (t ThinkingSpec) IsOff() bool {
	return t.Level == "" || t.Level == ThinkingOff
}

// levelBudget is the canonical token budget each level maps to for
// budget-shaped upstreams (Anthropic/Gemini). These are conservative defaults;
// an explicit BudgetTokens overrides them.
var levelBudget = map[ThinkingLevel]int{
	ThinkingMinimal: 1024,
	ThinkingLow:     4096,
	ThinkingMedium:  8192,
	ThinkingHigh:    16384,
	ThinkingMax:     32768,
}

// levelEffort maps the canonical ladder onto OpenAI's reasoning_effort. OpenAI
// has no "max"/"off" token, so off yields "" (the caller omits the field) and
// max clamps to "high".
var levelEffort = map[ThinkingLevel]string{
	ThinkingMinimal: "minimal",
	ThinkingLow:     "low",
	ThinkingMedium:  "medium",
	ThinkingHigh:    "high",
	ThinkingMax:     "high",
}

// BudgetForBudgetUpstream returns the token budget to apply for an
// Anthropic/Gemini-style upstream, or 0 when thinking is off. An explicit
// BudgetTokens wins over the level-derived default.
func (t ThinkingSpec) BudgetForBudgetUpstream() int {
	if t.IsOff() {
		return 0
	}
	if t.BudgetTokens != nil {
		return *t.BudgetTokens
	}
	return levelBudget[t.Level]
}

// EffortForOpenAI returns the reasoning_effort string to apply for an OpenAI-
// style upstream, or "" when thinking is off (the caller then omits the field).
func (t ThinkingSpec) EffortForOpenAI() string {
	if t.IsOff() {
		return ""
	}
	return levelEffort[t.Level]
}

// LevelFromEffort maps an inbound OpenAI reasoning_effort value onto the
// canonical ladder. Unknown/empty yields ThinkingOff.
func LevelFromEffort(effort string) ThinkingLevel {
	switch effort {
	case "minimal":
		return ThinkingMinimal
	case "low":
		return ThinkingLow
	case "medium":
		return ThinkingMedium
	case "high":
		return ThinkingHigh
	default:
		return ThinkingOff
	}
}

// LevelFromBudget maps an inbound Anthropic/Gemini token budget onto the
// nearest canonical level. A non-positive budget yields ThinkingOff.
func LevelFromBudget(budget int) ThinkingLevel {
	switch {
	case budget <= 0:
		return ThinkingOff
	case budget <= levelBudget[ThinkingMinimal]:
		return ThinkingMinimal
	case budget <= levelBudget[ThinkingLow]:
		return ThinkingLow
	case budget <= levelBudget[ThinkingMedium]:
		return ThinkingMedium
	case budget <= levelBudget[ThinkingHigh]:
		return ThinkingHigh
	default:
		return ThinkingMax
	}
}
