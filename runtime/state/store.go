package state

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/RenseiAI/agentfactory-tui/agent"
)

// AgentDirName is the conventional sub-directory name inside a worktree
// where state.json (and friends — heartbeat.json, todos.json) live.
const AgentDirName = ".agent"

// StateFileName is the conventional file name for the state file.
const StateFileName = "state.json"

// State is the persisted per-session state.json document. It mirrors
// the legacy TS WorktreeState shape (state-types.ts) closely enough
// that a worktree initialized by either runner can be inspected by
// either reader. New fields go at the bottom; do not reorder.
//
// Time fields are stored as Unix-millisecond integers to match the
// legacy TS Date.now() encoding.
type State struct {
	// IssueID is the platform-side issue UUID.
	IssueID string `json:"issueId"`

	// IssueIdentifier is the human-readable issue id (e.g. REN-1234).
	// The cross-issue recovery guard compares this to an expected
	// value before allowing reuse.
	IssueIdentifier string `json:"issueIdentifier"`

	// SessionID is the platform-side session UUID. Populated as soon
	// as the runner has claimed the work.
	SessionID string `json:"sessionId,omitempty"`

	// ProviderName is the agent provider that ran (or is running) this
	// session.
	ProviderName agent.ProviderName `json:"providerName,omitempty"`

	// ProviderSessionID is the provider-native session id captured
	// from agent.InitEvent. Empty until the first init event fires.
	ProviderSessionID string `json:"providerSessionId,omitempty"`

	// WorkType is the work-type slug (development/qa/...).
	WorkType string `json:"workType,omitempty"`

	// CurrentStep is a runner-level descriptor of the current
	// orchestration phase (e.g. "spawning", "streaming", "backstop").
	CurrentStep string `json:"currentStep,omitempty"`

	// AttemptCount tracks how many times the runner has attempted this
	// session — incremented on retry-after-failure flows.
	AttemptCount int `json:"attemptCount"`

	// StartedAt is the unix-ms timestamp of session start.
	StartedAt int64 `json:"startedAt"`

	// LastUpdatedAt is the unix-ms timestamp of the most recent
	// Update/Write — kept fresh by every state mutation.
	LastUpdatedAt int64 `json:"lastUpdatedAt"`

	// LastHeartbeat is the unix-ms timestamp of the most recent
	// session heartbeat the runner observed. The heartbeat package
	// owns the increment; state stores the snapshot for forensics.
	LastHeartbeat int64 `json:"lastHeartbeat,omitempty"`

	// PID is the agent provider subprocess pid, or 0 for multiplexed
	// providers (codex app-server). Populated via Spec.OnProcessSpawned.
	PID int `json:"pid,omitempty"`

	// WorkerID is the daemon worker that owns this session.
	WorkerID string `json:"workerId,omitempty"`
}

// Sentinel errors. Callers may type-check these via errors.Is.
var (
	// ErrNotFound is returned by Read when the state file does not
	// exist on disk.
	ErrNotFound = errors.New("runtime/state: state.json not found")

	// ErrMalformed is returned by Read when the state file exists but
	// cannot be parsed as a State document. Callers may handle this
	// by logging + treating the worktree as fresh.
	ErrMalformed = errors.New("runtime/state: state.json malformed")

	// ErrIdentifierMismatch is returned by ReadExpect when the on-disk
	// state belongs to a different issue identifier than the caller
	// expects. The runner refuses to reuse cross-issue state.
	ErrIdentifierMismatch = errors.New("runtime/state: state.json identifier mismatch")
)

// Store owns the .agent/state.json file inside one or more worktrees.
//
// A single Store is safe to use concurrently across worktrees: each
// Update call serializes through the per-worktree mutex held inside
// Store.locks. Different worktrees do not share a mutex, so they
// proceed in parallel.
//
// The zero value is valid.
type Store struct {
	mu    sync.Mutex
	locks map[string]*sync.Mutex // worktreePath → mutex
}

// NewStore returns an empty Store.
func NewStore() *Store {
	return &Store{}
}

// lockFor returns the per-worktree mutex used to serialize Update
// calls. Lazily initialized.
func (s *Store) lockFor(worktreePath string) *sync.Mutex {
	abs, _ := filepath.Abs(worktreePath)
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.locks == nil {
		s.locks = make(map[string]*sync.Mutex)
	}
	m, ok := s.locks[abs]
	if !ok {
		m = &sync.Mutex{}
		s.locks[abs] = m
	}
	return m
}

// Path returns the absolute path of the state.json file for a
// worktree. Useful for callers that need to log or stat the file.
func Path(worktreePath string) string {
	return filepath.Join(worktreePath, AgentDirName, StateFileName)
}

// Read parses the state.json under worktreePath. Returns ErrNotFound
// when the file does not exist and ErrMalformed when the bytes are not
// valid JSON.
func (s *Store) Read(worktreePath string) (*State, error) {
	body, err := os.ReadFile(Path(worktreePath))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("runtime/state: read: %w", err)
	}
	var st State
	if err := json.Unmarshal(body, &st); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrMalformed, err)
	}
	return &st, nil
}

// ReadExpect is like Read but enforces the cross-issue recovery guard:
// when expectedIdentifier is non-empty and the on-disk state belongs
// to a different issue, ReadExpect returns ErrIdentifierMismatch
// (still wrapping the loaded *State so callers can log forensics).
func (s *Store) ReadExpect(worktreePath, expectedIdentifier string) (*State, error) {
	st, err := s.Read(worktreePath)
	if err != nil {
		return nil, err
	}
	if expectedIdentifier != "" && st.IssueIdentifier != "" && st.IssueIdentifier != expectedIdentifier {
		return st, fmt.Errorf("%w: on-disk=%q expected=%q",
			ErrIdentifierMismatch, st.IssueIdentifier, expectedIdentifier)
	}
	return st, nil
}

// Write atomically replaces the state.json under worktreePath with the
// given State. Creates .agent/ when missing. The write is tmpfile +
// rename so a crash mid-write cannot leave a half-written file.
//
// LastUpdatedAt is overwritten with the current monotonic-millisecond
// timestamp so callers do not have to set it themselves.
func (s *Store) Write(worktreePath string, st *State) error {
	if st == nil {
		return errors.New("runtime/state: nil state")
	}
	mu := s.lockFor(worktreePath)
	mu.Lock()
	defer mu.Unlock()
	return s.writeLocked(worktreePath, st)
}

// writeLocked is Write without the per-worktree mutex acquire — used
// internally by Update which already holds the lock.
func (s *Store) writeLocked(worktreePath string, st *State) error {
	st.LastUpdatedAt = time.Now().UnixMilli()
	body, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return fmt.Errorf("runtime/state: marshal: %w", err)
	}
	dir := filepath.Join(worktreePath, AgentDirName)
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return fmt.Errorf("runtime/state: mkdir %q: %w", dir, err)
	}

	final := filepath.Join(dir, StateFileName)
	tmp, err := os.CreateTemp(dir, StateFileName+".tmp-*")
	if err != nil {
		return fmt.Errorf("runtime/state: create tmp: %w", err)
	}
	tmpName := tmp.Name()
	closed := false
	defer func() {
		if !closed {
			_ = tmp.Close()
		}
		_ = os.Remove(tmpName)
	}()
	if _, err := tmp.Write(body); err != nil {
		return fmt.Errorf("runtime/state: write tmp: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		return fmt.Errorf("runtime/state: sync tmp: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("runtime/state: close tmp: %w", err)
	}
	closed = true
	if err := os.Rename(tmpName, final); err != nil {
		return fmt.Errorf("runtime/state: rename: %w", err)
	}
	return nil
}

// Update reads the current state, applies fn, and writes the result —
// all under the per-worktree mutex so concurrent updates from the same
// process are serialized. fn may not return nil for *State; doing so
// is treated as "no change".
//
// When the state file does not exist, fn is called with a non-nil
// zero-value *State so callers can populate it on first run.
//
// Returns the post-update *State on success.
func (s *Store) Update(worktreePath string, fn func(*State) error) (*State, error) {
	mu := s.lockFor(worktreePath)
	mu.Lock()
	defer mu.Unlock()

	st, err := s.Read(worktreePath)
	switch {
	case errors.Is(err, ErrNotFound):
		st = &State{}
	case err != nil && !errors.Is(err, ErrMalformed):
		return nil, err
	case errors.Is(err, ErrMalformed):
		// Malformed file recovers to a zero-value State; the runner
		// will overwrite it with a fresh document.
		st = &State{}
	}
	if err := fn(st); err != nil {
		return nil, err
	}
	if err := s.writeLocked(worktreePath, st); err != nil {
		return nil, err
	}
	return st, nil
}
