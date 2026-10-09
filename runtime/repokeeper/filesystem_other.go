//go:build !darwin && !linux

package repokeeper

import "fmt"

// deviceOf is unsupported where the runtime exposes no device identity;
// the store refuses to start rather than guess about filesystem sharing.
func deviceOf(string) (uint64, error) {
	return 0, fmt.Errorf("repo keeper: filesystem identity unsupported on this platform")
}
