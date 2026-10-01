//go:build !darwin && !linux

package localqueue

import "os"

func lockWriter(*os.File) error   { return ErrUnsupportedPlatform }
func unlockWriter(*os.File) error { return nil }
