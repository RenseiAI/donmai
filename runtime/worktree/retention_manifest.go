// Package worktree — retention_manifest.go: minimal manifest field reader
// for the retention archive-drop pass.
//
// The archive manifest is daemon-owned; the retention pass needs only the
// creation timestamp to age archives. This reader extracts one top-level
// string field without binding the manifest schema, so manifest evolution
// cannot break the drop pass.
package worktree

import (
	"encoding/json"
)

// extractManifestStringField returns the top-level string field of a JSON
// document. Nested objects decode to nil and report absent: only a plain
// string value counts.
func extractManifestStringField(raw []byte, field string) (string, bool) {
	var doc map[string]json.RawMessage
	if err := json.Unmarshal(raw, &doc); err != nil {
		return "", false
	}
	encoded, ok := doc[field]
	if !ok {
		return "", false
	}
	var value string
	if err := json.Unmarshal(encoded, &value); err != nil {
		return "", false
	}
	return value, true
}
