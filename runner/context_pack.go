package runner

import (
	"log/slog"

	"github.com/RenseiAI/donmai/agent"
	"github.com/RenseiAI/donmai/runtime/workarea"
)

// executorSupportsContextPack reports whether the bound executor proved the
// exact negotiated-document protocol. Advertisement matches only the executor
// path that actually materializes the contract: a missing attestation refuses
// the carrier before any document is visible.
func executorSupportsContextPack(provider agent.Provider) bool {
	harness, ok := provider.(agent.HarnessProvider)
	if !ok {
		return false
	}
	return workarea.SupportsContextPack(harness.Manifest().Caps.ContextPackProtocols)
}

// materializeContextPack validates the versioned carrier and publishes its
// bytes under the session-owned context directory before the first executor
// turn. Legacy work without a carrier is unchanged. Any refusal fails the
// session before spawn: unsupported or rejected documents are never claimed
// as included.
func (r *Runner) materializeContextPack(qw QueuedWork, workareaRoot string, res *Result, provider agent.Provider, logger *slog.Logger) error {
	if len(qw.ContextPack) == 0 {
		return nil
	}
	if !executorSupportsContextPack(provider) {
		err := &workarea.ContextPackError{Reason: workarea.ReasonContextPackSupport, Detail: "bound executor did not attest the negotiated-document protocol"}
		res.Status, res.FailureMode, res.Error = "failed", FailureWorktreeProvision, err.Error()
		return err
	}
	pack, err := workarea.DecodeContextPack(qw.ContextPack)
	if err != nil {
		res.Status, res.FailureMode, res.Error = "failed", FailureWorktreeProvision, err.Error()
		return err
	}
	report, err := workarea.MaterializeContextPack(workarea.RootPath(workareaRoot), pack)
	if err != nil {
		res.Status, res.FailureMode, res.Error = "failed", FailureWorktreeProvision, err.Error()
		return err
	}
	res.ContextPack = &report
	if logger != nil {
		logger.Info("negotiated documents materialized",
			"sessionId", qw.SessionID,
			"packId", report.PackID,
			"included", len(report.Included),
		)
	}
	return nil
}
