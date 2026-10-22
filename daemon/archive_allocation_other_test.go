//go:build !darwin && !linux

package daemon

import "errors"

const archiveAllocationGrowthSupported = false

func growFileAllocation(string, int64) error { return errors.ErrUnsupported }
