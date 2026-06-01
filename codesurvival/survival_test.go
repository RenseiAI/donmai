package codesurvival

import "testing"

func TestComputeSurvivalRate(t *testing.T) {
	// Mirrors the cases in platform/src/lib/factory/code-survival.ts compute.
	f := func(v float64) *float64 { return &v }
	tests := []struct {
		name    string
		total   int
		surv    int
		want    *float64
		wantTot int
		wantSur int
	}{
		{"all survive", 100, 100, f(100), 100, 100},
		{"half survive", 100, 50, f(50), 100, 50},
		{"two-decimal rounding", 3, 1, f(33.33), 3, 1},
		{"zero total → null", 0, 0, nil, 0, 0},
		{"surviving clamps to total", 10, 25, f(100), 10, 10},
		{"negative clamps to zero", -5, -5, nil, 0, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := computeSurvivalRate(survivalRateInput{linesTotalAtMerge: tt.total, linesSurviving: tt.surv})
			if got.linesTotalAtMerge != tt.wantTot || got.linesSurviving != tt.wantSur {
				t.Errorf("counts = (%d,%d), want (%d,%d)", got.linesTotalAtMerge, got.linesSurviving, tt.wantTot, tt.wantSur)
			}
			switch {
			case tt.want == nil && got.survivalRatePct != nil:
				t.Errorf("pct = %v, want nil", *got.survivalRatePct)
			case tt.want != nil && got.survivalRatePct == nil:
				t.Errorf("pct = nil, want %v", *tt.want)
			case tt.want != nil && got.survivalRatePct != nil && *got.survivalRatePct != *tt.want:
				t.Errorf("pct = %v, want %v", *got.survivalRatePct, *tt.want)
			}
		})
	}
}

func TestCountLinesByCommit(t *testing.T) {
	sha := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	other := "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	// Two entries attributed to sha, one to other.
	porcelain := blameEntry(sha, 1) + blameEntry(other, 2) + blameEntry(sha, 3)

	if got := countLinesByCommit(porcelain, sha); got != 2 {
		t.Errorf("count(sha) = %d, want 2", got)
	}
	if got := countLinesByCommit(porcelain, other); got != 1 {
		t.Errorf("count(other) = %d, want 1", got)
	}
	if got := countLinesByCommit("", sha); got != 0 {
		t.Errorf("count(empty) = %d, want 0", got)
	}
	if got := countLinesByCommit(porcelain, ""); got != 0 {
		t.Errorf("count(no-sha) = %d, want 0", got)
	}
	// Abbreviated SHA prefix match.
	if got := countLinesByCommit(porcelain, sha[:12]); got != 2 {
		t.Errorf("count(abbrev) = %d, want 2", got)
	}
}

// blameEntry produces one --line-porcelain entry attributed to sha at final
// line n.
func blameEntry(sha string, n int) string {
	return sha + " " + itoa(n) + " " + itoa(n) + " 1\n" +
		"author Test\nauthor-mail <t@example.com>\nsummary x\n" +
		"\tsome source line\n"
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		b[i] = '-'
	}
	return string(b[i:])
}
