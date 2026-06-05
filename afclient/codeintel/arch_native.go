// Package codeintel — native arch-intel diff/gate layer.
//
// This file ports the pure-regex/JSON layers of the TS
// @donmai/architectural-intelligence package to Go:
//
//   - ReadDiffObservations — file-path zone inference, convention detection,
//     decision inference, acceptance-criteria inference (all pure regex).
//   - EvaluateGate — DriftGatePolicy evaluation.
//   - BuildNativeDriftReport — assemble a DriftReport from the above outputs.
//
// These functions cover the "cheap" subset of arch-intel that requires NO
// external binary, NO LLM, and NO database. They run as the primary path of
// `donmai arch assess` when DONMAI_ARCH_BIN is not set.
//
// The full pipeline (SQLite graph + LLM deviation-detection prompt) is deferred
// to a followup; see runs/2026-06-05-tui-readiness/research/18-arch-intel-assessment.md.
package codeintel

import (
	"fmt"
	"regexp"
	"strings"
	"time"
)

// ── Input / Output types ──────────────────────────────────────────────────────

// PrDiff is the provider-orthogonal input to ReadDiffObservations.
// Callers populate this from their VCS provider (or test fixtures).
type PrDiff struct {
	// Repository identifier, e.g. "github.com/org/repo"
	Repository string

	// PrNumber is the pull request number in the source VCS.
	PrNumber int

	// Title is the PR title.
	Title string

	// Body is the PR description / body text.
	Body string

	// Files is the list of changed files.
	Files []PrFileDiff

	// AcceptanceCriteria is an optional pre-parsed list of AC items.
	AcceptanceCriteria []string
}

// PrFileDiff describes one file in a PR.
type PrFileDiff struct {
	// Path is the relative file path.
	Path string

	// Patch is the unified diff text (the +/− lines).
	Patch string

	// Added is true when the file was newly created.
	Added bool
}

// DiffObservation is a single architectural signal extracted from a PR diff.
// The JSON field names are byte-compatible with the TS ArchObservation shape so
// downstream tools (platform, smokes) see identical JSON regardless of which
// implementation produced it.
type DiffObservation struct {
	// Kind is one of "pattern", "convention", "decision".
	Kind string `json:"kind"`

	// Payload carries the structured data for the observation.
	Payload map[string]any `json:"payload"`

	// Confidence is a 0–1 signal strength score.
	Confidence float64 `json:"confidence"`

	// Scope is the architectural scope (always "project" for native path).
	Scope string `json:"scope"`
}

// NativeDriftReport is the JSON-serialisable result of BuildNativeDriftReport.
// The top-level fields are byte-compatible with the TS DriftReport shape.
type NativeDriftReport struct {
	// Change is the assessed VCS change.
	Change ChangeRefJSON `json:"change"`

	// Observations are the raw diff-level signals (no LLM required).
	// This field is a donmai-native extension; the TS DriftReport omits it.
	Observations []DiffObservation `json:"observations"`

	// Deviations is always empty in the native path (LLM not run).
	Deviations []any `json:"deviations"`

	// Reinforced is always empty in the native path.
	Reinforced []any `json:"reinforced"`

	// HasCriticalDrift is always false in the native path.
	HasCriticalDrift bool `json:"hasCriticalDrift"`

	// Gated reflects the gate policy evaluation against the observation list.
	// In the native path, gate evaluation uses observation confidence thresholds
	// rather than LLM-produced severity levels.
	Gated bool `json:"gated"`

	// Summary is a human-readable description of the result.
	Summary string `json:"summary"`

	// AssessedAt is the RFC3339 assessment timestamp.
	AssessedAt string `json:"assessedAt"`

	// Mode is "native-diff-only" for this implementation path, distinguishing
	// it from a full LLM-backed assessment.
	Mode string `json:"mode"`
}

// ChangeRefJSON mirrors the TS ChangeRef shape for JSON output compatibility.
type ChangeRefJSON struct {
	Repository  string `json:"repository"`
	Kind        string `json:"kind"`
	PrNumber    int    `json:"prNumber,omitempty"`
	Description string `json:"description,omitempty"`
}

// ── ReadDiffObservations ──────────────────────────────────────────────────────

// ReadDiffObservations extracts ArchObservation candidates from a PR diff.
// All extraction is pure regex — no external binary, LLM, or DB is required.
//
// Three signal classes are emitted (matching the TS diff-reader.ts behaviour):
//
//  1. File-path zone patterns → "pattern" observations
//  2. Convention signals from diff patches → "convention" observations
//  3. Decision inference from PR title/body → "decision" observations
//  4. Acceptance-criteria items → "pattern" / "convention" observations
func ReadDiffObservations(diff PrDiff, scope string) []DiffObservation {
	if scope == "" {
		scope = "project"
	}

	changeRef := ChangeRefJSON{
		Repository:  diff.Repository,
		Kind:        "pr",
		PrNumber:    diff.PrNumber,
		Description: diff.Title,
	}

	var out []DiffObservation
	out = append(out, inferZonePatterns(diff.Files, changeRef, scope)...)
	out = append(out, inferConventions(diff.Files, changeRef, scope)...)
	out = append(out, inferDecisions(diff, changeRef, scope)...)
	if len(diff.AcceptanceCriteria) > 0 {
		out = append(out, inferFromAcceptanceCriteria(diff.AcceptanceCriteria, changeRef, scope)...)
	}
	return out
}

// ── Zone pattern inference ────────────────────────────────────────────────────

type zoneEntry struct {
	re   *regexp.Regexp
	zone string
	tag  string
}

// zonePatterns mirrors the ZONE_PATTERNS constant from diff-reader.ts.
var zonePatterns = []zoneEntry{
	{regexp.MustCompile(`(?i)/(auth|authentication|authorization|middleware)/`), "Auth layer", "auth"},
	{regexp.MustCompile(`(?i)/(db|database|orm|migrations?|schema)/`), "Database layer", "database"},
	{regexp.MustCompile(`(?i)/(api|routes?|endpoints?|controllers?|handlers?)/`), "API layer", "api"},
	{regexp.MustCompile(`(?i)/(tests?|spec|__tests?__|e2e|integration)/`), "Test layer", "testing"},
	{regexp.MustCompile(`(?i)/(domain|core|entities|models)/`), "Domain layer", "domain"},
	{regexp.MustCompile(`(?i)/(adapters?|providers?|infra|infrastructure)/`), "Infrastructure layer", "infra"},
	{regexp.MustCompile(`(?i)/(components?|ui|views?|pages?)/`), "UI layer", "ui"},
	{regexp.MustCompile(`(?i)/(hooks?|composables?|stores?)/`), "State layer", "state"},
	{regexp.MustCompile(`(?i)/(config|configs?|settings?)/`), "Config layer", "config"},
	{regexp.MustCompile(`(?i)/(utils?|helpers?|lib|shared)/`), "Shared utilities", "utils"},
}

type zoneData struct {
	count int
	paths []string
	tag   string
}

func inferZonePatterns(files []PrFileDiff, changeRef ChangeRefJSON, scope string) []DiffObservation {
	zones := make(map[string]*zoneData)

	for _, f := range files {
		pathWithSlash := "/" + f.Path
		for _, zp := range zonePatterns {
			if zp.re.MatchString(pathWithSlash) {
				d, ok := zones[zp.zone]
				if !ok {
					d = &zoneData{tag: zp.tag}
					zones[zp.zone] = d
				}
				d.count++
				d.paths = append(d.paths, f.Path)
			}
		}
	}

	var out []DiffObservation
	for zone, data := range zones {
		if data.count < 1 {
			continue
		}
		// Confidence scales with evidence: 1 file = 0.35, 3+ files → 0.45 (capped at 0.55)
		confidence := 0.35 + float64(data.count)*0.03
		if confidence > 0.55 {
			confidence = 0.55
		}

		shown := data.paths
		if len(shown) > 3 {
			shown = shown[:3]
		}
		extra := ""
		if len(data.paths) > 3 {
			extra = fmt.Sprintf(" (+%d more)", len(data.paths)-3)
		}

		locs := make([]map[string]string, 0, len(data.paths))
		if len(data.paths) > 5 {
			for _, p := range data.paths[:5] {
				locs = append(locs, map[string]string{"path": p})
			}
		} else {
			for _, p := range data.paths {
				locs = append(locs, map[string]string{"path": p})
			}
		}

		out = append(out, DiffObservation{
			Kind: "pattern",
			Payload: map[string]any{
				"title": fmt.Sprintf("%s pattern", zone),
				"description": fmt.Sprintf(
					"Files in the %s zone were modified in PR #%d: %s%s.",
					strings.ToLower(zone), changeRef.PrNumber, strings.Join(shown, ", "), extra,
				),
				"locations": locs,
				"tags":      []string{data.tag, "inferred", "pr-analysis"},
			},
			Confidence: confidence,
			Scope:      scope,
		})
	}
	return out
}

// ── Convention signals ────────────────────────────────────────────────────────

type conventionSignal struct {
	re          *regexp.Regexp
	title       string
	description string
	tag         string
	confidence  float64
}

// conventionSignals mirrors the CONVENTION_SIGNALS constant from diff-reader.ts.
var conventionSignals = []conventionSignal{
	{
		regexp.MustCompile(`Result<[A-Za-z,\s]+>`),
		"Result<T, E> error handling",
		"Code uses Result<T, E> pattern for error propagation instead of throwing.",
		"error-handling", 0.55,
	},
	{
		regexp.MustCompile(`\basync\b.*\bawait\b`),
		"Async/await concurrency",
		"Code consistently uses async/await for asynchronous operations.",
		"async", 0.40,
	},
	{
		regexp.MustCompile(`interface\s+[A-Z][A-Za-z]+\s*\{`),
		"TypeScript interface-first typing",
		"New types are defined as interfaces rather than type aliases or classes.",
		"typescript", 0.45,
	},
	{
		regexp.MustCompile(`export\s+(const|function|class|interface|type)\s+[A-Z]`),
		"Named exports convention",
		"Modules use named exports for public API surface.",
		"modules", 0.40,
	},
	{
		regexp.MustCompile(`describe\s*\(|it\s*\(|test\s*\(`),
		"Vitest/Jest test structure",
		"Tests use describe/it/test block structure.",
		"testing", 0.50,
	},
	{
		regexp.MustCompile(`\.toThrow\(|expect\.assertions\(`),
		"Explicit error assertion testing",
		"Tests explicitly assert on thrown errors.",
		"testing", 0.45,
	},
}

func inferConventions(files []PrFileDiff, _ ChangeRefJSON, scope string) []DiffObservation {
	// Aggregate added lines from all patches
	var addedLines []string
	for _, f := range files {
		for _, line := range strings.Split(f.Patch, "\n") {
			if strings.HasPrefix(line, "+") {
				addedLines = append(addedLines, line)
			}
		}
	}
	addedText := strings.Join(addedLines, "\n")

	var out []DiffObservation
	seen := make(map[string]bool)

	for _, sig := range conventionSignals {
		if seen[sig.title] {
			continue
		}
		if !sig.re.MatchString(addedText) {
			continue
		}
		seen[sig.title] = true

		var examples []map[string]string
		for _, f := range files {
			if sig.re.MatchString(f.Patch) {
				examples = append(examples, map[string]string{"path": f.Path})
				if len(examples) >= 3 {
					break
				}
			}
		}

		out = append(out, DiffObservation{
			Kind: "convention",
			Payload: map[string]any{
				"title":       sig.title,
				"description": sig.description,
				"examples":    examples,
			},
			Confidence: sig.confidence,
			Scope:      scope,
		})
	}
	return out
}

// ── Decision inference ────────────────────────────────────────────────────────

type decisionSignal struct {
	re      *regexp.Regexp
	extract func(groups []string) (title, chosen, rationale string)
}

// decisionSignals mirrors the DECISION_SIGNALS constant from diff-reader.ts.
var decisionSignals = []decisionSignal{
	{
		// "chose X over Y" / "picked X over Y" / etc.
		regexp.MustCompile(`(?i)\b(chose|picked|selected|choosing|picking|using|switched?(?:\s+to)?)\s+([A-Za-z0-9_\-.@/]+)\s+(?:over|instead\s+of|rather\s+than)\s+([A-Za-z0-9_\-.@/]+)`),
		func(g []string) (string, string, string) {
			if len(g) < 4 {
				return "", "", ""
			}
			return fmt.Sprintf("%s chosen over %s", g[2], g[3]),
				g[2],
				fmt.Sprintf("%s %s over %s (inferred from PR title/body)", g[1], g[2], g[3])
		},
	},
	{
		// "migrated from X to Y"
		regexp.MustCompile(`(?i)\b(migrat(?:e|ed|ing|ion))\s+(?:from\s+)?([A-Za-z0-9_\-.@/]+)\s+to\s+([A-Za-z0-9_\-.@/]+)`),
		func(g []string) (string, string, string) {
			if len(g) < 4 {
				return "", "", ""
			}
			return fmt.Sprintf("Migration from %s to %s", g[2], g[3]),
				g[3],
				fmt.Sprintf("Migration from %s to %s (inferred from PR title/body)", g[2], g[3])
		},
	},
	{
		// "replaced X with Y"
		regexp.MustCompile(`(?i)\b(replac(?:e|ed|ing))\s+([A-Za-z0-9_\-.@/]+)\s+with\s+([A-Za-z0-9_\-.@/]+)`),
		func(g []string) (string, string, string) {
			if len(g) < 4 {
				return "", "", ""
			}
			return fmt.Sprintf("%s replaces %s", g[3], g[2]),
				g[3],
				fmt.Sprintf("Replaced %s with %s (inferred from PR title/body)", g[2], g[3])
		},
	},
}

func inferDecisions(diff PrDiff, _ ChangeRefJSON, scope string) []DiffObservation {
	text := diff.Title + "\n" + diff.Body
	var out []DiffObservation

	for _, sig := range decisionSignals {
		match := sig.re.FindStringSubmatch(text)
		if match == nil {
			continue
		}
		title, chosen, rationale := sig.extract(match)
		if title == "" {
			continue
		}

		out = append(out, DiffObservation{
			Kind: "decision",
			Payload: map[string]any{
				"title":        title,
				"chosen":       chosen,
				"alternatives": []any{},
				"rationale":    rationale,
				"status":       "active",
			},
			Confidence: 0.60,
			Scope:      scope,
		})
	}
	return out
}

// ── Acceptance criteria inference ─────────────────────────────────────────────

var normativeRe = regexp.MustCompile(`(?i)\b(must|should|always|never|required|shall)\b`)

func inferFromAcceptanceCriteria(ac []string, changeRef ChangeRefJSON, scope string) []DiffObservation {
	var out []DiffObservation
	for _, item := range ac {
		trimmed := strings.TrimSpace(item)
		if len(trimmed) < 10 {
			continue
		}

		normative := normativeRe.MatchString(trimmed)
		kind := "pattern"
		confidence := 0.50
		if normative {
			kind = "convention"
			confidence = 0.65
		}

		payload := map[string]any{
			"title":       acTitle(trimmed),
			"description": fmt.Sprintf("Acceptance criterion from PR #%d: %s", changeRef.PrNumber, trimmed),
		}
		if kind == "convention" {
			payload["examples"] = []map[string]string{{"path": fmt.Sprintf("PR #%d", changeRef.PrNumber)}}
		} else {
			payload["locations"] = []any{}
			payload["tags"] = []string{"ac", "pr-analysis"}
		}

		out = append(out, DiffObservation{
			Kind:       kind,
			Payload:    payload,
			Confidence: confidence,
			Scope:      scope,
		})
	}
	return out
}

var (
	acStripRe = regexp.MustCompile(`^[-*\[\]#\s]+`)
	acSpaceRe = regexp.MustCompile(`\s+`)
)

func acTitle(ac string) string {
	cleaned := acStripRe.ReplaceAllString(ac, "")
	cleaned = strings.TrimSpace(acSpaceRe.ReplaceAllString(cleaned, " "))
	if len(cleaned) <= 80 {
		return cleaned
	}
	return cleaned[:77] + "..."
}

// ── Gate evaluation ───────────────────────────────────────────────────────────

// EvaluateGate evaluates whether a set of observations triggers the drift gate.
//
// In the native path, observations are diff-level signals rather than
// LLM-derived Deviation nodes with explicit severity. The gate is evaluated
// on observation confidence:
//
//   - "none"              → never gate
//   - "no-severity-high"  → gate when any observation has confidence >= 0.65
//     (maps to inferred-high threshold — no LLM severity available)
//   - "zero-deviations"   → gate when there are any observations
//   - "max:N"             → gate when observation count > N
//
// The TS `assessChange()` path evaluates against Deviation.severity instead;
// this is only the no-LLM fallback.
func EvaluateGate(observations []DiffObservation, policy string) bool {
	switch policy {
	case "none":
		return false
	case "zero-deviations":
		return len(observations) > 0
	case "no-severity-high", "":
		// Proxy "high severity" as confidence >= 0.65
		for _, o := range observations {
			if o.Confidence >= 0.65 {
				return true
			}
		}
		return false
	default:
		if strings.HasPrefix(policy, "max:") {
			var n int
			if _, err := fmt.Sscanf(policy[4:], "%d", &n); err == nil && n >= 0 {
				return len(observations) > n
			}
		}
		return false
	}
}

// ── BuildNativeDriftReport ────────────────────────────────────────────────────

// BuildNativeDriftReport assembles a NativeDriftReport from diff observations
// and a gate policy. This is the primary output of `donmai arch assess` when
// DONMAI_ARCH_BIN is not set and ANTHROPIC_API_KEY is absent.
func BuildNativeDriftReport(
	diff PrDiff,
	observations []DiffObservation,
	gatePolicy string,
) NativeDriftReport {
	gated := EvaluateGate(observations, gatePolicy)

	summary := buildNativeSummary(observations, gated, gatePolicy)

	return NativeDriftReport{
		Change: ChangeRefJSON{
			Repository:  diff.Repository,
			Kind:        "pr",
			PrNumber:    diff.PrNumber,
			Description: diff.Title,
		},
		Observations:     observations,
		Deviations:       []any{},
		Reinforced:       []any{},
		HasCriticalDrift: false,
		Gated:            gated,
		Summary:          summary,
		AssessedAt:       time.Now().UTC().Format(time.RFC3339),
		Mode:             "native-diff-only",
	}
}

func buildNativeSummary(observations []DiffObservation, gated bool, policy string) string {
	if len(observations) == 0 {
		return "No architectural signals detected in the diff. Native diff-only mode (no LLM)."
	}

	patternCount := 0
	conventionCount := 0
	decisionCount := 0
	for _, o := range observations {
		switch o.Kind {
		case "pattern":
			patternCount++
		case "convention":
			conventionCount++
		case "decision":
			decisionCount++
		}
	}

	parts := []string{
		fmt.Sprintf("%d diff signal(s) detected (%d patterns, %d conventions, %d decisions). "+
			"Native diff-only mode — run with DONMAI_ARCH_BIN for full LLM-backed drift assessment.",
			len(observations), patternCount, conventionCount, decisionCount),
	}

	if gated {
		policyDesc := policy
		if policy == "" {
			policyDesc = "no-severity-high"
		}
		parts = append(parts, fmt.Sprintf("Change is BLOCKED by gate policy: %s.", policyDesc))
	}

	return strings.Join(parts, " ")
}
