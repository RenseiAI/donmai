package runner

import (
	"fmt"
	"strings"

	"github.com/RenseiAI/donmai/agent"
)

// toolOutputTruncatedInfix is the stable infix every truncated tool result
// carries, so operators and tests can recognise a bounded payload without
// scraping byte counts. It aliases agent.ToolOutputTruncatedInfix so the
// runner and every harness mark a truncation identically.
const toolOutputTruncatedInfix = agent.ToolOutputTruncatedInfix

// toolOutputTruncationWarningPrefix prefixes the post-session warning
// recorded when a turn's tool output was truncated. The prefix dedups the
// warning across the several streams one session merges.
const toolOutputTruncationWarningPrefix = "tool output exceeded"

// truncateToolOutput bounds one tool result's content to limit bytes,
// preserving the head and the tail around a marker that names the
// truncation. It is a thin alias over the shared agent helper so the runner
// and every harness apply byte-identical, rune-safe truncation around the
// same marker (see agent.TruncateToolOutput). In the runner it bounds the
// runner's own copy of a harness ToolResultEvent — what the runner retains
// or forwards; each harness separately bounds the copy it hands the model.
func truncateToolOutput(content string, limit int) (string, bool) {
	return agent.TruncateToolOutput(content, limit)
}

// toolOutputLimit returns the per-tool-result byte bound this Runner
// retains. Hand-built Runners that bypass New resolve to the default, so
// the cap holds on every construction path. The default is the same
// documented limit every harness applies before handing tool output to the
// model (see agent.DefaultMaxToolOutputBytes).
func (r *Runner) toolOutputLimit() int {
	if r == nil || r.maxToolOutputBytes <= 0 {
		return DefaultMaxToolOutputBytes
	}
	return r.maxToolOutputBytes
}

// noteToolOutputTruncation records an oversized tool result the runner
// truncated as a post-session warning — observability, never a failure
// — so the truncation of the runner's retained copy stays visible
// wherever warnings surface. Deduped by prefix: applyTo runs once per
// stream and several streams merge onto one envelope.
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
		fmt.Sprintf("tool output exceeded the %d-byte limit and was truncated with a marker before the runner retained or forwarded it (largest result %d bytes); the seat kept going", o.toolOutputLimit, o.toolOutputBytes))
}
