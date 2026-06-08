package codeintel

// arch_store.go — the SQLite-backed architectural observation graph (Layer 2,
// native Go port). Port of
// donmai-libraries/packages/architectural-intelligence/src/sqlite-impl.ts.
//
// Storage layout (single SQLite file, default .donmai/arch-intelligence/db.sqlite):
//
//	observations  — raw ArchObservation rows (JSON payload)
//	patterns      — ArchitecturalPattern rows
//	conventions   — Convention rows
//	decisions     — Decision rows
//	deviations    — StoredDeviation rows
//	citations     — Citation rows, FK'd to owning nodes
//
// Authored-intent constraint: citations with confidence 'authored' always rank
// above inferred rows (ORDER BY confidence_rank DESC) — the mechanical
// enforcement of 007 §"non-negotiable principles".
//
// SQLite driver: modernc.org/sqlite (pure Go, CGo-free). WAL + foreign_keys
// pragmas are applied at open.

import (
	"crypto/rand"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	// Pure-Go, CGo-free SQLite driver registered under the name "sqlite". The
	// blank import wires it into database/sql; the store never references the
	// package directly.
	_ "modernc.org/sqlite"
)

// DefaultArchDBPath is the default SQLite database file for the arch-intel graph.
const DefaultArchDBPath = ".donmai/arch-intelligence/db.sqlite"

// schemaDDL mirrors SCHEMA_DDL in sqlite-impl.ts (column names and types are
// kept identical so a TS-written DB and a Go-written DB are interchangeable).
const schemaDDL = `
CREATE TABLE IF NOT EXISTS observations (
  id           TEXT PRIMARY KEY,
  kind         TEXT NOT NULL,
  payload      TEXT NOT NULL,
  confidence   REAL NOT NULL,
  scope_level  TEXT NOT NULL,
  scope_json   TEXT NOT NULL,
  source_json  TEXT NOT NULL,
  repo         TEXT,
  created_at   TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS citations (
  id               TEXT PRIMARY KEY,
  owner_kind       TEXT NOT NULL,
  owner_id         TEXT NOT NULL,
  source_json      TEXT NOT NULL,
  confidence       TEXT NOT NULL,
  confidence_rank  INTEGER NOT NULL,
  recorded_at      TEXT NOT NULL,
  excerpt          TEXT
);

CREATE INDEX IF NOT EXISTS idx_citations_owner ON citations(owner_kind, owner_id);

CREATE TABLE IF NOT EXISTS patterns (
  id           TEXT PRIMARY KEY,
  title        TEXT NOT NULL,
  description  TEXT NOT NULL,
  locations    TEXT NOT NULL,
  tags         TEXT NOT NULL,
  scope_json   TEXT NOT NULL,
  repo         TEXT,
  created_at   TEXT NOT NULL,
  updated_at   TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS conventions (
  id           TEXT PRIMARY KEY,
  title        TEXT NOT NULL,
  description  TEXT NOT NULL,
  examples     TEXT NOT NULL,
  authored     INTEGER NOT NULL DEFAULT 0,
  scope_json   TEXT NOT NULL,
  repo         TEXT,
  created_at   TEXT NOT NULL,
  updated_at   TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS decisions (
  id           TEXT PRIMARY KEY,
  title        TEXT NOT NULL,
  chosen       TEXT NOT NULL,
  alternatives TEXT NOT NULL,
  rationale    TEXT NOT NULL,
  status       TEXT NOT NULL DEFAULT 'active',
  supersedes   TEXT,
  scope_json   TEXT NOT NULL,
  repo         TEXT,
  created_at   TEXT NOT NULL,
  updated_at   TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS deviations (
  id               TEXT PRIMARY KEY,
  title            TEXT NOT NULL,
  description      TEXT NOT NULL,
  deviates_from    TEXT NOT NULL,
  introduced_by    TEXT,
  status           TEXT NOT NULL DEFAULT 'pending',
  severity         TEXT NOT NULL DEFAULT 'medium',
  scope_json       TEXT NOT NULL,
  repo             TEXT,
  created_at       TEXT NOT NULL,
  updated_at       TEXT NOT NULL
);
`

// ArchStore is the SQLite-backed observation graph.
type ArchStore struct {
	db   *sql.DB
	path string
}

// OpenArchStore opens (creating if needed) the arch-intel SQLite store at
// dbPath. An empty dbPath defaults to DefaultArchDBPath. The parent directory
// is created (mkdir -p). WAL + foreign_keys pragmas are applied.
func OpenArchStore(dbPath string) (*ArchStore, error) {
	if dbPath == "" {
		dbPath = DefaultArchDBPath
	}

	if dir := filepath.Dir(dbPath); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o750); err != nil {
			return nil, fmt.Errorf("archstore: mkdir %q: %w", dir, err)
		}
	}

	// modernc.org/sqlite accepts PRAGMA via connection-string query params.
	// _pragma=journal_mode(WAL) and _pragma=foreign_keys(1) apply per connection.
	dsn := dbPath + "?_pragma=journal_mode(WAL)&_pragma=foreign_keys(1)&_pragma=busy_timeout(5000)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("archstore: open %q: %w", dbPath, err)
	}

	// Single writer keeps WAL semantics simple and avoids cross-connection
	// pragma drift for a local single-tenant store.
	db.SetMaxOpenConns(1)

	if _, err := db.Exec(schemaDDL); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("archstore: schema: %w", err)
	}

	return &ArchStore{db: db, path: dbPath}, nil
}

// Path returns the on-disk database path.
func (s *ArchStore) Path() string { return s.path }

// Close releases the database connection.
func (s *ArchStore) Close() error {
	if s.db == nil {
		return nil
	}
	return s.db.Close()
}

// Contribute inserts an observation and materializes it into the typed node
// table (pattern/convention/decision/deviation) with an authored-confidence
// ranked citation. Mirrors sqlite-impl.ts contribute().
func (s *ArchStore) Contribute(obs ArchObservation) error {
	id := newUUID()
	now := time.Now().UTC().Format(time.RFC3339Nano)

	payload := obs.Payload
	if len(payload) == 0 {
		payload = json.RawMessage("null")
	}
	scopeJSON, err := json.Marshal(obs.Scope)
	if err != nil {
		return fmt.Errorf("archstore: marshal scope: %w", err)
	}
	sourceJSON, err := json.Marshal(obs.Source)
	if err != nil {
		return fmt.Errorf("archstore: marshal source: %w", err)
	}

	_, err = s.db.Exec(
		`INSERT INTO observations (id, kind, payload, confidence, scope_level, scope_json, source_json, repo, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		id, obs.Kind, string(payload), obs.Confidence, obs.Scope.Level,
		string(scopeJSON), string(sourceJSON), nullableRepo(obs.Scope.Repo), now,
	)
	if err != nil {
		return fmt.Errorf("archstore: insert observation: %w", err)
	}

	// Materialize into the typed node + citation.
	switch obs.Kind {
	case "pattern":
		return s.materializePattern(id, obs, now)
	case "convention":
		return s.materializeConvention(id, obs, now)
	case "decision":
		return s.materializeDecision(id, obs, now)
	case "deviation":
		return s.materializeDeviation(id, obs, now)
	}
	return nil
}

// Query retrieves architectural context for a spec, applying scope-level and
// repo filtering. Mirrors sqlite-impl.ts query().
func (s *ArchStore) Query(spec ArchQuerySpec) (ArchView, error) {
	scopeLevel := spec.Scope.Level
	repos := effectiveRepos(spec)

	// Build the optional repo IN (...) clause.
	repoClause := ""
	args := []any{scopeLevel}
	if len(repos) > 0 {
		ph := make([]string, len(repos))
		for i, r := range repos {
			ph[i] = "?"
			args = append(args, r)
		}
		repoClause = " AND repo IN (" + strings.Join(ph, ", ") + ")"
	}

	patterns, err := s.queryPatterns(repoClause, args, spec.Paths)
	if err != nil {
		return ArchView{}, err
	}
	conventions, err := s.queryConventions(repoClause, args)
	if err != nil {
		return ArchView{}, err
	}
	decisions, err := s.queryDecisions(repoClause, args)
	if err != nil {
		return ArchView{}, err
	}

	// Collect all citations for returned nodes (already attached per node), then
	// flatten + sort authored-first + dedupe by id.
	var allCitations []Citation
	for _, p := range patterns {
		allCitations = append(allCitations, p.Citations...)
	}
	for _, c := range conventions {
		allCitations = append(allCitations, c.Citations...)
	}
	for _, d := range decisions {
		allCitations = append(allCitations, d.Citations...)
	}
	sort.SliceStable(allCitations, func(i, j int) bool {
		return CitationConfidenceRank[allCitations[i].Confidence] > CitationConfidenceRank[allCitations[j].Confidence]
	})

	return ArchView{
		Patterns:    patterns,
		Conventions: conventions,
		Decisions:   decisions,
		Citations:   deduplicateCitations(allCitations),
		Scope:       spec.Scope,
		RetrievedAt: time.Now().UTC(),
	}, nil
}

// ── Query helpers ─────────────────────────────────────────────────────────────

func (s *ArchStore) queryPatterns(repoClause string, baseArgs []any, paths []string) ([]ArchitecturalPattern, error) {
	//nolint:gosec // G202: repoClause is built only from bound '?' placeholders (no user data); all repo values are passed as query parameters.
	q := `SELECT id, title, description, locations, tags, scope_json, created_at, updated_at
	      FROM patterns
	      WHERE JSON_EXTRACT(scope_json, '$.level') = ?` + repoClause + `
	      ORDER BY updated_at DESC`
	// NOTE: the row cursor is fully drained and closed BEFORE loading citations.
	// The pool is capped at one connection (SetMaxOpenConns(1)); issuing the
	// nested loadCitations query while the outer cursor is still open would
	// deadlock waiting for a second connection that can never arrive.
	var out []ArchitecturalPattern
	if err := func() error {
		rows, err := s.db.Query(q, baseArgs...)
		if err != nil {
			return fmt.Errorf("archstore: query patterns: %w", err)
		}
		defer func() { _ = rows.Close() }()
		for rows.Next() {
			var id, title, desc, locs, tags, scopeJSON, createdAt, updatedAt string
			if err := rows.Scan(&id, &title, &desc, &locs, &tags, &scopeJSON, &createdAt, &updatedAt); err != nil {
				return fmt.Errorf("archstore: scan pattern: %w", err)
			}
			p := ArchitecturalPattern{
				ID:          id,
				Title:       title,
				Description: desc,
				Scope:       parseScope(scopeJSON),
				CreatedAt:   parseTime(createdAt),
				UpdatedAt:   parseTime(updatedAt),
			}
			_ = json.Unmarshal([]byte(locs), &p.Locations)
			_ = json.Unmarshal([]byte(tags), &p.Tags)
			// Path narrowing: keep pattern if any location overlaps any spec path.
			if len(paths) > 0 && !patternMatchesPaths(p.Locations, paths) {
				continue
			}
			out = append(out, p)
		}
		return rows.Err()
	}(); err != nil {
		return nil, err
	}

	for i := range out {
		cites, err := s.loadCitations("pattern", out[i].ID)
		if err != nil {
			return nil, err
		}
		out[i].Citations = cites
	}
	return out, nil
}

func (s *ArchStore) queryConventions(repoClause string, baseArgs []any) ([]Convention, error) {
	//nolint:gosec // G202: repoClause is built only from bound '?' placeholders (no user data); all repo values are passed as query parameters.
	q := `SELECT id, title, description, examples, authored, scope_json, created_at, updated_at
	      FROM conventions
	      WHERE JSON_EXTRACT(scope_json, '$.level') = ?` + repoClause + `
	      ORDER BY authored DESC, updated_at DESC`
	// Drain + close before loading citations — see queryPatterns note.
	var out []Convention
	if err := func() error {
		rows, err := s.db.Query(q, baseArgs...)
		if err != nil {
			return fmt.Errorf("archstore: query conventions: %w", err)
		}
		defer func() { _ = rows.Close() }()
		for rows.Next() {
			var (
				id, title, desc, examples, scopeJSON, createdAt, updatedAt string
				authored                                                   int
			)
			if err := rows.Scan(&id, &title, &desc, &examples, &authored, &scopeJSON, &createdAt, &updatedAt); err != nil {
				return fmt.Errorf("archstore: scan convention: %w", err)
			}
			c := Convention{
				ID:          id,
				Title:       title,
				Description: desc,
				Authored:    authored == 1,
				Scope:       parseScope(scopeJSON),
				CreatedAt:   parseTime(createdAt),
				UpdatedAt:   parseTime(updatedAt),
			}
			_ = json.Unmarshal([]byte(examples), &c.Examples)
			out = append(out, c)
		}
		return rows.Err()
	}(); err != nil {
		return nil, err
	}

	for i := range out {
		cites, err := s.loadCitations("convention", out[i].ID)
		if err != nil {
			return nil, err
		}
		out[i].Citations = cites
	}
	return out, nil
}

func (s *ArchStore) queryDecisions(repoClause string, baseArgs []any) ([]Decision, error) {
	// Active-only, matching sqlite-impl.ts. The status='active' clause sits
	// before the repo clause exactly as in the TS query string.
	//nolint:gosec // G202: repoClause is built only from bound '?' placeholders (no user data); all repo values are passed as query parameters.
	q := `SELECT id, title, chosen, alternatives, rationale, status, supersedes, scope_json, created_at, updated_at
	      FROM decisions
	      WHERE JSON_EXTRACT(scope_json, '$.level') = ? AND status = 'active'` + repoClause + `
	      ORDER BY updated_at DESC`
	// Drain + close before loading citations — see queryPatterns note.
	var out []Decision
	if err := func() error {
		rows, err := s.db.Query(q, baseArgs...)
		if err != nil {
			return fmt.Errorf("archstore: query decisions: %w", err)
		}
		defer func() { _ = rows.Close() }()
		for rows.Next() {
			var (
				id, title, chosen, alts, rationale, status, scopeJSON, createdAt, updatedAt string
				supersedes                                                                  sql.NullString
			)
			if err := rows.Scan(&id, &title, &chosen, &alts, &rationale, &status, &supersedes, &scopeJSON, &createdAt, &updatedAt); err != nil {
				return fmt.Errorf("archstore: scan decision: %w", err)
			}
			d := Decision{
				ID:        id,
				Title:     title,
				Chosen:    chosen,
				Rationale: rationale,
				Status:    status,
				Scope:     parseScope(scopeJSON),
				CreatedAt: parseTime(createdAt),
				UpdatedAt: parseTime(updatedAt),
			}
			if supersedes.Valid {
				d.Supersedes = supersedes.String
			}
			_ = json.Unmarshal([]byte(alts), &d.Alternatives)
			out = append(out, d)
		}
		return rows.Err()
	}(); err != nil {
		return nil, err
	}

	for i := range out {
		cites, err := s.loadCitations("decision", out[i].ID)
		if err != nil {
			return nil, err
		}
		out[i].Citations = cites
	}
	return out, nil
}

// QueryDeviations returns deviation nodes for a scope-level (+ optional repo).
// Exposed for the assess-against-baseline path (wired in a later stage) and for
// diagnostics. Mirrors sqlite-impl.ts _getAllDeviations but scope-filtered.
func (s *ArchStore) QueryDeviations(scopeLevel string, repos ...string) ([]StoredDeviation, error) {
	args := []any{scopeLevel}
	repoClause := ""
	var clean []string
	for _, r := range repos {
		if r != "" {
			clean = append(clean, r)
		}
	}
	if len(clean) > 0 {
		ph := make([]string, len(clean))
		for i, r := range clean {
			ph[i] = "?"
			args = append(args, r)
		}
		repoClause = " AND repo IN (" + strings.Join(ph, ", ") + ")"
	}

	//nolint:gosec // G202: repoClause is built only from bound '?' placeholders (no user data); all repo values are passed as query parameters.
	q := `SELECT id, title, description, deviates_from, introduced_by, status, severity, scope_json, created_at, updated_at
	      FROM deviations
	      WHERE JSON_EXTRACT(scope_json, '$.level') = ?` + repoClause + `
	      ORDER BY updated_at DESC`
	// Drain + close before loading citations — see queryPatterns note.
	var out []StoredDeviation
	if err := func() error {
		rows, err := s.db.Query(q, args...)
		if err != nil {
			return fmt.Errorf("archstore: query deviations: %w", err)
		}
		defer func() { _ = rows.Close() }()
		for rows.Next() {
			var (
				id, title, desc, deviatesFrom, status, severity, scopeJSON, createdAt, updatedAt string
				introducedBy                                                                     sql.NullString
			)
			if err := rows.Scan(&id, &title, &desc, &deviatesFrom, &introducedBy, &status, &severity, &scopeJSON, &createdAt, &updatedAt); err != nil {
				return fmt.Errorf("archstore: scan deviation: %w", err)
			}
			dv := StoredDeviation{
				ID:          id,
				Title:       title,
				Description: desc,
				Status:      status,
				Severity:    severity,
				Scope:       parseScope(scopeJSON),
				CreatedAt:   parseTime(createdAt),
				UpdatedAt:   parseTime(updatedAt),
			}
			_ = json.Unmarshal([]byte(deviatesFrom), &dv.DeviatesFrom)
			if introducedBy.Valid && introducedBy.String != "" {
				var cr ArchChangeRef
				if json.Unmarshal([]byte(introducedBy.String), &cr) == nil {
					dv.IntroducedBy = &cr
				}
			}
			out = append(out, dv)
		}
		return rows.Err()
	}(); err != nil {
		return nil, err
	}

	for i := range out {
		cites, err := s.loadCitations("deviation", out[i].ID)
		if err != nil {
			return nil, err
		}
		out[i].Citations = cites
	}
	return out, nil
}

func (s *ArchStore) loadCitations(ownerKind, ownerID string) ([]Citation, error) {
	rows, err := s.db.Query(
		`SELECT id, source_json, confidence, recorded_at, excerpt
		 FROM citations WHERE owner_kind = ? AND owner_id = ? ORDER BY confidence_rank DESC`,
		ownerKind, ownerID,
	)
	if err != nil {
		return nil, fmt.Errorf("archstore: load citations: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []Citation
	for rows.Next() {
		var (
			id, sourceJSON, confidence, recordedAt string
			excerpt                                sql.NullString
		)
		if err := rows.Scan(&id, &sourceJSON, &confidence, &recordedAt, &excerpt); err != nil {
			return nil, fmt.Errorf("archstore: scan citation: %w", err)
		}
		c := Citation{
			ID:         id,
			Confidence: confidence,
			RecordedAt: parseTime(recordedAt),
		}
		_ = json.Unmarshal([]byte(sourceJSON), &c.Source)
		if excerpt.Valid {
			c.Excerpt = excerpt.String
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// ── Materialization helpers ───────────────────────────────────────────────────

// payloadMap unmarshals an observation payload into a generic map for the
// simplified direct-materialization path (mirrors the `payload as Partial<...>`
// casts in sqlite-impl.ts).
func payloadMap(obs ArchObservation) map[string]any {
	m := map[string]any{}
	if len(obs.Payload) > 0 {
		_ = json.Unmarshal(obs.Payload, &m)
	}
	return m
}

func (s *ArchStore) materializePattern(observationID string, obs ArchObservation, now string) error {
	p := payloadMap(obs)
	id := newUUID()
	title := strOr(p["title"], "Untitled pattern")
	desc := strOr(p["description"], "")
	locations := jsonOr(p["locations"], "[]")
	tags := jsonOr(p["tags"], "[]")
	scopeJSON, _ := json.Marshal(obs.Scope)

	if _, err := s.db.Exec(
		`INSERT INTO patterns (id, title, description, locations, tags, scope_json, repo, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		id, title, desc, locations, tags, string(scopeJSON), nullableRepo(obs.Scope.Repo), now, now,
	); err != nil {
		return fmt.Errorf("archstore: insert pattern: %w", err)
	}
	return s.insertCitation("pattern", id, obs, observationID, now)
}

func (s *ArchStore) materializeConvention(observationID string, obs ArchObservation, now string) error {
	p := payloadMap(obs)
	id := newUUID()
	title := strOr(p["title"], "Untitled convention")
	desc := strOr(p["description"], "")
	examples := jsonOr(p["examples"], "[]")
	authored := 0
	if obs.Source.AuthoredDoc != nil {
		authored = 1
	}
	scopeJSON, _ := json.Marshal(obs.Scope)

	if _, err := s.db.Exec(
		`INSERT INTO conventions (id, title, description, examples, authored, scope_json, repo, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		id, title, desc, examples, authored, string(scopeJSON), nullableRepo(obs.Scope.Repo), now, now,
	); err != nil {
		return fmt.Errorf("archstore: insert convention: %w", err)
	}
	return s.insertCitation("convention", id, obs, observationID, now)
}

func (s *ArchStore) materializeDecision(observationID string, obs ArchObservation, now string) error {
	p := payloadMap(obs)
	id := newUUID()
	title := strOr(p["title"], "Untitled decision")
	chosen := strOr(p["chosen"], "")
	alternatives := jsonOr(p["alternatives"], "[]")
	rationale := strOr(p["rationale"], "")
	status := strOr(p["status"], "active")
	scopeJSON, _ := json.Marshal(obs.Scope)

	if _, err := s.db.Exec(
		`INSERT INTO decisions (id, title, chosen, alternatives, rationale, status, scope_json, repo, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		id, title, chosen, alternatives, rationale, status, string(scopeJSON), nullableRepo(obs.Scope.Repo), now, now,
	); err != nil {
		return fmt.Errorf("archstore: insert decision: %w", err)
	}
	return s.insertCitation("decision", id, obs, observationID, now)
}

func (s *ArchStore) materializeDeviation(observationID string, obs ArchObservation, now string) error {
	p := payloadMap(obs)
	id := newUUID()
	title := strOr(p["title"], "Untitled deviation")
	desc := strOr(p["description"], "")
	deviatesFrom := jsonOr(p["deviatesFrom"], `{"kind":"pattern","patternId":"unknown"}`)
	var introducedBy any
	if obs.Source.ChangeRef != nil {
		b, _ := json.Marshal(obs.Source.ChangeRef)
		introducedBy = string(b)
	}
	status := strOr(p["status"], "pending")
	severity := strOr(p["severity"], "medium")
	scopeJSON, _ := json.Marshal(obs.Scope)

	if _, err := s.db.Exec(
		`INSERT INTO deviations (id, title, description, deviates_from, introduced_by, status, severity, scope_json, repo, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		id, title, desc, deviatesFrom, introducedBy, status, severity, string(scopeJSON), nullableRepo(obs.Scope.Repo), now, now,
	); err != nil {
		return fmt.Errorf("archstore: insert deviation: %w", err)
	}
	return s.insertCitation("deviation", id, obs, observationID, now)
}

// insertCitation writes a citation for a freshly materialized node. The
// confidence level enforces the authored-intent constraint via
// observationConfidenceToLevel. Mirrors sqlite-impl.ts _insertCitation.
func (s *ArchStore) insertCitation(ownerKind, ownerID string, obs ArchObservation, observationID, now string) error {
	confidence := observationConfidenceToLevel(obs)
	citationID := newUUID()

	var src CitationSource
	switch {
	case obs.Source.AuthoredDoc != nil:
		src = CitationSource{Kind: "file", Path: obs.Source.AuthoredDoc.Path}
	case obs.Source.ChangeRef != nil:
		src = CitationSource{Kind: "change", ChangeRef: obs.Source.ChangeRef}
	case obs.Source.SessionID != "":
		src = CitationSource{Kind: "session", SessionID: obs.Source.SessionID}
	default:
		src = CitationSource{Kind: "session", SessionID: observationID}
	}
	sourceJSON, err := json.Marshal(src)
	if err != nil {
		return fmt.Errorf("archstore: marshal citation source: %w", err)
	}

	if _, err := s.db.Exec(
		`INSERT INTO citations (id, owner_kind, owner_id, source_json, confidence, confidence_rank, recorded_at, excerpt)
		 VALUES (?, ?, ?, ?, ?, ?, ?, NULL)`,
		citationID, ownerKind, ownerID, string(sourceJSON), confidence, CitationConfidenceRank[confidence], now,
	); err != nil {
		return fmt.Errorf("archstore: insert citation: %w", err)
	}
	return nil
}

// ── Small helpers ─────────────────────────────────────────────────────────────

// deduplicateCitations drops citations sharing an id, preserving first-seen
// order. Mirrors sqlite-impl.ts _deduplicateCitations.
func deduplicateCitations(cites []Citation) []Citation {
	seen := make(map[string]struct{})
	out := make([]Citation, 0, len(cites))
	for _, c := range cites {
		if _, ok := seen[c.ID]; ok {
			continue
		}
		seen[c.ID] = struct{}{}
		out = append(out, c)
	}
	return out
}

// patternMatchesPaths reports whether any of the pattern's locations overlaps
// any of the query paths (substring either direction), matching the TS
// `l.path.includes(p) || p.includes(l.path)` narrowing.
func patternMatchesPaths(locs []PatternLocation, paths []string) bool {
	for _, l := range locs {
		for _, p := range paths {
			if strings.Contains(l.Path, p) || strings.Contains(p, l.Path) {
				return true
			}
		}
	}
	return false
}

// parseScope decodes a scope_json column; a parse failure yields a zero scope.
func parseScope(s string) ArchScope {
	var sc ArchScope
	_ = json.Unmarshal([]byte(s), &sc)
	return sc
}

// parseTime parses an RFC3339(/Nano) timestamp column; a parse failure yields
// the zero time.
func parseTime(s string) time.Time {
	if t, err := time.Parse(time.RFC3339Nano, s); err == nil {
		return t
	}
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t
	}
	return time.Time{}
}

// strOr returns v as a string, or fallback when v is nil/non-string/empty
// matches the TS `String(payload['x'] ?? 'default')` for the missing case.
func strOr(v any, fallback string) string {
	if s, ok := v.(string); ok {
		return s
	}
	if v == nil {
		return fallback
	}
	// Non-string present (number/bool): stringify defensively.
	return fmt.Sprintf("%v", v)
}

// jsonOr re-marshals v to JSON, or returns fallback when v is nil. Mirrors the
// TS `JSON.stringify(payload['x'] ?? [])` materialization.
func jsonOr(v any, fallback string) string {
	if v == nil {
		return fallback
	}
	b, err := json.Marshal(v)
	if err != nil {
		return fallback
	}
	return string(b)
}

// nullableRepo returns the repo string or nil so an empty repo persists as SQL
// NULL (matching `observation.scope.repo ?? null` in sqlite-impl.ts).
func nullableRepo(repo string) any {
	if repo == "" {
		return nil
	}
	return repo
}

// newUUID returns a random RFC 4122 v4 UUID string. Uses crypto/rand to avoid a
// new direct dependency (AGENTS.md: no new direct deps without justification).
func newUUID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		// crypto/rand should never fail; fall back to a timestamp-derived id so
		// we never panic (AGENTS.md: never panic).
		return fmt.Sprintf("00000000-0000-4000-8000-%012x", time.Now().UnixNano())
	}
	b[6] = (b[6] & 0x0f) | 0x40 // version 4
	b[8] = (b[8] & 0x3f) | 0x80 // variant 10
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}
