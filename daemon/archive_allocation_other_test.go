//go:build !darwin && !linux

package daemon

import "errors"

func growFileAllocation(string, int64) error { return errors.ErrUnsupported }
