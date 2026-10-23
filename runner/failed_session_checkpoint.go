package runner

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"regexp"
	"strings"
	"time"
)

var safeWIPSessionID = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9._-]*$`)

// wipRefAttemptNow and wipRefAttemptRand are the clock and entropy
// behind per-attempt reserved refs. Variables so a test can pin both
// and assert two failures of one session land on distinct refs.
var (
	wipRefAttemptNow  = time.Now
	wipRefAttemptRand = func() string {
		var raw [4]byte
		if _, err := rand.Read(raw[:]); err != nil {
			return "fallback"
		}
		return hex.EncodeToString(raw[:])
	}
)

// wipAttemptRef returns the reserved ref for one failed-session
// checkpoint attempt. The ref is per-attempt
// (refs/wip/<session>/<utc-stamp>-<rand>) rather than per-session
// (refs/wip/<session>): the WIP commit is a commit-tree outside local
// history, so a second failure of the same session can never
// fast-forward a per-session ref — the probe got
// "! [rejected] (non-fast-forward)" — and a stale lost-ownership
// worker racing the new owner would collide on it. A per-attempt leaf
// keeps every failure's work fetchable without ever overwriting, and
// the layout is fixed before any host pushes the per-session shape so
// a later change cannot cause directory/file ref conflicts.
func wipAttemptRef(sessionID string) string {
	stamp := wipRefAttemptNow().UTC().Format("20060102T150405Z")
	return "refs/wip/" + sessionID + "/" + stamp + "-" + wipRefAttemptRand()
}

// checkpointFailedSession supplements local rescue with a bounded remote copy.
func (r *Runner) checkpointFailedSession(qw QueuedWork, res *Result) {
	if res == nil || res.Status == "completed" || !RequiresPRURL(qw.WorkType, qw.WorkTypeRole) || res.ResumeCheckpoint != nil {
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
	checkpoint, warnings, err := createWIPCheckpoint(ctx, qw, target, wipAttemptRef(qw.SessionID))
	res.PostSessionWarnings = append(res.PostSessionWarnings, warnings...)
	if err != nil {
		warn(err)
		return
	}
	res.WIPRef = checkpoint
	if checkpoint != nil {
		r.logger.Info("failed-session WIP checkpoint pushed", "sessionId", qw.SessionID, "ref", checkpoint.Ref, "commitSha", checkpoint.CommitSHA)
	}
}
