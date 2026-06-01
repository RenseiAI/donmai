package codesurvival

import (
	"math"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// survivalRateInput is the pre-collected line counts the pure compute consumes.
type survivalRateInput struct {
	linesTotalAtMerge int
	linesSurviving    int
}

// survivalRateResult mirrors SurvivalRateResult in
// platform/src/lib/factory/code-survival.ts.
type survivalRateResult struct {
	linesTotalAtMerge int
	linesSurviving    int
	// survivalRatePct on [0,100], or nil when there were zero lines at merge.
	// nil is preferred over 0 because a 0% reading is misleading in rollups.
	survivalRatePct *float64
}

// computeSurvivalRate is a verbatim port of computeSurvivalRate in
// platform/src/lib/factory/code-survival.ts:70-89.
//
// Pure arithmetic, stable to two decimals (the DB column is numeric(5,2)).
// total clamps at >= 0; surviving clamps into [0, total]. The TS source floors
// its (possibly fractional) inputs; our inputs are already line counts (int) so
// the floor is a no-op and is elided.
func computeSurvivalRate(in survivalRateInput) survivalRateResult {
	total := in.linesTotalAtMerge
	if total < 0 {
		total = 0
	}
	surviving := in.linesSurviving
	if surviving < 0 {
		surviving = 0
	}
	if surviving > total {
		surviving = total
	}

	if total == 0 {
		return survivalRateResult{linesTotalAtMerge: 0, linesSurviving: 0, survivalRatePct: nil}
	}

	raw := (float64(surviving) / float64(total)) * 100
	rounded := math.Round(raw*100) / 100

	return survivalRateResult{
		linesTotalAtMerge: total,
		linesSurviving:    surviving,
		survivalRatePct:   &rounded,
	}
}

// blameShaPrefix matches a 40-hex sha at the very start of a line followed by a
// space (the start of a `git blame --line-porcelain` entry). Equivalent to the
// JS /^([0-9a-f]{40}) /i in countLinesByCommit.
var blameShaPrefix = regexp.MustCompile(`(?i)^([0-9a-f]{40}) `)

// blameHeaderLine matches a full `--line-porcelain` header line and captures
// the SHA and the FINAL line number (3rd field):
//
//	<40-hex-sha> <orig-line> <final-line> [<group-size>]
//
// RW4 reachability needs the final (HEAD) line number — which symbol a surviving
// line falls in — not just the count countLinesByCommit returns.
var blameHeaderLine = regexp.MustCompile(`(?i)^([0-9a-f]{40}) \d+ (\d+)`)

// countLinesByCommit is a verbatim port of countLinesByCommit in
// platform/src/lib/factory/code-survival.ts:303-323.
//
// It counts lines in `git blame --line-porcelain` output attributed to a given
// commit SHA. Each blame entry begins with a 40-hex sha at the start of a line
// followed by a space; there is one entry per blamed line. Abbreviated SHAs are
// accepted via prefix matching in either direction.
func countLinesByCommit(porcelain, commitSha string) int {
	if commitSha == "" || porcelain == "" {
		return 0
	}
	want := strings.ToLower(commitSha)
	count := 0
	for _, line := range strings.Split(porcelain, "\n") {
		m := blameShaPrefix.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		sha := strings.ToLower(m[1])
		if sha == want || strings.HasPrefix(sha, want) || strings.HasPrefix(want, sha) {
			count++
		}
	}
	return count
}

// survivingLinesByCommit is countLinesByCommit's sibling that returns the FINAL
// (HEAD-side) line numbers of every blame entry attributed to commitSha, rather
// than just the count. The result is the set of line numbers in the file-at-HEAD
// that survive from the merge — RW4 maps each onto a symbol to classify it
// hot/cold/unknown. Returned in ascending order with no duplicates.
func survivingLinesByCommit(porcelain, commitSha string) []int {
	if commitSha == "" || porcelain == "" {
		return nil
	}
	want := strings.ToLower(commitSha)
	var lines []int
	for _, line := range strings.Split(porcelain, "\n") {
		m := blameHeaderLine.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		sha := strings.ToLower(m[1])
		if sha != want && !strings.HasPrefix(sha, want) && !strings.HasPrefix(want, sha) {
			continue
		}
		n, err := strconv.Atoi(m[2])
		if err != nil || n <= 0 {
			continue
		}
		lines = append(lines, n)
	}
	if len(lines) <= 1 {
		return lines
	}
	sort.Ints(lines)
	out := lines[:1]
	for _, n := range lines[1:] {
		if n != out[len(out)-1] {
			out = append(out, n)
		}
	}
	return out
}
