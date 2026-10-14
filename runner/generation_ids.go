package runner

import (
	"slices"
	"strings"

	"github.com/RenseiAI/donmai/agent"
)

// maxGenerationIDs bounds the terminal receipt and deduplication memory.
const maxGenerationIDs = 1024

func collectGenerationID(ids []string, event agent.Event) []string {
	var id string
	switch e := event.(type) {
	case agent.SystemEvent:
		if e.Subtype == agent.SystemSubtypeGenerationID || e.Subtype == agent.SystemSubtypeModelIdentity {
			id = e.GenerationID
		}
	case agent.LlmCallEvent:
		if !e.Synthetic && e.UsageSource != agent.LlmUsageAggregate {
			id = e.GenerationID
		}
	}
	// Response ids are opaque, short identifiers. Refuse oversized or blank
	// values rather than letting a malformed event inflate the terminal post.
	if strings.TrimSpace(id) == "" || len(id) > 256 || len(ids) >= maxGenerationIDs || slices.Contains(ids, id) {
		return ids
	}
	return append(ids, id)
}
