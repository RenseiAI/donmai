package sanitize

import (
	_ "embed"
	"encoding/base64"
	"encoding/json"
	"fmt"
)

//go:embed testdata/corpus.json
var corpusJSON []byte

// Entry is one shared conformance fixture. The corpus is language-neutral: input
// and expected_output are base64-encoded raw bytes, so the relay (W5), the web
// viewer (W7), and the iOS viewer (W11) all decode and check against the same
// file. Every entry is evaluated against a sanitizer built with the reference
// defaults (New / DefaultHoldMaxBytes / DefaultSixelMaxBytes): feeding Input
// (contiguously, AND split at any boundary) MUST yield ExpectedOutput.
type Entry struct {
	// Name is a stable, unique identifier for the fixture.
	Name string `json:"name"`
	// Description explains what the fixture exercises.
	Description string `json:"description"`
	// Input is the base64-encoded raw input byte stream.
	Input string `json:"input"`
	// ExpectedOutput is the base64-encoded raw sanitized output for Input under
	// the reference defaults.
	ExpectedOutput string `json:"expected_output"`
	// Disposition is the dominant §9 disposition: pass | strip | neutralize |
	// display-only | mixed.
	Disposition string `json:"disposition"`
	// SpecRow names the §9 table row (or edge case) the fixture covers.
	SpecRow string `json:"spec_row"`
}

// InputBytes decodes the base64 Input.
func (e Entry) InputBytes() ([]byte, error) {
	return base64.StdEncoding.DecodeString(e.Input)
}

// ExpectedOutputBytes decodes the base64 ExpectedOutput.
func (e Entry) ExpectedOutputBytes() ([]byte, error) {
	return base64.StdEncoding.DecodeString(e.ExpectedOutput)
}

// ConformanceCorpus returns the shared §9 conformance fixtures embedded from
// testdata/corpus.json. Other implementations (relay, web, iOS) consume the
// same JSON file directly; this loader is the Go entry point.
func ConformanceCorpus() ([]Entry, error) {
	var entries []Entry
	if err := json.Unmarshal(corpusJSON, &entries); err != nil {
		return nil, fmt.Errorf("sanitize: decode conformance corpus: %w", err)
	}
	return entries, nil
}
