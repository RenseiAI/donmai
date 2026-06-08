package codeintel

import (
	"encoding/json"
	"path/filepath"
	"testing"
)

// openTestStore opens a store in a per-test temp directory and registers cleanup.
func openTestStore(t *testing.T) *ArchStore {
	t.Helper()
	dir := t.TempDir()
	s, err := OpenArchStore(filepath.Join(dir, "arch", "db.sqlite"))
	if err != nil {
		t.Fatalf("OpenArchStore: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func mustPayload(t *testing.T, v any) json.RawMessage {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}
	return b
}

func TestOpenArchStore_CreatesDirAndSchema(t *testing.T) {
	s := openTestStore(t)
	if s.Path() == "" {
		t.Fatal("expected non-empty path")
	}
	// A query against an empty store must succeed and return empty slices.
	view, err := s.Query(ArchQuerySpec{WorkType: "research", Scope: ArchScope{Level: "project"}})
	if err != nil {
		t.Fatalf("Query empty store: %v", err)
	}
	if len(view.Patterns) != 0 || len(view.Conventions) != 0 || len(view.Decisions) != 0 {
		t.Errorf("expected empty view, got %+v", view)
	}
}

func TestOpenArchStore_DefaultPath(t *testing.T) {
	// Calling with "" should choose DefaultArchDBPath. We override the path by
	// chdir into a temp dir so we never touch the real .donmai.
	dir := t.TempDir()
	t.Chdir(dir)
	s, err := OpenArchStore("")
	if err != nil {
		t.Fatalf("OpenArchStore default: %v", err)
	}
	defer func() { _ = s.Close() }()
	if s.Path() != DefaultArchDBPath {
		t.Errorf("path = %q, want %q", s.Path(), DefaultArchDBPath)
	}
}

func TestContributeAndQueryPattern(t *testing.T) {
	s := openTestStore(t)

	obs := ArchObservation{
		Kind: "pattern",
		Payload: mustPayload(t, map[string]any{
			"title":       "Auth middleware",
			"description": "centralized auth",
			"locations":   []map[string]string{{"path": "src/auth/middleware.ts"}},
			"tags":        []string{"auth"},
		}),
		Confidence: 0.8,
		Scope:      ArchScope{Level: "project"},
		Source:     ObservationSource{SessionID: "sess-1"},
	}
	if err := s.Contribute(obs); err != nil {
		t.Fatalf("Contribute: %v", err)
	}

	view, err := s.Query(ArchQuerySpec{WorkType: "development", Scope: ArchScope{Level: "project"}})
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if len(view.Patterns) != 1 {
		t.Fatalf("got %d patterns, want 1", len(view.Patterns))
	}
	p := view.Patterns[0]
	if p.Title != "Auth middleware" {
		t.Errorf("title = %q", p.Title)
	}
	if len(p.Locations) != 1 || p.Locations[0].Path != "src/auth/middleware.ts" {
		t.Errorf("locations = %+v", p.Locations)
	}
	if len(p.Tags) != 1 || p.Tags[0] != "auth" {
		t.Errorf("tags = %+v", p.Tags)
	}
	// 0.8 confidence, non-authored → inferred-high citation.
	if len(p.Citations) != 1 {
		t.Fatalf("got %d citations, want 1", len(p.Citations))
	}
	if p.Citations[0].Confidence != ConfidenceInferredHigh {
		t.Errorf("citation confidence = %q, want %q", p.Citations[0].Confidence, ConfidenceInferredHigh)
	}
	if p.Citations[0].Source.Kind != "session" || p.Citations[0].Source.SessionID != "sess-1" {
		t.Errorf("citation source = %+v", p.Citations[0].Source)
	}
}

func TestContribute_AuthoredCitation(t *testing.T) {
	s := openTestStore(t)

	obs := ArchObservation{
		Kind:    "convention",
		Payload: mustPayload(t, map[string]any{"title": "Result type", "description": "use Result<T,E>"}),
		// >= 0.9 + authoredDoc → 'authored'
		Confidence: 0.95,
		Scope:      ArchScope{Level: "project"},
		Source:     ObservationSource{AuthoredDoc: &AuthoredDoc{Path: "CLAUDE.md", Kind: "claude-md"}},
	}
	if err := s.Contribute(obs); err != nil {
		t.Fatalf("Contribute: %v", err)
	}

	view, err := s.Query(ArchQuerySpec{WorkType: "research", Scope: ArchScope{Level: "project"}})
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if len(view.Conventions) != 1 {
		t.Fatalf("got %d conventions, want 1", len(view.Conventions))
	}
	c := view.Conventions[0]
	if !c.Authored {
		t.Errorf("expected authored convention")
	}
	if len(c.Citations) != 1 || c.Citations[0].Confidence != ConfidenceAuthored {
		t.Fatalf("citation = %+v, want authored", c.Citations)
	}
	if c.Citations[0].Source.Kind != "file" || c.Citations[0].Source.Path != "CLAUDE.md" {
		t.Errorf("citation source = %+v", c.Citations[0].Source)
	}
}

func TestQuery_AuthoredRanksFirst(t *testing.T) {
	s := openTestStore(t)

	// One inferred + one authored convention. After flattening, the view's
	// citations must be sorted authored-first.
	inferred := ArchObservation{
		Kind:       "convention",
		Payload:    mustPayload(t, map[string]any{"title": "Async/await", "description": "use async"}),
		Confidence: 0.5,
		Scope:      ArchScope{Level: "project"},
		Source:     ObservationSource{SessionID: "s2"},
	}
	authored := ArchObservation{
		Kind:       "convention",
		Payload:    mustPayload(t, map[string]any{"title": "Result type", "description": "use Result"}),
		Confidence: 0.95,
		Scope:      ArchScope{Level: "project"},
		Source:     ObservationSource{AuthoredDoc: &AuthoredDoc{Path: "CLAUDE.md", Kind: "claude-md"}},
	}
	if err := s.Contribute(inferred); err != nil {
		t.Fatalf("Contribute inferred: %v", err)
	}
	if err := s.Contribute(authored); err != nil {
		t.Fatalf("Contribute authored: %v", err)
	}

	view, err := s.Query(ArchQuerySpec{WorkType: "research", Scope: ArchScope{Level: "project"}})
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if len(view.Citations) < 2 {
		t.Fatalf("got %d citations, want >= 2", len(view.Citations))
	}
	if view.Citations[0].Confidence != ConfidenceAuthored {
		t.Errorf("first citation confidence = %q, want authored", view.Citations[0].Confidence)
	}
	// authored convention sorts first in the conventions slice too (authored DESC).
	if !view.Conventions[0].Authored {
		t.Errorf("expected authored convention first, got %+v", view.Conventions[0])
	}
}

func TestQuery_ScopeLevelFilter(t *testing.T) {
	s := openTestStore(t)

	projObs := ArchObservation{
		Kind:       "pattern",
		Payload:    mustPayload(t, map[string]any{"title": "Project pattern"}),
		Confidence: 0.6,
		Scope:      ArchScope{Level: "project"},
		Source:     ObservationSource{SessionID: "s"},
	}
	orgObs := ArchObservation{
		Kind:       "pattern",
		Payload:    mustPayload(t, map[string]any{"title": "Org pattern"}),
		Confidence: 0.6,
		Scope:      ArchScope{Level: "org"},
		Source:     ObservationSource{SessionID: "s"},
	}
	if err := s.Contribute(projObs); err != nil {
		t.Fatal(err)
	}
	if err := s.Contribute(orgObs); err != nil {
		t.Fatal(err)
	}

	proj, err := s.Query(ArchQuerySpec{WorkType: "development", Scope: ArchScope{Level: "project"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(proj.Patterns) != 1 || proj.Patterns[0].Title != "Project pattern" {
		t.Errorf("project query returned %+v", proj.Patterns)
	}

	org, err := s.Query(ArchQuerySpec{WorkType: "development", Scope: ArchScope{Level: "org"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(org.Patterns) != 1 || org.Patterns[0].Title != "Org pattern" {
		t.Errorf("org query returned %+v", org.Patterns)
	}
}

func TestQuery_RepoFilter(t *testing.T) {
	s := openTestStore(t)

	for _, repo := range []string{"github.com/acme/api", "github.com/acme/web"} {
		obs := ArchObservation{
			Kind:       "pattern",
			Payload:    mustPayload(t, map[string]any{"title": "Pattern for " + repo}),
			Confidence: 0.6,
			Scope:      ArchScope{Level: "project", Repo: repo},
			Source:     ObservationSource{SessionID: "s"},
		}
		if err := s.Contribute(obs); err != nil {
			t.Fatal(err)
		}
	}
	// One repo-untagged row too.
	if err := s.Contribute(ArchObservation{
		Kind:       "pattern",
		Payload:    mustPayload(t, map[string]any{"title": "Untagged"}),
		Confidence: 0.6,
		Scope:      ArchScope{Level: "project"},
		Source:     ObservationSource{SessionID: "s"},
	}); err != nil {
		t.Fatal(err)
	}

	// No repo filter → all three (whole-project corpus, backward compatible).
	all, err := s.Query(ArchQuerySpec{WorkType: "development", Scope: ArchScope{Level: "project"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(all.Patterns) != 3 {
		t.Errorf("no-filter query got %d patterns, want 3", len(all.Patterns))
	}

	// scope.repo shorthand → only the api repo.
	api, err := s.Query(ArchQuerySpec{
		WorkType: "development",
		Scope:    ArchScope{Level: "project", Repo: "github.com/acme/api"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(api.Patterns) != 1 || api.Patterns[0].Title != "Pattern for github.com/acme/api" {
		t.Errorf("repo-filtered query got %+v", api.Patterns)
	}

	// repos list (union) → api + web, excluding untagged.
	union, err := s.Query(ArchQuerySpec{
		WorkType: "development",
		Scope:    ArchScope{Level: "project"},
		Repos:    []string{"github.com/acme/api", "github.com/acme/web"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(union.Patterns) != 2 {
		t.Errorf("repos-union query got %d patterns, want 2", len(union.Patterns))
	}
}

func TestQuery_PathNarrowing(t *testing.T) {
	s := openTestStore(t)

	if err := s.Contribute(ArchObservation{
		Kind: "pattern",
		Payload: mustPayload(t, map[string]any{
			"title":     "Auth pattern",
			"locations": []map[string]string{{"path": "src/auth/middleware.ts"}},
		}),
		Confidence: 0.6,
		Scope:      ArchScope{Level: "project"},
		Source:     ObservationSource{SessionID: "s"},
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.Contribute(ArchObservation{
		Kind: "pattern",
		Payload: mustPayload(t, map[string]any{
			"title":     "DB pattern",
			"locations": []map[string]string{{"path": "src/db/schema.ts"}},
		}),
		Confidence: 0.6,
		Scope:      ArchScope{Level: "project"},
		Source:     ObservationSource{SessionID: "s"},
	}); err != nil {
		t.Fatal(err)
	}

	view, err := s.Query(ArchQuerySpec{
		WorkType: "development",
		Scope:    ArchScope{Level: "project"},
		Paths:    []string{"src/auth"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(view.Patterns) != 1 || view.Patterns[0].Title != "Auth pattern" {
		t.Errorf("path-narrowed query got %+v", view.Patterns)
	}
}

func TestQuery_DecisionsActiveOnly(t *testing.T) {
	s := openTestStore(t)

	active := ArchObservation{
		Kind: "decision",
		Payload: mustPayload(t, map[string]any{
			"title": "Use Drizzle", "chosen": "drizzle", "rationale": "edge support", "status": "active",
		}),
		Confidence: 0.7,
		Scope:      ArchScope{Level: "project"},
		Source:     ObservationSource{SessionID: "s"},
	}
	superseded := ArchObservation{
		Kind: "decision",
		Payload: mustPayload(t, map[string]any{
			"title": "Use Prisma", "chosen": "prisma", "rationale": "old", "status": "superseded",
		}),
		Confidence: 0.7,
		Scope:      ArchScope{Level: "project"},
		Source:     ObservationSource{SessionID: "s"},
	}
	if err := s.Contribute(active); err != nil {
		t.Fatal(err)
	}
	if err := s.Contribute(superseded); err != nil {
		t.Fatal(err)
	}

	view, err := s.Query(ArchQuerySpec{WorkType: "research", Scope: ArchScope{Level: "project"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(view.Decisions) != 1 || view.Decisions[0].Chosen != "drizzle" {
		t.Errorf("active-only query got %+v", view.Decisions)
	}
}

func TestContributeAndQueryDeviation(t *testing.T) {
	s := openTestStore(t)

	cr := ArchChangeRef{Repository: "github.com/acme/api", Kind: "pr", PrNumber: 42, Description: "add feature"}
	obs := ArchObservation{
		Kind: "deviation",
		Payload: mustPayload(t, map[string]any{
			"title":        "Inline auth",
			"description":  "auth implemented inline rather than via middleware",
			"deviatesFrom": map[string]any{"kind": "pattern", "patternId": "auth-mw"},
			"status":       "pending",
			"severity":     "high",
		}),
		Confidence: 0.8,
		Scope:      ArchScope{Level: "project"},
		Source:     ObservationSource{ChangeRef: &cr},
	}
	if err := s.Contribute(obs); err != nil {
		t.Fatalf("Contribute deviation: %v", err)
	}

	devs, err := s.QueryDeviations("project")
	if err != nil {
		t.Fatalf("QueryDeviations: %v", err)
	}
	if len(devs) != 1 {
		t.Fatalf("got %d deviations, want 1", len(devs))
	}
	d := devs[0]
	if d.Title != "Inline auth" || d.Severity != "high" || d.Status != "pending" {
		t.Errorf("deviation = %+v", d)
	}
	if d.DeviatesFrom.Kind != "pattern" || d.DeviatesFrom.PatternID != "auth-mw" {
		t.Errorf("deviatesFrom = %+v", d.DeviatesFrom)
	}
	if d.IntroducedBy == nil || d.IntroducedBy.PrNumber != 42 {
		t.Errorf("introducedBy = %+v", d.IntroducedBy)
	}
	// Deviation citation derives from the changeRef source.
	if len(d.Citations) != 1 || d.Citations[0].Source.Kind != "change" {
		t.Errorf("citation = %+v", d.Citations)
	}
}

func TestContribute_DefaultsMissingPayloadFields(t *testing.T) {
	s := openTestStore(t)

	// Empty payload → materialization falls back to "Untitled ..." defaults.
	obs := ArchObservation{
		Kind:       "pattern",
		Payload:    json.RawMessage("{}"),
		Confidence: 0.3,
		Scope:      ArchScope{Level: "project"},
		Source:     ObservationSource{},
	}
	if err := s.Contribute(obs); err != nil {
		t.Fatalf("Contribute: %v", err)
	}
	view, err := s.Query(ArchQuerySpec{WorkType: "development", Scope: ArchScope{Level: "project"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(view.Patterns) != 1 || view.Patterns[0].Title != "Untitled pattern" {
		t.Errorf("defaulted pattern = %+v", view.Patterns)
	}
	// confidence 0.3, no authored doc → inferred-low; source falls back to the
	// observation id session kind.
	if view.Patterns[0].Citations[0].Confidence != ConfidenceInferredLow {
		t.Errorf("citation confidence = %q, want inferred-low", view.Patterns[0].Citations[0].Confidence)
	}
}

func TestObservationConfidenceToLevel(t *testing.T) {
	authoredDoc := &AuthoredDoc{Path: "CLAUDE.md", Kind: "claude-md"}
	tests := []struct {
		name     string
		conf     float64
		authored bool
		want     string
	}{
		{"authored high", 0.95, true, ConfidenceAuthored},
		{"authored but low conf is not authored", 0.85, true, ConfidenceInferredHigh},
		{"inferred high", 0.7, false, ConfidenceInferredHigh},
		{"inferred medium", 0.5, false, ConfidenceInferredMedium},
		{"inferred low", 0.2, false, ConfidenceInferredLow},
		{"high conf no authoredDoc stays inferred", 0.99, false, ConfidenceInferredHigh},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			obs := ArchObservation{Confidence: tc.conf}
			if tc.authored {
				obs.Source.AuthoredDoc = authoredDoc
			}
			if got := observationConfidenceToLevel(obs); got != tc.want {
				t.Errorf("level = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestEffectiveRepos(t *testing.T) {
	tests := []struct {
		name string
		spec ArchQuerySpec
		want []string
	}{
		{"none", ArchQuerySpec{Scope: ArchScope{Level: "project"}}, nil},
		{
			"scope.repo only",
			ArchQuerySpec{Scope: ArchScope{Level: "project", Repo: "a"}},
			[]string{"a"},
		},
		{
			"repos only",
			ArchQuerySpec{Scope: ArchScope{Level: "project"}, Repos: []string{"a", "b"}},
			[]string{"a", "b"},
		},
		{
			"union dedupes",
			ArchQuerySpec{Scope: ArchScope{Level: "project", Repo: "a"}, Repos: []string{"a", "b", ""}},
			[]string{"a", "b"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := effectiveRepos(tc.spec)
			if len(got) != len(tc.want) {
				t.Fatalf("effectiveRepos = %v, want %v", got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Errorf("effectiveRepos[%d] = %q, want %q", i, got[i], tc.want[i])
				}
			}
		})
	}
}

func TestNewUUID_Format(t *testing.T) {
	seen := make(map[string]struct{})
	for i := 0; i < 100; i++ {
		id := newUUID()
		if len(id) != 36 {
			t.Fatalf("uuid %q len = %d, want 36", id, len(id))
		}
		// Version 4 nibble + RFC4122 variant.
		if id[14] != '4' {
			t.Errorf("uuid %q version nibble = %c, want 4", id, id[14])
		}
		if _, dup := seen[id]; dup {
			t.Fatalf("duplicate uuid generated: %q", id)
		}
		seen[id] = struct{}{}
	}
}
