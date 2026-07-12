package ptyhost

import "math"

// recorderCapHint bounds the cast-line capacity hint (n is a len() of an
// in-memory JSON buffer; the clamp exists to make the arithmetic explicitly
// overflow-safe — go/allocation-size-overflow).
func recorderCapHint(n int) int {
	if n < 0 || n > math.MaxInt-32 {
		return 32
	}
	return n + 32
}
