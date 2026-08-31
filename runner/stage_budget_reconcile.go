package runner

import (
	"encoding/json"
	"errors"
	"fmt"

	"github.com/RenseiAI/donmai/prompt"
)

// ReconcileStageBudget reconciles the daemon SessionDetail's sibling
// stageBudget compatibility mirror with qw. Raw JSON keeps this shared
// constructor independent of package daemon, which already imports runner and
// therefore cannot be imported here.
//
// Call this after QueuedWork has been decoded from OperationalPayload and
// before building a prepared harness. For receipt-bearing work, StageBudget is
// already immutable operational authority: the sibling must match its
// absence/presence and value exactly, and never overwrites it. Only legacy work
// with no authoritative raw OperationalPayload adopts the sibling value.
func ReconcileStageBudget(qw QueuedWork, stageBudgetJSON json.RawMessage) (QueuedWork, error) {
	var sibling *prompt.StageBudget
	if len(stageBudgetJSON) > 0 {
		if err := json.Unmarshal(stageBudgetJSON, &sibling); err != nil {
			return qw, fmt.Errorf("runner: decode stage budget: %w", err)
		}
	}
	if len(qw.OperationalPayload) == 0 {
		qw.StageBudget = sibling
		return qw, nil
	}
	if !stageBudgetsEqual(qw.StageBudget, sibling) {
		return qw, errors.New("runner: stage budget compatibility mirror differs from operational payload")
	}
	return qw, nil
}

func stageBudgetsEqual(left, right *prompt.StageBudget) bool {
	if left == nil || right == nil {
		return left == right
	}
	return *left == *right
}
