package landing

import (
	"context"
	"time"
)

// FileManifest is the set of files a proposal's source branch modifies relative
// to the target branch. The conflict graph consumes these to decide which
// proposals may land concurrently.
//
// Ported from donmai-libraries merge-queue/file-manifest.ts (PRFileManifest).
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
// introduced on the source branch since divergence are reported — changes that
// landed on target after the branch point are excluded.
//
// A diff failure (e.g. the branch was never fetched) yields a nil slice with no
// error, so the proposal is treated as a universal conflict by the graph — the
// same fail-safe as the TS source.
func BuildFileManifest(ctx context.Context, repoPath, sourceBranch, targetBranch, remote string) ([]string, error) {
	return buildFileManifest(ctx, defaultRunner, repoPath, sourceBranch, targetBranch, remote)
}

// buildFileManifest is the runner-injectable implementation.
func buildFileManifest(ctx context.Context, r commandRunner, repoPath, sourceBranch, targetBranch, remote string) ([]string, error) {
	out, err := r.run(ctx, repoPath, nil,
		"git", "diff", "--name-only", remote+"/"+targetBranch+"..."+sourceBranch)
	if err != nil {
		// Fail-safe: an empty manifest makes the graph treat this proposal as
		// conflicting with everything, so an uncomputed diff never lands a
		// change concurrently with an unknown one.
		return nil, nil
	}
	return splitLines(out), nil
}

// BuildFileManifests builds manifests for multiple proposals, preserving input
// order. ComputedAt is stamped per manifest. Errors from individual diffs are
// not surfaced (each falls back to an empty manifest), matching the TS source.
func BuildFileManifests(ctx context.Context, repoPath string, entries []ManifestEntry, targetBranch, remote string) ([]FileManifest, error) {
	return buildFileManifests(ctx, defaultRunner, repoPath, entries, targetBranch, remote, time.Now)
}

// buildFileManifests is the runner-injectable implementation. now supplies the
// ComputedAt timestamp so tests are deterministic.
func buildFileManifests(ctx context.Context, r commandRunner, repoPath string, entries []ManifestEntry, targetBranch, remote string, now func() time.Time) ([]FileManifest, error) {
	manifests := make([]FileManifest, 0, len(entries))
	for _, e := range entries {
		files, _ := buildFileManifest(ctx, r, repoPath, e.SourceBranch, targetBranch, remote)
		manifests = append(manifests, FileManifest{
			Proposal:     e.Proposal,
			SourceBranch: e.SourceBranch,
			Files:        files,
			ComputedAt:   now(),
		})
	}
	return manifests, nil
}
