package runner

// Turn-result manifest — the deterministic turn-outcome wire (W3).
//
// A session's terminal outcome (the QA/acceptance verdict, the PR/commit it
// produced, a short summary) historically reached the platform only by
// SCRAPING the agent's free-form final message for a `WORK_RESULT:<verdict>`
// marker (runner/loop.go scanWorkResult). That scrape is fragile: weaker
// tool-using models forget the marker, bury it mid-sentence, or emit it on a
// non-final turn, and the platform then derives `result=unknown` and stalls the
// SDLC chain.
//
// The manifest replaces the scrape with a structured artifact the agent WRITES
// to a known path — `.agent/turn-result.json` under the worktree — at the end
// of its turn. The runner reads + validates it FIRST (runner/loop.go resolution
// order); the marker scrape and the deterministic backstop remain as fallbacks
// for agents that did not (or could not) write the file. Writing a file is a
// far more reliable instruction for a weak tool user than "end your final
// message with exactly one marker on its own line".
//
// The schema is intentionally minimal + versioned. It carries exactly what the
// platform's `applyTurnManifest` consumes — the verdict, a summary, and the
// artifact references (PR url, commit sha) — and nothing the runner already
// captures more authoritatively (cost, provider session id, failure mode are
// runner-owned, not agent-owned). Additive fields bump nothing; a breaking
// change bumps `schemaVersion`.
//
// Canonical: donmai-architecture/ADR-2026-06-15-turn-result-manifest.md
// (boundary: shared) + 013-orchestrator-and-governor.md § "completion
// contracts".

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/santhosh-tekuri/jsonschema/v6"

	"github.com/RenseiAI/donmai/agent"
	"github.com/RenseiAI/donmai/runtime/state"
)

// ManifestFileName is the worktree-relative path (under .agent/) the agent
// writes its turn-result manifest to. Stable wire contract — the prompt
// templates instruct the agent to write exactly this path, and the runner
// reads exactly this path.
const ManifestFileName = "turn-result.json"

// ManifestSchemaVersion is the current manifest schema version. The runner
// rejects a manifest whose `schemaVersion` it does not recognise so a future
// breaking schema change cannot be silently mis-parsed by an older runner.
const ManifestSchemaVersion = 1

// ErrNoManifest is returned by [ParseManifest] when no manifest file exists at
// the expected path. It is a benign, expected condition — the runner falls
// through to the marker scrape + backstop — so callers MUST distinguish it from
// a genuine parse/validation failure (which signals a malformed file the agent
// DID write).
var ErrNoManifest = errors.New("runner: no turn-result manifest")

// ErrNoInlineManifest is returned by [ParseInlineManifest] when the agent's
// final message carries no recoverable inline manifest — either there is no
// `Intended manifest:` label, or the JSON after it does not parse / validate.
// Like [ErrNoManifest] it is a benign, expected condition: the caller degrades
// cleanly to the WORK_RESULT marker scrape rather than failing the turn. It
// mirrors ErrNoManifest so the inline tier behaves like the file tier.
var ErrNoInlineManifest = errors.New("runner: no inline turn-result manifest")

// inlineManifestLabelRE matches the `Intended manifest:` label some agents
// print next to the WORK_RESULT marker as a backstop when their tool policy
// removed the file-writing tool and they COULD NOT write
// `.agent/turn-result.json`. Case-insensitive with flexible internal/leading
// whitespace; the balanced-brace scan that follows starts at the first `{`
// after the label.
var inlineManifestLabelRE = regexp.MustCompile(`(?i)intended\s+manifest\s*:`)

// TurnManifest is the deterministic turn-outcome the agent writes to
// `.agent/turn-result.json`. It is the agent-owned half of the session
// completion contract (013-orchestrator-and-governor.md § "completion
// contracts"); the runner owns the rest of the terminal envelope (cost,
// failure classification, provider session id).
//
// It is a type ALIAS of [agent.TurnManifest] — the wire-canonical struct that
// rides [agent.Result.Manifest] to the platform. Aliasing keeps a single source
// of truth for the contract: the parse/validate surface here and the wire
// carrier in the agent package are the same type, so a field can never drift
// between "what the runner reads" and "what the poster sends".
//
// Verdict vocabulary ("passed" | "failed" | "blocked") matches the legacy
// WORK_RESULT marker so the platform consumes either channel uniformly.
type TurnManifest = agent.TurnManifest

// validVerdicts is the closed set the manifest schema accepts. Mirrors the
// WORK_RESULT marker vocabulary (loop.go) so both channels are interchangeable.
var validVerdicts = map[string]struct{}{
	"passed":  {},
	"failed":  {},
	"blocked": {},
}

// manifestSchema is the JSON Schema the manifest is validated against, using
// the santhosh-tekuri/jsonschema/v6 pattern established in agent/oneshot.go
// (validateAgainstSchema). Keeping the contract as a schema — not just struct
// tags — lets a malformed agent-written file be rejected with a precise error
// rather than silently coerced (e.g. a string "1" schemaVersion, an unknown
// verdict, a missing required field).
const manifestSchema = `{
  "$schema": "https://json-schema.org/draft/2020-12/schema",
  "type": "object",
  "required": ["schemaVersion", "verdict"],
  "properties": {
    "schemaVersion": { "type": "integer", "minimum": 1 },
    "verdict": { "type": "string", "enum": ["passed", "failed", "blocked"] },
    "summary": { "type": "string" },
    "blockedReason": { "type": "string" },
    "pullRequestUrl": { "type": "string" },
    "commitSha": { "type": "string" }
  }
}`

// ParseManifest reads + validates the turn-result manifest at
// `<worktreePath>/.agent/turn-result.json`.
//
// Returns:
//   - ([*TurnManifest], nil) when the file exists, parses as JSON, validates
//     against [manifestSchema], and carries a recognised schemaVersion.
//   - (nil, [ErrNoManifest]) when no file exists — the benign "agent didn't
//     write one" case; the caller falls through to the marker scrape.
//   - (nil, error) for any genuine failure: read error, malformed JSON, schema
//     violation, or unrecognised schemaVersion. The agent DID write a file but
//     it is unusable — surface it so the resolution order falls through to the
//     scrape rather than swallowing a real problem silently.
//
// ParseManifest is pure aside from the single file read — no network, no
// mutation — so the resolution-order fork in loop.go is unit-testable in
// isolation.
func ParseManifest(worktreePath string) (*TurnManifest, error) {
	if strings.TrimSpace(worktreePath) == "" {
		return nil, fmt.Errorf("runner: ParseManifest: empty worktree path")
	}
	path := filepath.Join(worktreePath, state.AgentDirName, ManifestFileName)

	//nolint:gosec // G304: path is owned by the runner via the worktree manager.
	raw, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, ErrNoManifest
		}
		return nil, fmt.Errorf("runner: read turn-result manifest: %w", err)
	}

	if !validateManifestSchema(raw) {
		return nil, fmt.Errorf("runner: turn-result manifest at %s failed schema validation", path)
	}

	var m TurnManifest
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, fmt.Errorf("runner: parse turn-result manifest: %w", err)
	}

	// Version gate: schema enforces minimum:1, but an older runner must reject
	// a FUTURE breaking version it cannot interpret rather than mis-read it.
	if m.SchemaVersion != ManifestSchemaVersion {
		return nil, fmt.Errorf(
			"runner: unsupported turn-result manifest schemaVersion %d (this runner supports %d)",
			m.SchemaVersion, ManifestSchemaVersion,
		)
	}

	// Defensive: the schema enum already constrains verdict, but guard the
	// struct-level invariant too so a future schema relaxation can't leak an
	// unrecognised verdict into the resolution order.
	if _, ok := validVerdicts[m.Verdict]; !ok {
		return nil, fmt.Errorf("runner: turn-result manifest verdict %q is not one of passed/failed/blocked", m.Verdict)
	}

	return &m, nil
}

// ParseInlineManifest recovers a turn-result manifest from an `Intended
// manifest: { … }` JSON block the agent printed INLINE in its final message.
//
// Some stages run under a tool policy that removes the file-writing tool, so the
// agent CANNOT write `.agent/turn-result.json`. Their prompt has them print the
// manifest inline next to the WORK_RESULT marker as a backstop. Without this
// tier the runner would reduce that backstop to only the scraped marker verdict
// and lose the structured summary; this recovers the full manifest so the same
// structured outcome the file tier produces reaches the wire.
//
// It scans finalMessage for the (case-insensitive, whitespace-flexible)
// `Intended manifest:` label, extracts the FIRST balanced JSON object after it
// with a string-literal-aware brace scan (a `{`/`}` inside a JSON string value
// does NOT truncate the object), and validates the result through the SAME path
// the file tier uses ([validateManifestSchema] + the [ManifestSchemaVersion]
// guard + the verdict enum).
//
// Returns:
//   - ([*TurnManifest], nil) when a labelled, balanced, valid manifest is found.
//   - (nil, [ErrNoInlineManifest]) for every degrade case — no label, no
//     balanced object after the label, JSON that does not parse, a schema
//     violation, or an unrecognised schemaVersion. The caller MUST fall through
//     to the marker scrape on this sentinel; an inline backstop is best-effort
//     and must never fail the turn on bad text.
//
// Pure: no I/O, no mutation. A trailing WORK_RESULT marker (or any other prose)
// after the JSON object is tolerated — the balanced scan stops at the matching
// close brace and ignores the remainder.
func ParseInlineManifest(finalMessage string) (*TurnManifest, error) {
	loc := inlineManifestLabelRE.FindStringIndex(finalMessage)
	if loc == nil {
		return nil, ErrNoInlineManifest
	}

	raw, ok := extractBalancedJSONObject(finalMessage[loc[1]:])
	if !ok {
		return nil, ErrNoInlineManifest
	}

	if !validateManifestSchema(json.RawMessage(raw)) {
		return nil, ErrNoInlineManifest
	}

	var m TurnManifest
	if err := json.Unmarshal([]byte(raw), &m); err != nil {
		return nil, ErrNoInlineManifest
	}

	// Version + verdict gates mirror ParseManifest so the inline tier accepts
	// exactly what the file tier does — no looser contract via a side door.
	if m.SchemaVersion != ManifestSchemaVersion {
		return nil, ErrNoInlineManifest
	}
	if _, ok := validVerdicts[m.Verdict]; !ok {
		return nil, ErrNoInlineManifest
	}

	return &m, nil
}

// extractBalancedJSONObject returns the FIRST balanced `{ … }` object in s,
// starting the scan at the first `{`. The scan is string-literal-aware: braces
// inside a JSON string value (and escaped quotes within it) do NOT affect the
// nesting depth, so a `}` in a summary string cannot truncate the object early.
// Returns (object, true) on the matched object, (\"\", false) when there is no
// opening brace or the object never closes (unbalanced).
func extractBalancedJSONObject(s string) (string, bool) {
	start := strings.IndexByte(s, '{')
	if start < 0 {
		return "", false
	}

	depth := 0
	inString := false
	escaped := false
	for i := start; i < len(s); i++ {
		c := s[i]
		if inString {
			switch {
			case escaped:
				// Previous byte was a backslash; this byte is consumed as the
				// escaped char (handles \" and \\) and ends the escape.
				escaped = false
			case c == '\\':
				escaped = true
			case c == '"':
				inString = false
			}
			continue
		}
		switch c {
		case '"':
			inString = true
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				return s[start : i+1], true
			}
		}
	}
	return "", false
}

// validateManifestSchema reports whether raw validates against the manifest
// JSON Schema. A malformed schema or instance returns false (fail-closed —
// an unparseable instance can't certify anything). Mirrors
// agent/oneshot.go::validateAgainstSchema.
func validateManifestSchema(raw json.RawMessage) bool {
	schemaDoc, err := jsonschema.UnmarshalJSON(strings.NewReader(manifestSchema))
	if err != nil {
		return false
	}
	c := jsonschema.NewCompiler()
	if err := c.AddResource("runner://turn-manifest-schema", schemaDoc); err != nil {
		return false
	}
	sch, err := c.Compile("runner://turn-manifest-schema")
	if err != nil {
		return false
	}
	instDoc, err := jsonschema.UnmarshalJSON(bytes.NewReader(raw))
	if err != nil {
		return false
	}
	return sch.Validate(instDoc) == nil
}

// applyTurnManifest reads the agent-written turn-result manifest and folds it
// onto the terminal envelope, taking PRECEDENCE over the scraped WORK_RESULT
// marker (the verdict, summary, and PR url the manifest carries override the
// stream-observed values). It is the FIRST tier of the verdict resolution
// order (manifest → marker scrape → backstop); see runner/loop.go step 10·M.
//
// The merge:
//   - Verdict → res.WorkResult and obs.workResult. "blocked" maps to
//     obs.blocked (+ obs.blockedReason) so the existing classifyBlocked fork
//     in loop.go treats a manifest-declared decline identically to a scraped
//     "WORK_RESULT:blocked" marker — no second classification path to keep in
//     sync.
//   - Summary → res.Summary (the agent's structured summary beats the
//     terminal-event message, which codex leaves empty).
//   - PullRequestURL → res.PullRequestURL + obs.pullRequestURL (so the blocked
//     fork's "PR-producing session is never blocked" guard sees a manifest PR).
//   - CommitSHA → res.CommitSHA ONLY when the runner has not already captured a
//     head sha; the runner's post-backstop git capture is authoritative.
//
// Best-effort, never fatal: ErrNoManifest (the common case — the agent wrote
// no file) falls through to the INLINE tier; any other file error logs at warn
// and falls through to the scraped marker. Mutates res + obs in place.
//
// Resolution order for the structured manifest: the written file FIRST, then —
// when no file exists — an `Intended manifest: { … }` block recovered from the
// agent's final message (the same text the marker scan reads). This inline tier
// exists for stages whose tool policy removed the file-writing tool: they cannot
// write the file but their prompt has them print the manifest inline as a
// backstop. The WORK_RESULT marker scrape remains the FINAL fallback below — any
// inline-parse degrade (ErrNoInlineManifest) is a silent no-op so a bad inline
// block never fails the turn.
func (r *Runner) applyTurnManifest(worktreePath string, qw QueuedWork, res *Result, obs *streamObservation) {
	m, err := ParseManifest(worktreePath)
	if err != nil {
		if !errors.Is(err, ErrNoManifest) {
			r.logger.Warn("turn-result manifest unusable; falling back to marker scrape",
				"sessionId", qw.SessionID,
				"err", err,
			)
			return
		}
		// No file. Try to recover an inline manifest the agent printed in its
		// final message (a backstop for tool-restricted stages that cannot write
		// the file). On any degrade, fall through to the marker scrape.
		m, err = ParseInlineManifest(obs.lastAssistantText)
		if err != nil {
			return
		}
		r.logger.Info("inline turn-result manifest recovered from final message (no file written)",
			"sessionId", qw.SessionID,
			"verdict", m.Verdict,
			"hasPR", m.PullRequestURL != "",
		)
	} else {
		r.logger.Info("turn-result manifest applied (overrides marker scrape)",
			"sessionId", qw.SessionID,
			"verdict", m.Verdict,
			"hasPR", m.PullRequestURL != "",
		)
	}

	// Carry the validated manifest VERBATIM on the envelope so the poster can
	// post the structured object to the platform's applyTurnManifest route
	// (idempotent by content hash). runner.TurnManifest is a type alias of
	// agent.TurnManifest, so this assigns directly.
	res.Manifest = m

	switch m.Verdict {
	case "blocked":
		// A manifest-declared decline drives the same signal the marker scan
		// would. The verdict itself ("blocked") is NOT a QA work-result — leave
		// res.WorkResult unset for it (mirrors scanWorkResult, which never
		// returns "blocked" into obs.workResult; loop.go's workResultBlockedRE
		// routes the decline through obs.blocked instead).
		obs.blocked = true
		if m.BlockedReason != "" && obs.blockedReason == "" {
			obs.blockedReason = m.BlockedReason
		}
	default:
		// "passed" | "failed" — the QA/acceptance verdict the platform
		// transitions on. Override the scraped marker on both the envelope and
		// the observation (so any later re-apply of obs stays consistent).
		res.WorkResult = m.Verdict
		obs.workResult = m.Verdict
	}

	if m.Summary != "" {
		res.Summary = m.Summary
	}
	if m.PullRequestURL != "" {
		res.PullRequestURL = m.PullRequestURL
		obs.pullRequestURL = m.PullRequestURL
	}
	// Advisory: the runner's own post-backstop git rev-parse is the
	// authoritative head sha (it runs AFTER tail recovery + backstop, which may
	// add commits). Only adopt the manifest sha when the runner has nothing.
	if m.CommitSHA != "" && res.CommitSHA == "" {
		res.CommitSHA = m.CommitSHA
	}
}
