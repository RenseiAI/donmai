package linear

import (
	"fmt"
	"strings"
)

// Cheap issue-reading field sets.
//
// list-issues defaults to the slim row so bulk enumeration stays small:
// human-readable id, title, status, project, manual rank, parent reference
// and labels. Anything beyond that is opt-in per call via --fields or
// --full, which keeps shared API-budget use proportional to what the caller
// actually reads.

// ListIssueSlimFields is the default list-issues column set.
var ListIssueSlimFields = []string{"id", "title", "status", "project", "sortOrder", "parent", "labels"}

// ListIssueFullFields is the complete selectable column set for list-issues
// rows (--full). It is a superset of ListIssueSlimFields.
var ListIssueFullFields = []string{"id", "title", "status", "priority", "sortOrder", "estimate", "labels", "project", "assignee", "createdAt", "parent"}

// GetIssueFields is the complete selectable field set for get-issue output.
// The get-issue default is the full set; --fields subsets it.
var GetIssueFields = []string{"id", "identifier", "title", "description", "url", "status", "team", "project", "labels", "parentId", "parentIdentifier", "priority", "sortOrder", "estimate", "createdAt", "updatedAt"}

// parentExpandedKeys is the key expansion behind the logical "parent" column:
// a parent reference is only useful with both the id and its human-readable
// identifier, so selecting "parent" emits both keys.
var parentExpandedKeys = []string{"parentId", "parentIdentifier"}

// ParseIssueFields parses a comma-separated --fields value against universe.
// Matching is case-insensitive and surrounding whitespace is ignored; repeated
// names collapse to their first occurrence. Unknown or empty names fail closed
// with an error naming every valid field, so a typo never silently returns a
// narrower row than the caller asked for.
func ParseIssueFields(raw string, universe []string) ([]string, error) {
	parts := strings.Split(raw, ",")
	selected := make([]string, 0, len(parts))
	seen := make(map[string]bool, len(parts))
	for _, part := range parts {
		name := strings.TrimSpace(part)
		if name == "" {
			return nil, fmt.Errorf("empty field name in %q (valid fields: %s)", raw, strings.Join(universe, ", "))
		}
		canonical := ""
		for _, valid := range universe {
			if strings.EqualFold(name, valid) {
				canonical = valid
				break
			}
		}
		if canonical == "" {
			return nil, fmt.Errorf("unknown field %q (valid fields: %s)", strings.TrimSpace(part), strings.Join(universe, ", "))
		}
		if !seen[canonical] {
			seen[canonical] = true
			selected = append(selected, canonical)
		}
	}
	if len(selected) == 0 {
		return nil, fmt.Errorf("no fields selected in %q (valid fields: %s)", raw, strings.Join(universe, ", "))
	}
	return selected, nil
}

// SelectIssueKeys restricts row to fields, expanding the logical "parent"
// column to its parentId/parentIdentifier key pair. Keys listed in fields are
// copied when present; the full row a command builds always carries every
// universe key (null-valued where the issue has no value), so selection never
// invents data.
func SelectIssueKeys(row map[string]any, fields []string) map[string]any {
	out := make(map[string]any, len(fields)+1)
	for _, field := range fields {
		if field == "parent" {
			for _, key := range parentExpandedKeys {
				if value, ok := row[key]; ok {
					out[key] = value
				}
			}
			continue
		}
		if value, ok := row[field]; ok {
			out[field] = value
		}
	}
	return out
}

// IsSlimIssueFieldSet reports whether fields is exactly the slim default.
// Commands use it to echo the effective shape back in their output envelope.
func IsSlimIssueFieldSet(fields []string) bool {
	if len(fields) != len(ListIssueSlimFields) {
		return false
	}
	for i, field := range fields {
		if field != ListIssueSlimFields[i] {
			return false
		}
	}
	return true
}
