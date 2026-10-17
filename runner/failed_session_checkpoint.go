package runner

import (
	"context"
	"fmt"
	"regexp"
	"strings"
)

var safeWIPSessionID = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9._-]*$`)

// checkpointFailedSession supplements local rescue with a bounded remote copy.
func (r *Runner) checkpointFailedSession(qw QueuedWork, res *Result) {
	if res == nil || res.Status == "completed" || !RequiresPRURL(qw.WorkType) || res.ResumeCheckpoint != nil {
		return
	}
	target, ok := providerErrorCheckpointTarget(res)
	if !ok {
		return
	}
	warn := func(err error) {
		warning := fmt.Sprintf("failed-session WIP checkpoint failed: %v", err)
		res.PostSessionWarnings = append(res.PostSessionWarnings, warning)
		r.logger.Warn(warning, "sessionId", qw.SessionID)
	}
	if !safeWIPSessionID.MatchString(qw.SessionID) || strings.Contains(qw.SessionID, "..") || strings.HasSuffix(qw.SessionID, ".") || strings.HasSuffix(qw.SessionID, ".lock") {
		warn(fmt.Errorf("unsafe session id for WIP ref"))
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), providerErrorCheckpointTimeout)
	defer cancel()
	checkpoint, err := createWIPCheckpoint(ctx, qw, target, "refs/wip/"+qw.SessionID)
	if err != nil {
		warn(err)
		return
	}
	res.WIPRef = checkpoint
	if checkpoint != nil {
		r.logger.Info("failed-session WIP checkpoint pushed", "sessionId", qw.SessionID, "ref", checkpoint.Ref, "commitSha", checkpoint.CommitSHA)
	}
}
