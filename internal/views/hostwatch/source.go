package hostwatch

import (
	"errors"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"time"

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
// the unit the card grid renders.
type SessionCard struct {
	AcceptedAt          string
	AgentCardID         string
	AgentCardName       string
	ActualModel         string
	ActualModelProvider string
	ActualModelVersion  string
	SessionID           string
	PID                 int
	DaemonState         string // daemon lifecycle: starting/running/...
	WorktreePath        string
	ProjectName         string
	Repository          string

	// From state.json (best-effort; zero values when unreadable).
	IssueID          string
	IssueIdentifier  string
	IssueTitle       string
	Provider         string
	Harness          string
	Model            string
	ModelProvider    string // configured endpoint surface, not model author
	ModelAuthor      string
	EndpointOperator string
	Protocol         string
	WorkType         string
	CurrentStep      string
	StartedAtUnixMs  int64
	// LastHeartbeatUnixMs is the most recent session heartbeat the runner
	// observed (state.json snapshot, unix-ms). Zero when not reported.
	LastHeartbeatUnixMs int64
	EventLogStartOffset *int64

	// Live metrics accumulated from the events.jsonl tail.
	ToolCalls    int
	LastTool     string
	LastActivity string
	CostUsd      float64
	NumTurns     int
	Errored      bool
	// Observed reports whether any tail event (live or replayed history)
	// has been folded into this card. Counts render as "not reported"
	// until Observed; a zero is then a measured zero, not a guess.
	Observed bool
	// CostReported and TurnsReported are independent native-observation
	// availability bits. MetricsReported is their compatibility union.
	MetricsReported     bool
	CostReported        bool
	TurnsReported       bool
	seenCallSpanIDs     map[string]struct{}
	seenTerminalOffsets map[int64]struct{}
	// Freshness timestamps. Only live (non-replay) events advance them:
	// replaying history must not make an idle session look active now.
	// LastWorkAt is the last meaningful-work event (tool invocation or
	// model call with usage); LastOutputAt the last output observation
	// (assistant text, tool result, progress tick, system notice).
	LastWorkAt   time.Time
	LastOutputAt time.Time
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

// Counters is the dashboard header summary. Sessions is the visible row count;
// status and stats are sourced from the daemon's local endpoints.
type Counters struct {
	Sessions int
	Running  int // visible running/starting rows, excluding held and terminal rows
	// HostActive and MaxSessions are the daemon's host-wide occupied and
	// total session slots, whatever the dashboard's scope.
	HostActive    int
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
		return Snapshot{Err: fmt.Errorf("hostwatch: list sessions: %w", err), Counters: s.counters(0, 0)}
	}

	cards := make([]SessionCard, 0, len(handles))
	running := 0
	for _, h := range handles {
		if !repoMatch(s.repoScope, h.Repository) {
			continue
		}
		card := SessionCard{
			SessionID:     h.SessionID,
			AcceptedAt:    h.AcceptedAt,
			AgentCardID:   h.AgentCardID,
			AgentCardName: h.AgentCardName,
			PID:           h.PID,
			DaemonState:   h.State,
			WorktreePath:  h.WorktreePath,
			ProjectName:   h.ProjectName,
			Repository:    h.Repository,
			// The daemon index is the owning seam for display metadata:
			// the handle already carries the admitted identity, so a
			// local reader needs no per-card fetch. state.json only
			// backfills what an older daemon omits.
			Harness:          h.Harness,
			Model:            h.Model,
			ModelProvider:    h.ModelProvider,
			ModelAuthor:      h.ModelAuthor,
			EndpointOperator: h.EndpointOperator,
			Protocol:         h.Protocol,
			WorkType:         h.WorkType,
			IssueIdentifier:  h.IssueIdentifier,
		}
		if card.isHeld() {
			// A held row has no live process or workarea, even if a malformed
			// older projection accidentally supplies those fields.
			card.PID = 0
			card.WorktreePath = ""
		} else {
			s.enrichFromState(&card)
		}
		cards = append(cards, card)
		if isLiveState(card.DaemonState) {
			running++
		}
	}

	sort.SliceStable(cards, func(i, j int) bool {
		if cards[i].GroupKey() != cards[j].GroupKey() {
			return cards[i].GroupKey() < cards[j].GroupKey()
		}
		return cards[i].SessionID < cards[j].SessionID
	})

	return Snapshot{Cards: cards, Counters: s.counters(len(cards), running)}
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
	if st.SessionID != "" && st.SessionID != card.SessionID {
		return // a reused path still contains another session's state
	}
	card.IssueID = st.IssueID
	if card.IssueIdentifier == "" {
		card.IssueIdentifier = st.IssueIdentifier
	}
	card.IssueTitle = st.IssueTitle
	card.Provider = string(st.ProviderName)
	// Card name and ID belong to one annotation. Do not mix an index ID
	// with an unrelated stale state name (or the converse).
	if card.AgentCardID == "" && card.AgentCardName == "" {
		card.AgentCardID = st.AgentCardID
		card.AgentCardName = st.AgentCardName
	}
	// state.json backfills only what the daemon index did not already
	// supply, so an older daemon's local state still renders while a new
	// daemon's admitted identity always wins.
	if card.Harness == "" {
		card.Harness = st.Harness
	}
	if card.Model == "" {
		card.Model = st.Model
	}
	// Older state writers populated ModelProvider from the harness rather
	// than an admitted endpoint declaration. Only the daemon's SessionSpec
	// company projection is a reliable configured surface for this card.
	if card.ModelAuthor == "" && st.SessionID == card.SessionID {
		card.ModelAuthor = st.ModelAuthor
	}
	if card.EndpointOperator == "" && st.SessionID == card.SessionID {
		card.EndpointOperator = st.EndpointOperator
	}
	if card.Protocol == "" && st.SessionID == card.SessionID {
		card.Protocol = st.Protocol
	}
	if card.WorkType == "" {
		card.WorkType = st.WorkType
	}
	card.CurrentStep = st.CurrentStep
	card.StartedAtUnixMs = st.StartedAt
	card.LastHeartbeatUnixMs = st.LastHeartbeat
	if st.SessionID == card.SessionID && st.StartedAt > 0 && st.EventLogStartOffset != nil {
		offset := *st.EventLogStartOffset
		card.EventLogStartOffset = &offset
	}
	if card.PID == 0 && st.PID != 0 {
		card.PID = st.PID
	}
}

// counters reads daemon status + stats and records the visible session count.
// Header fetch errors are non-fatal. The daemon's ActiveSessions counter is
// intentionally not used here because it counts running sessions only while
// the grid also includes held sessions.
func (s *Source) counters(sessions, running int) Counters {
	c := Counters{Sessions: sessions, Running: running}
	if st, err := s.daemon.GetStatus(); err == nil && st != nil {
		c.HostActive = st.ActiveSessions
		c.MaxSessions = st.MaxSessions
		c.UptimeSeconds = st.UptimeSeconds
		c.Version = st.Version
	} else if err != nil {
		c.Err = err
	}
	if stats, err := s.daemon.GetStats(false, false); err == nil && stats != nil {
		c.QueueDepth = stats.QueueDepth
	}
	return c
}
