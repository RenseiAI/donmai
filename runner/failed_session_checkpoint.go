package runner

import (
	"context"
	"fmt"
	"regexp"
	"strings"
	"time"
)

var safeWIPSessionID = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9._-]*$`)

// wipRefAttemptStamp formats the per-attempt suffix of a failed-session WIP
// ref: a UTC stamp plus the runner's unique suffix for this teardown, so two
// failures of one session land on distinct refs. A reused bare ref would
// accept only its first push: the WIP commit is a commit-tree outside local
// history, so a second push of the same ref is rejected as non-fast-forward.
// The stamp sorts lexicographically, so the newest attempt is the last ref.
func wipRefAttemptStamp(t time.Time) string {
	return t.UTC().Format("20060102T150405Z")
}

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
	// Per-attempt ref: a reused bare session ref accepts only its first
	// push (the WIP commit sits outside local history), so a re-entered
	// generation or a racing stale worker would be rejected. The stamp is
	// part of the recorded ref, so the next session knows which ref to
	// fetch. Changing the layout afterwards would cause directory/file ref
	// conflicts, so the per-attempt shape ships before any host pushes it.
	ref := "refs/wip/" + qw.SessionID + "/" + wipRefAttemptStamp(r.now())
	checkpoint, err := createWIPCheckpoint(ctx, qw, target, ref)
	if err != nil {
		warn(err)
		return
	}
	res.WIPRef = checkpoint
	if checkpoint != nil {
		r.logger.Info("failed-session WIP checkpoint pushed", "sessionId", qw.SessionID, "ref", checkpoint.Ref, "commitSha", checkpoint.CommitSHA)
	}
}
