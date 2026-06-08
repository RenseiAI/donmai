package codeintel

// arch_cluster.go — observation cluster + dedupe + decay pass for the
// arch-intel pipeline. Port of donmai-libraries/packages/architectural-intelligence/src/cluster.ts.
//
// Design (mirrors cluster.ts):
//
//   - Text similarity uses the Jaccard coefficient over normalized token sets.
//     No ML deps — fast, deterministic. similarityThreshold >= 0.6 → same cluster.
//   - Decay: observations not reinforced for N days (default 30) have their
//     effective confidence reduced before they feed contribute(). Decay is soft
//     (confidence reduction only) — rows are never deleted.
//   - Cluster merging: the highest-confidence member is the representative; each
//     extra member adds a small merge boost (capped). Reinforcement counts add a
//     further small boost.
//   - Authored intent (CLAUDE.md, ADRs) bypasses clustering entirely — always
//     emitted as-is with confidence 1.0. This upholds the authored-intent
//     constraint from 007 §"non-negotiable principles".

import (
	"encoding/json"
	"math"
	"sort"
	"strings"
	"time"
)

// Cluster tuning defaults (mirror cluster.ts DEFAULT_* constants).
const (
	defaultClusterThreshold = 0.6
	defaultDecayDays        = 30.0
	defaultInferenceConfCap = 0.95
)

// ClusterConfig tunes the clustering/decay pass. Zero values fall back to the
// package defaults, matching the optional-field semantics of cluster.ts.
type ClusterConfig struct {
	// SimilarityThreshold is the Jaccard similarity at/above which two
	// observations merge into one cluster. Range 0..1. 0 → default (0.6).
	SimilarityThreshold float64
	// DecayDays is the age (days) after which an un-reinforced observation
	// begins to decay. 0 → default (30).
	DecayDays float64
	// InferenceConfidenceCap caps the confidence of inferred (non-authored)
	// observations after merge boosting. 0 → default (0.95).
	InferenceConfidenceCap float64
}

func (c ClusterConfig) threshold() float64 {
	if c.SimilarityThreshold > 0 {
		return c.SimilarityThreshold
	}
	return defaultClusterThreshold
}

func (c ClusterConfig) decayDays() float64 {
	if c.DecayDays > 0 {
		return c.DecayDays
	}
	return defaultDecayDays
}

func (c ClusterConfig) inferenceCap() float64 {
	if c.InferenceConfidenceCap > 0 {
		return c.InferenceConfidenceCap
	}
	return defaultInferenceConfCap
}

// ObservationWithTimestamp is the input unit to the clusterer.
type ObservationWithTimestamp struct {
	Observation ArchObservation
	// RecordedAt is when this observation was first recorded (drives decay).
	RecordedAt time.Time
	// ReinforcementCount is how many times this observation has been seen again.
	ReinforcementCount int
}

// ClusterResult is one deduplicated cluster's representative observation plus
// metadata.
type ClusterResult struct {
	// Representative is the highest-confidence member, with confidence possibly
	// boosted by merges/reinforcements and possibly reduced by decay.
	Representative ArchObservation
	// ClusterSize is the number of source observations merged in (1 = singleton).
	ClusterSize int
	// Decayed reports whether the representative's confidence was reduced for
	// staleness. The caller may drop decayed clusters below a confidence floor.
	Decayed bool
}

// ClusterObservations clusters and deduplicates timestamped observations.
//
// Authored observations (source.authoredDoc present) bypass clustering and are
// emitted as-is with confidence 1.0. All others are subject to Jaccard
// clustering and decay. now is the reference time for decay; pass time.Now() in
// production and a fixed time in tests.
func ClusterObservations(observations []ObservationWithTimestamp, now time.Time, cfg ClusterConfig) []ClusterResult {
	threshold := cfg.threshold()
	decayDays := cfg.decayDays()
	inferenceCap := cfg.inferenceCap()

	var results []ClusterResult

	// Partition: authored observations bypass clustering entirely.
	var authored, inferred []ObservationWithTimestamp
	for _, item := range observations {
		if item.Observation.Source.AuthoredDoc != nil {
			authored = append(authored, item)
		} else {
			inferred = append(inferred, item)
		}
	}

	// Authored: emit directly with confidence 1.0.
	for _, item := range authored {
		rep := item.Observation
		rep.Confidence = 1.0
		results = append(results, ClusterResult{
			Representative: rep,
			ClusterSize:    1,
			Decayed:        false,
		})
	}

	// Inferred: cluster by Jaccard similarity over text tokens.
	clusters := buildClusters(inferred, threshold)

	for _, cluster := range clusters {
		// Representative: highest-confidence member. Stable sort keeps the TS
		// "first wins on ties" behaviour (the JS sort is not guaranteed stable,
		// but insertion order is the natural tiebreak here).
		sorted := make([]ObservationWithTimestamp, len(cluster))
		copy(sorted, cluster)
		sort.SliceStable(sorted, func(i, j int) bool {
			return sorted[i].Observation.Confidence > sorted[j].Observation.Confidence
		})
		representative := sorted[0]
		remainingCount := len(sorted) - 1

		// Merge boost: each additional member adds 0.02, capped at 0.1.
		mergeBoost := math.Min(float64(remainingCount)*0.02, 0.1)
		effective := math.Min(representative.Observation.Confidence+mergeBoost, inferenceCap)

		// Decay: reduce confidence if the representative is stale.
		ageDays := now.Sub(representative.RecordedAt).Hours() / 24
		decayed := ageDays > decayDays
		if decayed {
			// Linear decay: full decay at 2x the decayDays threshold.
			decayFactor := math.Max(0, 1-(ageDays-decayDays)/decayDays)
			effective *= decayFactor
		}

		// Reinforcement boost: bump slightly per extra seen count, capped 0.05.
		if representative.ReinforcementCount > 0 {
			reinforcementBoost := math.Min(float64(representative.ReinforcementCount)*0.01, 0.05)
			effective = math.Min(effective+reinforcementBoost, inferenceCap)
		}

		rep := representative.Observation
		rep.Confidence = effective
		results = append(results, ClusterResult{
			Representative: rep,
			ClusterSize:    len(cluster),
			Decayed:        decayed,
		})
	}

	return results
}

// buildClusters does greedy single-pass clustering by Jaccard similarity.
//
// For each observation, find an existing cluster whose representative-token-set
// has similarity >= threshold; merge into it (and union the token set).
// Otherwise start a new cluster. O(n²) worst case — fine for the expected
// tens-to-hundreds-per-pass volumes.
func buildClusters(items []ObservationWithTimestamp, threshold float64) [][]ObservationWithTimestamp {
	var clusters [][]ObservationWithTimestamp
	var repTokens []map[string]struct{}

	for _, item := range items {
		tokens := archTokenize(observationText(item.Observation))
		merged := false

		for i := range clusters {
			if jaccardSimilarity(tokens, repTokens[i]) >= threshold {
				clusters[i] = append(clusters[i], item)
				// Update representative tokens to the union of the cluster.
				for t := range tokens {
					repTokens[i][t] = struct{}{}
				}
				merged = true
				break
			}
		}

		if !merged {
			clusters = append(clusters, []ObservationWithTimestamp{item})
			repTokens = append(repTokens, tokens)
		}
	}

	return clusters
}

// observationText extracts a searchable text representation from an observation.
// Uses payload title + description when present; else JSON-stringifies payload.
// Mirrors cluster.ts _observationText.
func observationText(obs ArchObservation) string {
	if len(obs.Payload) > 0 {
		var m map[string]any
		if err := json.Unmarshal(obs.Payload, &m); err == nil && m != nil {
			title, _ := m["title"].(string)
			desc, _ := m["description"].(string)
			return strings.TrimSpace(obs.Kind + " " + title + " " + desc)
		}
	}
	return obs.Kind + " " + string(obs.Payload)
}

// clusterStopWords mirrors the STOP_WORDS set in cluster.ts _tokenize.
var clusterStopWords = map[string]struct{}{
	"the": {}, "a": {}, "an": {}, "and": {}, "or": {}, "in": {}, "on": {},
	"at": {}, "to": {}, "for": {}, "of": {}, "is": {}, "are": {}, "was": {},
	"were": {}, "be": {}, "been": {}, "have": {}, "has": {}, "had": {}, "do": {},
	"does": {}, "did": {}, "with": {}, "that": {}, "this": {}, "from": {},
	"by": {}, "as": {}, "it": {}, "its": {}, "not": {}, "but": {}, "all": {},
	"we": {}, "they": {},
}

// archTokenize normalizes a string into a set of lowercase alphanumeric tokens.
// Lowercased, split on non-alphanumeric, tokens < 2 chars dropped, stop words
// dropped. Mirrors cluster.ts _tokenize. (Named archTokenize to avoid colliding
// with bm25.go's tokenize, which returns an ordered slice for BM25 scoring.)
func archTokenize(text string) map[string]struct{} {
	tokens := make(map[string]struct{})
	for _, raw := range splitNonAlnum(strings.ToLower(text)) {
		if len(raw) >= 2 {
			if _, stop := clusterStopWords[raw]; !stop {
				tokens[raw] = struct{}{}
			}
		}
	}
	return tokens
}

// splitNonAlnum splits on runs of non-[a-z0-9] characters, matching the JS
// regex /[^a-z0-9]+/ (input is already lowercased by the caller).
func splitNonAlnum(s string) []string {
	return strings.FieldsFunc(s, func(r rune) bool {
		return !((r >= 'a' && r <= 'z') || (r >= '0' && r <= '9'))
	})
}

// jaccardSimilarity returns |a ∩ b| / |a ∪ b|. Both empty → 0 (undefined
// similarity → not similar). Mirrors cluster.ts _jaccardSimilarity.
func jaccardSimilarity(a, b map[string]struct{}) float64 {
	if len(a) == 0 && len(b) == 0 {
		return 0
	}
	intersection := 0
	for t := range a {
		if _, ok := b[t]; ok {
			intersection++
		}
	}
	union := len(a) + len(b) - intersection
	if union == 0 {
		return 0
	}
	return float64(intersection) / float64(union)
}

// EffectiveConfidence returns the decay-adjusted effective confidence for a
// single observation given its age. Authored observations always return 1.0;
// inferences decay linearly to 0 between decayDays and 2*decayDays. Mirrors
// cluster.ts effectiveConfidence (exported for the contribute/decay path and
// tests).
func EffectiveConfidence(obs ArchObservation, recordedAt, now time.Time, cfg ClusterConfig) float64 {
	if obs.Source.AuthoredDoc != nil {
		return 1.0
	}

	decayDays := cfg.decayDays()
	inferenceCap := cfg.inferenceCap()

	ageDays := now.Sub(recordedAt).Hours() / 24
	capped := math.Min(obs.Confidence, inferenceCap)

	if ageDays <= decayDays {
		return capped
	}

	decayFactor := math.Max(0, 1-(ageDays-decayDays)/decayDays)
	return capped * decayFactor
}
