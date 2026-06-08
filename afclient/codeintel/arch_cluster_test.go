package codeintel

import (
	"encoding/json"
	"math"
	"testing"
	"time"
)

// mkObs builds an ArchObservation with a title/description payload and an
// optional authored-doc flag. Used across the cluster tests.
func mkObs(t *testing.T, kind, title, desc string, conf float64, authored bool) ArchObservation {
	t.Helper()
	payload, err := json.Marshal(map[string]any{"title": title, "description": desc})
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}
	obs := ArchObservation{
		Kind:       kind,
		Payload:    payload,
		Confidence: conf,
		Scope:      ArchScope{Level: "project"},
	}
	if authored {
		obs.Source.AuthoredDoc = &AuthoredDoc{Path: "CLAUDE.md", Kind: "claude-md"}
	}
	return obs
}

func TestJaccardSimilarity(t *testing.T) {
	tests := []struct {
		name string
		a, b []string
		want float64
	}{
		{"both empty", nil, nil, 0},
		{"one empty", []string{"foo", "bar"}, nil, 0},
		{"identical", []string{"foo", "bar"}, []string{"foo", "bar"}, 1.0},
		{"half overlap", []string{"foo", "bar"}, []string{"foo", "baz"}, 1.0 / 3.0},
		{"disjoint", []string{"foo", "bar"}, []string{"baz", "qux"}, 0},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			a := toSet(tc.a)
			b := toSet(tc.b)
			got := jaccardSimilarity(a, b)
			if math.Abs(got-tc.want) > 1e-9 {
				t.Fatalf("jaccardSimilarity = %v, want %v", got, tc.want)
			}
		})
	}
}

func toSet(items []string) map[string]struct{} {
	s := make(map[string]struct{})
	for _, i := range items {
		s[i] = struct{}{}
	}
	return s
}

func TestArchTokenize(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want []string // tokens that MUST be present
		omit []string // tokens that must NOT be present
	}{
		{
			name: "lowercases and splits",
			in:   "Auth Middleware Layer",
			want: []string{"auth", "middleware", "layer"},
		},
		{
			name: "drops stop words and short tokens",
			in:   "the a is auth in db",
			want: []string{"auth", "db"},
			omit: []string{"the", "a", "is", "in"},
		},
		{
			name: "splits on punctuation",
			in:   "api/routes,handlers",
			want: []string{"api", "routes", "handlers"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := archTokenize(tc.in)
			for _, w := range tc.want {
				if _, ok := got[w]; !ok {
					t.Errorf("expected token %q present, set=%v", w, got)
				}
			}
			for _, o := range tc.omit {
				if _, ok := got[o]; ok {
					t.Errorf("expected token %q absent, set=%v", o, got)
				}
			}
		})
	}
}

func TestClusterObservations_AuthoredBypass(t *testing.T) {
	now := time.Now()
	// An authored observation, low numeric confidence, but very old: must still
	// emit at confidence 1.0 and never be decayed (authored bypass).
	obs := mkObs(t, "convention", "Result type", "use Result<T,E>", 0.3, true)
	in := []ObservationWithTimestamp{
		{Observation: obs, RecordedAt: now.Add(-365 * 24 * time.Hour)},
	}

	got := ClusterObservations(in, now, ClusterConfig{})
	if len(got) != 1 {
		t.Fatalf("got %d clusters, want 1", len(got))
	}
	if got[0].Representative.Confidence != 1.0 {
		t.Errorf("authored confidence = %v, want 1.0", got[0].Representative.Confidence)
	}
	if got[0].Decayed {
		t.Errorf("authored observation must not be decayed")
	}
	if got[0].ClusterSize != 1 {
		t.Errorf("authored clusterSize = %d, want 1", got[0].ClusterSize)
	}
}

func TestClusterObservations_MergeSimilar(t *testing.T) {
	now := time.Now()
	// Two near-identical inferred observations should merge into one cluster, and
	// the representative (higher confidence) gets a small merge boost.
	a := mkObs(t, "pattern", "Auth middleware pattern", "centralized auth middleware", 0.5, false)
	b := mkObs(t, "pattern", "Auth middleware pattern", "centralized auth middleware layer", 0.4, false)
	in := []ObservationWithTimestamp{
		{Observation: a, RecordedAt: now},
		{Observation: b, RecordedAt: now},
	}

	got := ClusterObservations(in, now, ClusterConfig{})
	if len(got) != 1 {
		t.Fatalf("got %d clusters, want 1 (should merge)", len(got))
	}
	r := got[0]
	if r.ClusterSize != 2 {
		t.Errorf("clusterSize = %d, want 2", r.ClusterSize)
	}
	// Representative is the higher-confidence member (0.5) + merge boost 0.02.
	wantConf := 0.5 + 0.02
	if math.Abs(r.Representative.Confidence-wantConf) > 1e-9 {
		t.Errorf("merged confidence = %v, want %v", r.Representative.Confidence, wantConf)
	}
}

func TestClusterObservations_DistinctStaySeparate(t *testing.T) {
	now := time.Now()
	a := mkObs(t, "pattern", "Auth middleware", "auth layer", 0.5, false)
	b := mkObs(t, "pattern", "Database schema", "drizzle migrations orm", 0.5, false)
	in := []ObservationWithTimestamp{
		{Observation: a, RecordedAt: now},
		{Observation: b, RecordedAt: now},
	}

	got := ClusterObservations(in, now, ClusterConfig{})
	if len(got) != 2 {
		t.Fatalf("got %d clusters, want 2 (distinct topics)", len(got))
	}
}

func TestClusterObservations_Decay(t *testing.T) {
	now := time.Now()
	// 45 days old, decayDays=30 → decayFactor = 1 - (45-30)/30 = 0.5.
	obs := mkObs(t, "pattern", "Stale pattern", "old observation", 0.8, false)
	in := []ObservationWithTimestamp{
		{Observation: obs, RecordedAt: now.Add(-45 * 24 * time.Hour)},
	}

	got := ClusterObservations(in, now, ClusterConfig{})
	if len(got) != 1 {
		t.Fatalf("got %d clusters, want 1", len(got))
	}
	if !got[0].Decayed {
		t.Errorf("expected decayed=true at 45 days")
	}
	want := 0.8 * 0.5
	if math.Abs(got[0].Representative.Confidence-want) > 1e-9 {
		t.Errorf("decayed confidence = %v, want %v", got[0].Representative.Confidence, want)
	}
}

func TestEffectiveConfidence(t *testing.T) {
	now := time.Now()
	tests := []struct {
		name     string
		conf     float64
		authored bool
		ageDays  float64
		want     float64
	}{
		{"authored always 1.0", 0.2, true, 1000, 1.0},
		{"fresh inference capped", 0.99, false, 1, 0.95},
		{"fresh below cap", 0.7, false, 10, 0.7},
		{"decayed 45d", 0.8, false, 45, 0.8 * 0.5},
		{"fully decayed 60d", 0.8, false, 60, 0},
		{"fully decayed beyond 2x", 0.8, false, 90, 0},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			obs := mkObs(t, "pattern", "t", "d", tc.conf, tc.authored)
			recordedAt := now.Add(-time.Duration(tc.ageDays*24) * time.Hour)
			got := EffectiveConfidence(obs, recordedAt, now, ClusterConfig{})
			if math.Abs(got-tc.want) > 1e-9 {
				t.Errorf("EffectiveConfidence = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestClusterConfig_Defaults(t *testing.T) {
	var c ClusterConfig
	if c.threshold() != defaultClusterThreshold {
		t.Errorf("threshold default = %v, want %v", c.threshold(), defaultClusterThreshold)
	}
	if c.decayDays() != defaultDecayDays {
		t.Errorf("decayDays default = %v, want %v", c.decayDays(), defaultDecayDays)
	}
	if c.inferenceCap() != defaultInferenceConfCap {
		t.Errorf("inferenceCap default = %v, want %v", c.inferenceCap(), defaultInferenceConfCap)
	}

	override := ClusterConfig{SimilarityThreshold: 0.9, DecayDays: 7, InferenceConfidenceCap: 0.8}
	if override.threshold() != 0.9 || override.decayDays() != 7 || override.inferenceCap() != 0.8 {
		t.Errorf("override not honored: %+v", override)
	}
}
