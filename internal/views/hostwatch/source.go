package hostwatch

import (
	"errors"
	"fmt"
	"path/filepath"
	"sort"
	"strings"

	"github.com/RenseiAI/donmai/afclient"
	"github.com/RenseiAI/donmai/runtime/state"
)

// daemonLister is the minimal slice of afclient.DaemonClient the source
// depends on. Narrowing to an interface lets tests inject a fake without a
// live daemon and keeps the source decoupled from the HTTP client.
type daemonLister interface {
	GetSessions() ([]afclient.DaemonSessionHandle, error)
	GetStatus() (*afclient.DaemonStatusResponse, error)
	GetStats(withPool, byMachine bool) (*afclient.DaemonStatsResponse, error)
}

// stateReader reads a session's on-disk state.json. *state.Store satisfies
// it; tests inject a fake.
type stateReader interface {
	Read(worktreePath string) (*state.State, error)
}

// SessionCard is the joined per-session view model: the daemon index entry
// (id, pid, state, worktree path, project, repo) enriched with the on-disk
// state.json header (issue identifier, provider, work type, phase) and the
// live metrics the tailer accumulates (tool count, last tool, cost). It is
// the unit the FleetGrid renders.
type SessionCard struct {
	SessionID    string
	PID          int
	DaemonState  string // daemon lifecycle: starting/running/...
	WorktreePath string
	ProjectName  string
	Repository   string

	// From state.json (best-effort; zero values when unreadable).
	IssueID         string
	IssueIdentifier string
	Provider        string
	WorkType        string
	CurrentStep     string
	StartedAtUnixMs int64

	// Live metrics accumulated from the events.jsonl tail.
	ToolCalls    int
	LastTool     string
	LastActivity string
	CostUsd      float64
	NumTurns     int
	Errored      bool
}

// EventsPath returns the absolute path to this card's events.jsonl, or ""
// when the worktree path is unknown.
func (c SessionCard) EventsPath() string {
	if c.WorktreePath == "" {
		return ""
	}
	return filepath.Join(c.WorktreePath, state.AgentDirName, "events.jsonl")
}

// GroupKey is the card grid grouping key — the issue identifier when known,
// else the session id. Cards cluster by issue, mirroring the FIG 2.0
// "issue clustering" vibe.
func (c SessionCard) GroupKey() string {
	if c.IssueIdentifier != "" {
		return c.IssueIdentifier
	}
	return c.SessionID
}

// Source is the local-first data layer. It owns no goroutines and performs
// no platform I/O — every method is a synchronous read of the localhost
// daemon API and on-disk state. The Bubble Tea model drives it from tea.Cmd
// closures so the work happens off the render path.
type Source struct {
	daemon daemonLister
	state  stateReader

	// repoScope, when non-empty, filters Snapshot to sessions whose
	// Repository matches (the af-worker-fleet "one tab per project"
	// ergonomic, scoped by the CWD's repo). Empty = every session on the
	// host (the --all overview).
	repoScope string
}

// NewSource constructs a Source. daemon must be non-nil; st defaults to a
// fresh state.Store when nil.
func NewSource(daemon daemonLister, st stateReader, repoScope string) *Source {
	if st == nil {
		st = state.NewStore()
	}
	return &Source{daemon: daemon, state: st, repoScope: strings.TrimSpace(repoScope)}
}

// Counters is the dashboard header summary, all sourced from the daemon's
// local status/stats endpoints (no platform call).
type Counters struct {
	Running       int
	MaxSessions   int
	QueueDepth    int
	UptimeSeconds int64
	Version       string
	Err           error // status/stats fetch error (header degrades gracefully)
}

// Snapshot is the result of one index poll: the scoped session cards plus
// the header counters.
type Snapshot struct {
	Cards    []SessionCard
	Counters Counters
	Err      error // sessions-list fetch error (fatal for the grid this tick)
}

// repoMatch reports whether a session's repository belongs to the scope.
// It mirrors the daemon's matchProject leniency so a Linear project slug
// and a git URL that refer to the same repo both match: exact, or either
// being the "/"-suffix of the other.
func repoMatch(scope, repo string) bool {
	if scope == "" {
		return true
	}
	if repo == "" {
		return false
	}
	return scope == repo ||
		strings.HasSuffix(repo, "/"+scope) ||
		strings.HasSuffix(scope, "/"+repo)
}

// Snapshot performs one index poll and returns the scoped, header-enriched
// view. It reads state.json for each scoped session to populate card
// headers; an unreadable state file is non-fatal (the card still renders
// from the daemon index alone). The returned cards are sorted by group key
// then session id for a stable grid.
func (s *Source) Snapshot() Snapshot {
	handles, err := s.daemon.GetSessions()
	if err != nil {
		return Snapshot{Err: fmt.Errorf("hostwatch: list sessions: %w", err), Counters: s.counters(0)}
	}

	cards := make([]SessionCard, 0, len(handles))
	for _, h := range handles {
		if !repoMatch(s.repoScope, h.Repository) {
			continue
		}
		card := SessionCard{
			SessionID:    h.SessionID,
			PID:          h.PID,
			DaemonState:  h.State,
			WorktreePath: h.WorktreePath,
			ProjectName:  h.ProjectName,
			Repository:   h.Repository,
		}
		s.enrichFromState(&card)
		cards = append(cards, card)
	}

	sort.SliceStable(cards, func(i, j int) bool {
		if cards[i].GroupKey() != cards[j].GroupKey() {
			return cards[i].GroupKey() < cards[j].GroupKey()
		}
		return cards[i].SessionID < cards[j].SessionID
	})

	return Snapshot{Cards: cards, Counters: s.counters(len(cards))}
}

// enrichFromState reads <worktree>/.agent/state.json and folds its header
// fields onto the card. Missing / malformed state is silently tolerated —
// the daemon index alone is enough to render a card.
func (s *Source) enrichFromState(card *SessionCard) {
	if card.WorktreePath == "" {
		return
	}
	st, err := s.state.Read(card.WorktreePath)
	if err != nil {
		// ErrNotFound is the common case before the runner's first write;
		// any read error leaves the card with daemon-only fields.
		_ = errors.Is(err, state.ErrNotFound)
		return
	}
	card.IssueID = st.IssueID
	card.IssueIdentifier = st.IssueIdentifier
	card.Provider = string(st.ProviderName)
	card.WorkType = st.WorkType
	card.CurrentStep = st.CurrentStep
	card.StartedAtUnixMs = st.StartedAt
	if card.PID == 0 && st.PID != 0 {
		card.PID = st.PID
	}
}

// counters reads the daemon status + stats and folds them into the header.
// running is the scoped card count (what the grid shows); the daemon's own
// activeSessions reflects the whole host, which we surface as MaxSessions
// context. Header fetch errors are non-fatal.
func (s *Source) counters(running int) Counters {
	c := Counters{Running: running}
	if st, err := s.daemon.GetStatus(); err == nil && st != nil {
		c.MaxSessions = st.MaxSessions
		c.UptimeSeconds = st.UptimeSeconds
		c.Version = st.Version
		if s.repoScope == "" {
			// Unscoped: trust the daemon's own active count.
			c.Running = st.ActiveSessions
		}
	} else if err != nil {
		c.Err = err
	}
	if stats, err := s.daemon.GetStats(false, false); err == nil && stats != nil {
		c.QueueDepth = stats.QueueDepth
	}
	return c
}
