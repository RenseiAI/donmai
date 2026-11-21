package runner

import (
	"encoding/json"
	"regexp"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/RenseiAI/donmai/agent"
)

// Bounds that keep posted findings inside the control plane's manifest
// budget. Findings share the stored turn result with the summary, so the
// runner orders blocking findings first and sheds from the tail: what is cut
// is always the least important.
const (
	// MaxManifestFindings is the per-manifest finding count the control
	// plane keeps.
	MaxManifestFindings = 50
	// MaxManifestFindingsBytes bounds the serialized findings array.
	MaxManifestFindingsBytes = 3072
	// maxFindingTextRunes bounds a finding's title and fix text.
	maxFindingTextRunes = 240
	// maxFindingPathRunes bounds a finding's path.
	maxFindingPathRunes = 512
	// defaultFindingCategory is the category given to a finding the agent
	// did not categorize; the control plane drops category-less findings.
	defaultFindingCategory = "other"
)

var (
	findingIDPattern  = regexp.MustCompile(`^B\d{1,3}$`)
	findingSeverities = map[string]string{
		"low": "low", "minor": "low", "nit": "low",
		"medium": "medium", "moderate": "medium",
		"high": "high", "major": "high",
		"critical": "critical", "blocker": "critical",
	}
)

// NormalizeManifestFindings makes agent-written findings acceptable to the
// control plane's finding contract and bounds them: findings with nothing to
// say (no id, title, path or fix) are dropped; a missing category becomes
// "other"; severity is mapped onto low/medium/high/critical (a blocking
// finding with no recognisable severity is "high", else "medium"); an id that
// is not a gate row id (B1…B999) is dropped; text is trimmed and bounded.
// Blocking findings sort first (stable), then the list is cut to
// MaxManifestFindings and MaxManifestFindingsBytes.
func NormalizeManifestFindings(in []agent.TurnManifestFinding) []agent.TurnManifestFinding {
	out := make([]agent.TurnManifestFinding, 0, len(in))
	for _, f := range in {
		f.ID = strings.TrimSpace(f.ID)
		if !findingIDPattern.MatchString(f.ID) {
			f.ID = ""
		}
		f.Title = clipRunes(strings.TrimSpace(f.Title), maxFindingTextRunes)
		f.Fix = clipRunes(strings.TrimSpace(f.Fix), maxFindingTextRunes)
		f.Path = clipRunes(strings.TrimSpace(f.Path), maxFindingPathRunes)
		if f.ID == "" && f.Title == "" && f.Path == "" && f.Fix == "" {
			continue
		}
		if f.Category = strings.TrimSpace(f.Category); f.Category == "" {
			f.Category = defaultFindingCategory
		}
		blocking := f.Blocking != nil && *f.Blocking
		if sev, ok := findingSeverities[strings.ToLower(strings.TrimSpace(f.Severity))]; ok {
			f.Severity = sev
		} else if blocking {
			f.Severity = "high"
		} else {
			f.Severity = "medium"
		}
		out = append(out, f)
	}
	slices.SortStableFunc(out, func(a, b agent.TurnManifestFinding) int {
		ab, bb := a.Blocking != nil && *a.Blocking, b.Blocking != nil && *b.Blocking
		switch {
		case ab == bb:
			return 0
		case ab:
			return -1
		}
		return 1
	})
	if len(out) > MaxManifestFindings {
		out = out[:MaxManifestFindings]
	}
	for len(out) > 0 {
		data, err := json.Marshal(out)
		if err == nil && len(data) <= MaxManifestFindingsBytes {
			break
		}
		out = out[:len(out)-1]
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// clipRunes cuts s to at most n runes on a rune boundary.
func clipRunes(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	r := []rune(s)
	return string(r[:n])
}
