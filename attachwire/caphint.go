package attachwire

import "math"

// capHint returns base+n for use as a make() capacity hint, clamped so the
// addition can never overflow int (go/allocation-size-overflow). n is a
// len() of real bytes in memory, so the clamp is unreachable in practice —
// it exists to bound the arithmetic explicitly; on clamp the caller simply
// allocates lazily via append's amortized growth.
func capHint(base, n int) int {
	if n < 0 || n > math.MaxInt-base {
		return base
	}
	return base + n
}
