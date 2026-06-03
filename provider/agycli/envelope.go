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
