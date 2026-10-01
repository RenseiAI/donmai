//go:build !darwin && !linux

package localgithub

import "io/fs"

func fileLinkCount(fs.FileInfo) uint64 { return 0 }
