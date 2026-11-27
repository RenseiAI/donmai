package present

import (
	"encoding/json"
	"strings"
)

// OutputText decodes tool output for display: a JSON array of content parts
// (`[{"type":"text","text":"…"}]`, as MCP and Claude results send) becomes
// its texts joined by newlines, non-text parts become "[type]"; anything else
// is returned unchanged.
func OutputText(content string) string {
	trimmed := strings.TrimSpace(content)
	if !strings.HasPrefix(trimmed, "[") || !strings.HasSuffix(trimmed, "]") {
		return content
	}
	var parts []map[string]any
	if err := json.Unmarshal([]byte(trimmed), &parts); err != nil || len(parts) == 0 {
		return content
	}
	texts := make([]string, 0, len(parts))
	for _, p := range parts {
		typ, _ := p["type"].(string)
		if typ == "" {
			return content
		}
		if typ == "text" {
			s, ok := p["text"].(string)
			if !ok {
				return content
			}
			texts = append(texts, s)
			continue
		}
		texts = append(texts, "["+typ+"]")
	}
	return strings.Join(texts, "\n")
}
