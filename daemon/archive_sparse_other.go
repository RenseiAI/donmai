//go:build !darwin && !linux

package daemon

import (
	"fmt"
	"os"
)

func copySparseData(_, _ *os.File, _ int64) error {
	return fmt.Errorf("archive copy cannot preserve sparse extents on this platform")
}
