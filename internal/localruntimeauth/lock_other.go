//go:build !darwin && !linux

package localruntimeauth

import (
	"io/fs"
	"os"
)

func lockWriter(*os.File) error        { return ErrUnsupportedPlatform }
func lockReader(*os.File) error        { return ErrUnsupportedPlatform }
func unlockWriter(*os.File) error      { return ErrUnsupportedPlatform }
func fileLinkCount(fs.FileInfo) uint64 { return 0 }
