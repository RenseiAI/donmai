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
// no file) is a silent no-op; any other error logs at warn and falls through
// to the scraped marker. Mutates res + obs in place.
func (r *Runner) applyTurnManifest(worktreePath string, qw QueuedWork, res *Result, obs *streamObservation) {
	m, err := ParseManifest(worktreePath)
	if err != nil {
		if !errors.Is(err, ErrNoManifest) {
			r.logger.Warn("turn-result manifest unusable; falling back to marker scrape",
				"sessionId", qw.SessionID,
				"err", err,
			)
		}
		return
	}

	r.logger.Info("turn-result manifest applied (overrides marker scrape)",
		"sessionId", qw.SessionID,
		"verdict", m.Verdict,
		"hasPR", m.PullRequestURL != "",
	)

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
