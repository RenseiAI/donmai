package runner

import (
	"errors"
	"os"
	"path/filepath"

	"github.com/RenseiAI/donmai/runtime/state"
)

var errRunJournalBoundaryUnavailable = errors.New("runner: event journal boundary unavailable")

// stampRunJournalBoundary keeps the run fence only for the exact same
// session and incoming Runner.Run start observation. A distinct invocation
// gets a new boundary, even when its session ID is reused.
// The boundary is recorded before provider spawn and before event append.
// An existing journal whose size cannot be trusted stays unknown (nil).
func stampRunJournalBoundary(st *state.State, sessionID string, startedAt int64, worktreePath string) error {
	if startedAt > 0 && st.SessionID == sessionID && st.StartedAt == startedAt {
		return nil
	}
	boundary, err := measureRunJournalBoundary(worktreePath)
	st.StartedAt = startedAt
	st.EventLogStartOffset = boundary
	st.LastHeartbeat = 0
	return err
}

// measureRunJournalBoundary reads only filesystem metadata. Nonexistent
// journals start at byte zero; an existing file must be a readable regular
// file whose opened descriptor still matches the inspected path and size.
func measureRunJournalBoundary(worktreePath string) (*int64, error) {
	path := filepath.Join(worktreePath, state.AgentDirName, "events.jsonl")
	before, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		zero := int64(0)
		return &zero, nil
	}
	if err != nil || !before.Mode().IsRegular() {
		return nil, errRunJournalBoundaryUnavailable
	}
	//nolint:gosec // G304: worktreePath is the runner-owned provisioned path.
	file, err := os.Open(path)
	if err != nil {
		return nil, errRunJournalBoundaryUnavailable
	}
	defer func() { _ = file.Close() }()
	opened, err := file.Stat()
	if err != nil || !opened.Mode().IsRegular() || !os.SameFile(before, opened) ||
		before.Size() != opened.Size() || !before.ModTime().Equal(opened.ModTime()) {
		return nil, errRunJournalBoundaryUnavailable
	}
	size := opened.Size()
	return &size, nil
}
