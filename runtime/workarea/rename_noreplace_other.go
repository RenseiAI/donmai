//go:build !darwin && !linux

package workarea

import "fmt"

func renameNoReplace(_, _ string) error {
	return fmt.Errorf("runtime/workarea: atomic no-replace rename unsupported on this platform")
}
