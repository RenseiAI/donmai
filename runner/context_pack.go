package runner

import (
	"log/slog"
	"path/filepath"
	"strings"

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

// sessionDocumentPrefix returns the checkout-relative pack directory, with a
// trailing slash, that this session published negotiated documents into
// INSIDE checkout, or "" when there is none. Only the flat layout puts the
// pack inside a checkout (the checkout is the workarea root); a declared
// layout publishes beside its repository leaves. A session that carried no
// pack, or whose pack was refused, gets "" and keeps exactly the
// session-less exclusion rules: nothing else under .workarea/, and no
// nested sub/.workarea/, is ever treated as session machinery.
func sessionDocumentPrefix(res *Result, checkout string) string {
	if res == nil || res.ContextPack == nil || res.ContextPack.Dir == "" || checkout == "" || res.WorkareaRoot == "" {
		return ""
	}
	if filepath.Clean(checkout) != filepath.Clean(res.WorkareaRoot) {
		return ""
	}
	return strings.TrimSuffix(res.ContextPack.Dir, "/") + "/"
}

// underSessionDocuments reports whether the checkout-relative path is the
// session's pack directory or inside it. An empty prefix matches nothing.
func underSessionDocuments(prefix, path string) bool {
	if prefix == "" || path == "" {
		return false
	}
	return path == strings.TrimSuffix(prefix, "/") || strings.HasPrefix(path, prefix)
}
