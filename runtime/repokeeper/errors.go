package repokeeper

import "errors"

// Sentinel errors returned by this package. Identity mismatches fail closed:
// a stale or foreign mirror is never served under a different remote or
// scope.
var (
	// ErrScopeMismatch reports that an already-on-disk mirror was recorded
	// for a different credential scope than the one requested — a
	// fail-closed refusal to mix repository content across credential
	// scopes. Neither value is echoed in the error text.
	ErrScopeMismatch = errors.New("repo keeper: existing mirror scope does not match requested scope")

	// ErrOriginMismatch reports that an already-on-disk mirror's recorded
	// origin does not match the requested canonical remote — a fail-closed
	// refusal to silently reuse content from the wrong remote. Neither URL
	// is echoed in the error text.
	ErrOriginMismatch = errors.New("repo keeper: existing mirror origin does not match requested remote")

	// ErrSameFilesystem reports that the keeper root and the worktree root
	// do not share a filesystem. Later consumers clone worktrees from these
	// mirrors; a cross-filesystem layout would silently change copy cost
	// and sharing behavior, so the store refuses to start there.
	ErrSameFilesystem = errors.New("repo keeper: keeper root and worktree root are on different filesystems")
)
