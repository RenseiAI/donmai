package agycli

import (
	"encoding/json"
	"strings"
)

// resultEnvelope is the parsed form of the injected JSON result envelope.
type resultEnvelope struct {
	Status  string `json:"status"`  // "passed" | "failed"
	Summary string `json:"summary"` // one-sentence summary
}

// extractEnvelope scans agy's full stdout for the LAST
// <<<DONMAI_RESULT>>> … <<<END_DONMAI_RESULT>>> block and parses the JSON
// between the markers.
//
// Returns the parsed envelope, the raw JSON text (whitespace-trimmed), and
// ok=true only when both markers are present AND the body parses as a JSON
// object. The last block is preferred so that if the model echoes the
// instruction earlier in its narration, the genuine final result wins.
//
// A missing or unparseable envelope is non-fatal: ok=false and the caller
// falls back to the trailing stdout text. This keeps a model that ignored the
// instruction (or a future agy that formats differently) fully functional.
func extractEnvelope(stdout string) (env resultEnvelope, rawJSON string, ok bool) {
	endIdx := strings.LastIndex(stdout, resultEnvelopeEnd)
	if endIdx < 0 {
		return resultEnvelope{}, "", false
	}
	// Find the begin marker that precedes this end marker.
	beginIdx := strings.LastIndex(stdout[:endIdx], resultEnvelopeBegin)
	if beginIdx < 0 {
		return resultEnvelope{}, "", false
	}
	body := stdout[beginIdx+len(resultEnvelopeBegin) : endIdx]
	body = strings.TrimSpace(body)
	if body == "" {
		return resultEnvelope{}, "", false
	}

	// The body should be a single JSON object. Be tolerant of a code fence the
	// model may have wrapped it in.
	body = strings.TrimPrefix(body, "```json")
	body = strings.TrimPrefix(body, "```")
	body = strings.TrimSuffix(body, "```")
	body = strings.TrimSpace(body)

	var parsed resultEnvelope
	if err := json.Unmarshal([]byte(body), &parsed); err != nil {
		return resultEnvelope{}, body, false
	}
	return parsed, body, true
}

// envelopeLineFilter tracks whether a line stream is inside a
// <<<DONMAI_RESULT>>> … <<<END_DONMAI_RESULT>>> block so envelope lines can
// be withheld from AssistantTextEvent emission. The lines still reach the
// retained buffer — buildResult/extractEnvelope parse them from there — this
// filter only stops the raw envelope JSON from surfacing as "thought"
// activities.
//
// Marker detection is per flushed line: pty reads that split a line are
// reassembled by the caller's line carry before flushing, so a marker is
// only ever missed if a single line exceeds the forced-flush cap (markers
// and the one-line envelope JSON are tiny in practice).
type envelopeLineFilter struct {
	inEnvelope bool
}

// suppress reports whether line should be withheld from emission, updating
// the in-envelope state. Lines carrying a marker are themselves suppressed;
// a line containing both markers closes the block again.
func (f *envelopeLineFilter) suppress(line string) bool {
	if f.inEnvelope {
		if strings.Contains(line, resultEnvelopeEnd) {
			f.inEnvelope = false
		}
		return true
	}
	idx := strings.Index(line, resultEnvelopeBegin)
	if idx < 0 {
		return false
	}
	rest := line[idx+len(resultEnvelopeBegin):]
	f.inEnvelope = !strings.Contains(rest, resultEnvelopeEnd)
	return true
}

// successFromEnvelope maps a parsed envelope status to a bool. Defaults to the
// fallback when the status is absent or unrecognized.
func successFromEnvelope(env resultEnvelope, fallback bool) bool {
	switch strings.ToLower(strings.TrimSpace(env.Status)) {
	case "passed", "pass", "success", "ok":
		return true
	case "failed", "fail", "error":
		return false
	default:
		return fallback
	}
}
