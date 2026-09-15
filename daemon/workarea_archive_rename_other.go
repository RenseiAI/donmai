//go:build !darwin && !linux

package daemon

import (
	"fmt"
	"os"
)

func archiveRootRenameNoReplace(*os.Root, string, string) error {
	return fmt.Errorf("archive root: atomic no-replace rename unsupported on this platform")
}
