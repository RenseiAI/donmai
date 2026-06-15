package landing

import (
	"context"
	"fmt"
	"time"
)

// FileManifest is the set of files a proposal's source branch modifies relative
// to the target branch. The conflict graph consumes these to decide which
// proposals may land concurrently.
//
// Ported from donmai-libraries merge-queue/file-manifest.ts.
type FileManifest struct {
	Proposal     int
	SourceBranch string
	Files        []string
	ComputedAt   time.Time
}

// ManifestEntry identifies a proposal + its source branch for manifest building.
type ManifestEntry struct {
	Proposal     int
	SourceBranch string
}

// BuildFileManifest returns the files modified by sourceBranch relative to
// targetBranch using a three-dot diff (target...source), so only changes
// introduced on the source branch since divergence are reported.
//
// A diff failure (e.g. the branch was never fetched) yields an empty slice with
// no error, so the proposal is treated as a universal conflict by the graph —
// the same fail-safe as the TS source.
//
// Stub: not yet ported.
func BuildFileManifest(ctx context.Context, repoPath, sourceBranch, targetBranch, remote string) ([]string, error) {
	_ = ctx
	_ = repoPath
	_ = sourceBranch
	_ = targetBranch
	_ = remote
	return nil, fmt.Errorf("BuildFileManifest: %w", ErrNotImplemented)
}

// BuildFileManifests builds manifests for multiple proposals, preserving input
// order.
//
// Stub: not yet ported.
func BuildFileManifests(ctx context.Context, repoPath string, entries []ManifestEntry, targetBranch, remote string) ([]FileManifest, error) {
	_ = ctx
	_ = repoPath
	_ = entries
	_ = targetBranch
	_ = remote
	return nil, fmt.Errorf("BuildFileManifests: %w", ErrNotImplemented)
}
