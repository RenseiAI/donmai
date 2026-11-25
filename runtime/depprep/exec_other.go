//go:build !unix

package depprep

import "os/exec"

// isolateProcessGroup has no process-group primitive off unix; the child
// alone is killed on cancellation.
func isolateProcessGroup(*exec.Cmd) {}
