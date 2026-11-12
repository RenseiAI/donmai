package runner

import (
	"fmt"
	"strings"

	"github.com/RenseiAI/donmai/agent"
)

// toolOutputTruncationWarningPrefix prefixes the post-session warning
// recorded when a turn's tool output was truncated. The prefix dedups the
// warning across the several streams one session merges.
const toolOutputTruncationWarningPrefix = "tool output exceeded"

// toolOutputLimit returns the per-tool-result byte bound this Runner
// retains. Hand-built Runners that bypass New resolve to the default, so
// the cap holds on every construction path. It is the same documented
// limit every harness applies before handing tool output to the model
// (see agent.DefaultMaxToolOutputBytes).
func (r *Runner) toolOutputLimit() int {
	if r == nil || r.maxToolOutputBytes <= 0 {
		return DefaultMaxToolOutputBytes
	}
	return r.maxToolOutputBytes
}

// truncateToolOutput bounds one tool result's content to limit bytes. It is
// a thin alias over the shared agent helper so the runner and every harness
// apply byte-identical truncation around the same marker.
func truncateToolOutput(content string, limit int) (string, bool) {
	return agent.TruncateToolOutput(content, limit)
}

// noteToolOutputTruncation records an oversized tool result the runner
// truncated as a post-session warning — observability, never a failure —
// so the bound copy the seat kept going on stays visible wherever warnings
// surface. Deduped by prefix: applyTo runs once per stream and several
// streams merge onto one envelope.
func noteToolOutputTruncation(res *Result, o streamObservation) {
	if res == nil || !o.toolOutputTruncated {
		return
	}
	for _, w := range res.PostSessionWarnings {
		if strings.HasPrefix(w, toolOutputTruncationWarningPrefix) {
			return
		}
	}
	res.PostSessionWarnings = append(res.PostSessionWarnings,
		fmt.Sprintf("tool output exceeded the %d-byte limit and was truncated with a marker (largest result %d bytes); the seat kept going on the bounded copy", o.toolOutputLimit, o.toolOutputBytes))
}
