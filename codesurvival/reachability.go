package codesurvival

import (
	"math"
	"sort"
)

// RW4 — reachability / hot-path weighting.
//
// After RW3 survival succeeds, the executor runs a per-language reachability
// pass over the SAME clone to decide, for each surviving line, whether it lives
// inside a symbol reachable from a user-facing entrypoint (hot), outside any
// reachable symbol (cold), or in a symbol whose reachability could not be
// resolved (unknown → weighted as hot, never down-weighted).
//
// Weighting (documented, publishable — runs/2026-05-30-code-survival-hotpath-routing):
//
//	hot_weighted_surviving = hot + W_COLD*cold     (unknown counts as hot)
//	hot_weighted_rate_pct  = 100 * hot_weighted_surviving / lines_total_at_merge
//
// W_COLD ∈ [0,1], default 0.25. A surviving dead function inside a live file is
// classified cold and down-weighted vs a reachable one; a dynamic/unresolved
// symbol is unknown → hot (no down-weight, to avoid false negatives).
//
// GRACEFUL DEGRADATION: reachability NEVER hard-fails a scan. A toolchain that
// is absent, an OOM/timeout, or a crash collapses to status:"partial",
// hotWeighted=null, perSymbol=[] — survival (RW3) is preserved untouched.

// defaultWCold is the soft weight applied to surviving cold lines. NOT a hard
// reachable/dead gate — a documented down-weight. Mirrors w_cold default 0.25 in
// the platform project_code_survival_config table.
const defaultWCold = 0.25

// maxPerSymbol bounds the perSymbol[] slice in the result payload. The platform
// ingestion upserts code_survival_symbol_metrics one row per entry; an
// unbounded list on a huge PR would blow the request body. Symbols are sorted
// by linesSurviving desc so the cap keeps the most-survival-bearing symbols.
const maxPerSymbol = 2000

// symbolSpan is a resolved symbol with its line range in a file-at-HEAD and its
// reachability classification. Produced by each per-language reachability pass.
type symbolSpan struct {
	file      string
	symbol    string
	startLine int
	// endLine is the last line of the symbol (inclusive). 0 means "unknown end"
	// (e.g. a parse-only fallback) — such a span matches only its startLine.
	endLine   int
	reachable SymbolReachability
}

// reachabilityResult is what a per-language pass returns to the classifier.
type reachabilityResult struct {
	// spans are the symbols discovered in the analysed files, each tagged
	// hot/cold/unknown. A file with no span covering a surviving line means that
	// line is treated as unknown (cannot prove dead → weighted hot).
	spans []symbolSpan
	// partial is true when the pass degraded (timeout / file cap / parse error
	// on part of the tree). The executor maps any partial pass to status:partial
	// + hotWeighted=null even though spans may be present.
	partial bool
	// language identifies the pass ("go" | "ts") for logging.
	language string
}

// classifyResult is the output of classifying surviving lines against the union
// of all reachability passes. It carries everything needed to populate
// result.hotWeighted + result.perSymbol.
type classifyResult struct {
	hotLines  int
	coldLines int
	// unknownLines are folded into hot for the weighted rate (no down-weight) but
	// tracked separately for observability/perSymbol classification.
	unknownLines int
	perSymbol    []ScanSymbolBreakdown
}

// fileSpans indexes the symbol spans of one file, sorted by startLine, for
// O(log n) line→symbol lookup.
type fileSpans struct {
	spans []symbolSpan
}

// classifySurvivingLines maps every surviving line (per file, from the RW3 blame
// pass) onto a symbol span from the union of reachability passes and tallies
// hot/cold/unknown. A surviving line:
//   - inside a reachable (hot) symbol → hot
//   - inside an unreachable (cold) symbol → cold
//   - inside an unknown symbol, OR in a file analysed but covered by no span,
//     OR in a file no pass analysed → unknown (weighted as hot)
//
// MIXED-LANGUAGE UNION: passes are unioned per file. A .go file is covered by
// the Go pass; a .ts file by the TS pass; a file touched by both (rare) takes
// the strongest signal (hot > cold > unknown) so neither pass can falsely
// down-weight a line the other proved reachable.
func classifySurvivingLines(survivingByFile map[string][]int, passes ...reachabilityResult) classifyResult {
	// Build a per-file, per-line classification from the union of passes.
	// fileLineClass[file][line] = strongest reachability seen for that line.
	byFile := map[string]*fileSpans{}
	for _, p := range passes {
		for _, s := range p.spans {
			fs := byFile[s.file]
			if fs == nil {
				fs = &fileSpans{}
				byFile[s.file] = fs
			}
			fs.spans = append(fs.spans, s)
		}
	}
	for _, fs := range byFile {
		sort.Slice(fs.spans, func(i, j int) bool { return fs.spans[i].startLine < fs.spans[j].startLine })
	}

	out := classifyResult{}
	// Accumulate per-symbol surviving counts keyed by (file,symbol,start) so we
	// emit one perSymbol row per symbol with its surviving line tally.
	type symKey struct {
		file, symbol string
		start, end   int
		reachable    SymbolReachability
	}
	symCounts := map[symKey]int{}

	for file, lines := range survivingByFile {
		fs := byFile[file]
		for _, ln := range lines {
			r, sp, matched := classifyLine(fs, ln)
			switch r {
			case ReachableHot:
				out.hotLines++
			case ReachableCold:
				out.coldLines++
			default:
				out.unknownLines++
			}
			if matched {
				k := symKey{file: sp.file, symbol: sp.symbol, start: sp.startLine, end: sp.endLine, reachable: r}
				symCounts[k]++
			}
		}
	}

	// Build bounded perSymbol[].
	for k, n := range symCounts {
		end := k.end
		var endPtr *int
		if end > 0 {
			endPtr = &end
		}
		out.perSymbol = append(out.perSymbol, ScanSymbolBreakdown{
			File:           k.file,
			Symbol:         k.symbol,
			StartLine:      k.start,
			EndLine:        endPtr,
			LinesSurviving: n,
			Reachable:      k.reachable,
		})
	}
	// Sort by linesSurviving desc, then file/symbol for determinism, then cap.
	sort.Slice(out.perSymbol, func(i, j int) bool {
		a, b := out.perSymbol[i], out.perSymbol[j]
		if a.LinesSurviving != b.LinesSurviving {
			return a.LinesSurviving > b.LinesSurviving
		}
		if a.File != b.File {
			return a.File < b.File
		}
		if a.Symbol != b.Symbol {
			return a.Symbol < b.Symbol
		}
		return a.StartLine < b.StartLine
	})
	if len(out.perSymbol) > maxPerSymbol {
		out.perSymbol = out.perSymbol[:maxPerSymbol]
	}
	return out
}

// classifyLine resolves the strongest reachability covering line ln in a file.
// Returns the classification, the covering span (when matched), and whether a
// span matched at all. An unmatched line is unknown (no span proves it dead).
func classifyLine(fs *fileSpans, ln int) (SymbolReachability, symbolSpan, bool) {
	if fs == nil {
		return ReachableUnknown, symbolSpan{}, false
	}
	var best symbolSpan
	bestRank := -1
	matched := false
	for _, s := range fs.spans {
		if s.startLine > ln {
			// Spans are sorted by startLine; any later span starts even further
			// past ln and so cannot cover it. Safe to stop.
			break
		}
		within := false
		if s.endLine > 0 {
			within = ln >= s.startLine && ln <= s.endLine
		} else {
			within = ln == s.startLine
		}
		if !within {
			continue
		}
		if r := rank(s.reachable); r > bestRank {
			bestRank = r
			best = s
			matched = true
		}
	}
	if !matched {
		return ReachableUnknown, symbolSpan{}, false
	}
	return best.reachable, best, true
}

// rank orders reachability so the union takes the strongest signal: a line
// proven hot by any pass/span is hot, even if another span calls it cold.
func rank(r SymbolReachability) int {
	switch r {
	case ReachableHot:
		return 2
	case ReachableCold:
		return 1
	default: // unknown
		return 0
	}
}

// computeHotWeighted folds a classifyResult + total-at-merge into the
// ScanHotWeighted payload. unknown is added to hot (no down-weight). Returns a
// nil rate when total == 0 (mirrors computeSurvivalRate's zero-vs-null contract).
func computeHotWeighted(c classifyResult, linesTotalAtMerge int, wCold float64) ScanHotWeighted {
	hot := c.hotLines + c.unknownLines // unknown weighted as hot
	cold := c.coldLines
	hw := ScanHotWeighted{
		HotLinesSurviving:  hot,
		ColdLinesSurviving: cold,
		WCold:              wCold,
	}
	if linesTotalAtMerge <= 0 {
		return hw
	}
	raw := 100 * (float64(hot) + wCold*float64(cold)) / float64(linesTotalAtMerge)
	rounded := math.Round(raw*100) / 100
	hw.HotWeightedRatePct = &rounded
	return hw
}
