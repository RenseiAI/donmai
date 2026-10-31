//go:build !darwin && !linux

package daemon

import (
	"context"
	"fmt"
	"os"
)

func copySparseData(_ context.Context, _, _ *os.File, _ int64) error {
	return fmt.Errorf("archive copy cannot preserve sparse extents on this platform")
}
