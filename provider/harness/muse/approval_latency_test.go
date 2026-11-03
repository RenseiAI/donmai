package muse

import (
	"fmt"
	"sort"
	"strings"
	"testing"
	"time"
)

// This file is offline measurement evidence, not a production metric:
// it pairs approval/reviewing observations with their approval/resolved
// observations by (session, approval) identity using observer
// timestamps, and renders the timestamp-pairing report the change
// ships with. Nothing here runs in production; the harness emits no
// latency signal of its own.

// approvalObservation is one timestamped approval-phase sighting with
// its observer timestamp. Source names where the observation came
// from: "replay" for deterministic fixture replays, "captured" for
// passive captured events.
type approvalObservation struct {
	sessionID  string
	approvalID string
	phase      string // "reviewing" or "resolved"
	at         time.Time
	source     string
}

// approvalLatencyPair is one matched reviewing→resolved duration.
type approvalLatencyPair struct {
	sessionID  string
	approvalID string
	duration   time.Duration
}

// approvalLatencyReport is the timestamp-pairing report: every
// paired duration plus the sample/unmatched/duplicate counts and
// summary quantiles. LiveMeasured stays false unless the sample
// carries captured observer timestamps.
type approvalLatencyReport struct {
	source       string
	pairs        []approvalLatencyPair
	unmatched    int
	duplicates   int
	liveMeasured bool
}

// median returns the median pair duration; ok is false when the
// sample has no pairs.
func (r approvalLatencyReport) median() (d time.Duration, ok bool) {
	return quantileDurations(pairDurations(r.pairs), 0.5)
}

// p95 returns the nearest-rank 95th percentile; ok is false when the
// sample has no pairs.
func (r approvalLatencyReport) p95() (d time.Duration, ok bool) {
	return quantileDurations(pairDurations(r.pairs), 0.95)
}

func pairDurations(pairs []approvalLatencyPair) []time.Duration {
	out := make([]time.Duration, len(pairs))
	for i, p := range pairs {
		out[i] = p.duration
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// quantileDurations computes the median (q=0.5, averaging the two
// middles on even samples) or a nearest-rank quantile otherwise.
func quantileDurations(sorted []time.Duration, q float64) (time.Duration, bool) {
	if len(sorted) == 0 {
		return 0, false
	}
	if q == 0.5 && len(sorted)%2 == 0 {
		return (sorted[len(sorted)/2-1] + sorted[len(sorted)/2]) / 2, true
	}
	rank := int(q*float64(len(sorted)) + 0.999999999) // ceil without math
	if rank < 1 {
		rank = 1
	}
	if rank > len(sorted) {
		rank = len(sorted)
	}
	return sorted[rank-1], true
}

// render returns the human-readable timestamp-pairing report.
func (r approvalLatencyReport) render() string {
	var b strings.Builder
	fmt.Fprintf(&b, "approval-review latency report (source: %s)\n", r.source)
	fmt.Fprintf(&b, "sample: %d paired durations, %d unmatched observations, %d duplicates\n", len(r.pairs), r.unmatched, r.duplicates)
	for _, p := range r.pairs {
		fmt.Fprintf(&b, "  session %s approval %s: reviewing->resolved %s\n", p.sessionID, p.approvalID, p.duration)
	}
	if med, ok := r.median(); ok {
		fmt.Fprintf(&b, "median: %s\n", med)
	} else {
		b.WriteString("median: n/a (no pairs)\n")
	}
	if p95, ok := r.p95(); ok {
		fmt.Fprintf(&b, "p95: %s\n", p95)
	} else {
		b.WriteString("p95: n/a (no pairs)\n")
	}
	if r.liveMeasured {
		b.WriteString("live latency: measured from captured observer timestamps\n")
	} else {
		b.WriteString("live latency: unmeasured (no captured timestamps; replay only)\n")
	}
	return b.String()
}

// pairApprovalDurations pairs the earliest reviewing observation with
// the next resolved observation at or after it, per (session,
// approval) identity. Observations beyond the pair on a paired key
// count as duplicates; keys holding only one phase count as
// unmatched. Input order is irrelevant: pairing sorts by observer
// timestamp.
func pairApprovalDurations(obs []approvalObservation) approvalLatencyReport {
	rep := approvalLatencyReport{source: "replay (fixture)"}
	groups := map[string][]approvalObservation{}
	for _, o := range obs {
		if o.source == "captured" {
			rep.liveMeasured = true
			rep.source = "captured + replay (mixed)"
		}
		groups[o.sessionID+"\x00"+o.approvalID] = append(groups[o.sessionID+"\x00"+o.approvalID], o)
	}
	// Deterministic pair order: sort keys.
	keys := make([]string, 0, len(groups))
	for k := range groups {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		g := groups[k]
		sort.SliceStable(g, func(i, j int) bool { return g[i].at.Before(g[j].at) })
		firstReview := -1
		for i, o := range g {
			if o.phase == "reviewing" {
				firstReview = i
				break
			}
		}
		resolved := -1
		if firstReview >= 0 {
			for i, o := range g {
				if o.phase == "resolved" && !o.at.Before(g[firstReview].at) {
					resolved = i
					break
				}
			}
		}
		if firstReview >= 0 && resolved >= 0 {
			rep.pairs = append(rep.pairs, approvalLatencyPair{
				sessionID:  g[firstReview].sessionID,
				approvalID: g[firstReview].approvalID,
				duration:   g[resolved].at.Sub(g[firstReview].at),
			})
			rep.duplicates += len(g) - 2
		} else {
			rep.unmatched += len(g)
		}
	}
	return rep
}

// pairApprovalDurationsByApprovalOnly is the RED-control pairing: it
// groups by approval id WITHOUT session identity. On the interleaved
// fixture it must disagree with the correct pairing, proving the
// fixture discriminates session identity.
func pairApprovalDurationsByApprovalOnly(obs []approvalObservation) approvalLatencyReport {
	relabeled := make([]approvalObservation, len(obs))
	for i, o := range obs {
		o.sessionID = ""
		relabeled[i] = o
	}
	return pairApprovalDurations(relabeled)
}

// interleavedLatencySample is the deterministic two-session fixture:
// sessions A and B share one approval id, their observations
// interleave in arrival order, one duplicate reviewing hides in
// session A, and two unmatched observations (one reviewing-only key,
// one resolved-only key) round out the sample.
func interleavedLatencySample(base time.Time) []approvalObservation {
	at := func(sec int) time.Time { return base.Add(time.Duration(sec) * time.Second) }
	return []approvalObservation{
		{sessionID: "sess-A", approvalID: "appr-1", phase: "reviewing", at: at(0), source: "replay"},
		{sessionID: "sess-B", approvalID: "appr-1", phase: "reviewing", at: at(5), source: "replay"},
		{sessionID: "sess-A", approvalID: "appr-1", phase: "reviewing", at: at(1), source: "replay"}, // duplicate
		{sessionID: "sess-A", approvalID: "appr-1", phase: "resolved", at: at(10), source: "replay"},
		{sessionID: "sess-B", approvalID: "appr-1", phase: "resolved", at: at(20), source: "replay"},
		{sessionID: "sess-C", approvalID: "appr-9", phase: "reviewing", at: at(2), source: "replay"}, // unmatched
		{sessionID: "sess-D", approvalID: "appr-7", phase: "resolved", at: at(3), source: "replay"},  // unmatched
	}
}

func TestPairApprovalDurations_TwoSessionInterleaved(t *testing.T) {
	rep := pairApprovalDurations(interleavedLatencySample(time.Unix(1_700_000_000, 0).UTC()))
	if len(rep.pairs) != 2 {
		t.Fatalf("pairs=%d, want 2", len(rep.pairs))
	}
	want := map[string]time.Duration{"sess-A": 10 * time.Second, "sess-B": 15 * time.Second}
	for _, p := range rep.pairs {
		if p.approvalID != "appr-1" {
			t.Errorf("pair=%+v, want approval appr-1", p)
		}
		if want[p.sessionID] != p.duration {
			t.Errorf("session %s duration=%s, want %s", p.sessionID, p.duration, want[p.sessionID])
		}
	}
	if rep.duplicates != 1 {
		t.Errorf("duplicates=%d, want 1", rep.duplicates)
	}
	if rep.unmatched != 2 {
		t.Errorf("unmatched=%d, want 2", rep.unmatched)
	}
	if med, ok := rep.median(); !ok || med != 12500*time.Millisecond {
		t.Errorf("median=%s ok=%v, want 12.5s", med, ok)
	}
	if p95, ok := rep.p95(); !ok || p95 != 15*time.Second {
		t.Errorf("p95=%s ok=%v, want 15s", p95, ok)
	}
	if rep.liveMeasured {
		t.Error("replay-only sample must not claim a live measurement")
	}
}

// TestPairApprovalDurations_RequiresSessionIdentity is the RED
// control: pairing by approval id without session identity merges
// the two sessions and must NOT reproduce the correct report on this
// fixture.
func TestPairApprovalDurations_RequiresSessionIdentity(t *testing.T) {
	obs := interleavedLatencySample(time.Unix(1_700_000_000, 0).UTC())
	naive := pairApprovalDurationsByApprovalOnly(obs)
	if len(naive.pairs) == 2 {
		t.Fatalf("naive pairing produced %d pairs; the fixture must discriminate session identity (want != 2)", len(naive.pairs))
	}
	var naiveDurations []time.Duration
	for _, p := range naive.pairs {
		naiveDurations = append(naiveDurations, p.duration)
	}
	t.Logf("naive (approval-only) pairing yields %d pairs %v; correct pairing yields 2 pairs [10s 15s]", len(naive.pairs), naiveDurations)
}

// TestApprovalLatencyReport_RendersReplayReport pins the report
// shape: replay-labeled, with per-pair durations, counts,
// quantiles, and the explicit live-unmeasured line.
func TestApprovalLatencyReport_RendersReplayReport(t *testing.T) {
	rep := pairApprovalDurations(interleavedLatencySample(time.Unix(1_700_000_000, 0).UTC()))
	out := rep.render()
	t.Logf("timestamp-pairing report:\n%s", out)
	for _, want := range []string{
		"source: replay (fixture)",
		"sample: 2 paired durations, 2 unmatched observations, 1 duplicates",
		"sess-A", "sess-B", "10s", "15s",
		"median: 12.5s",
		"p95: 15s",
		"live latency: unmeasured",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("report missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "live latency: measured") || strings.Contains(out, "captured + replay") {
		t.Errorf("replay-only report must not claim captured data:\n%s", out)
	}
}
