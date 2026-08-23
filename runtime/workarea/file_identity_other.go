//go:build !darwin && !linux

package workarea

import (
	"fmt"
	"os"
)

func fileIdentity(os.FileInfo) (FileIdentity, error) {
	return FileIdentity{}, fmt.Errorf("runtime/workarea: durable filesystem identity unsupported on this platform")
}

func allocatedFileBytes(os.FileInfo) (int64, error) {
	return 0, fmt.Errorf("runtime/workarea: physical disk accounting unsupported on this platform")
}
