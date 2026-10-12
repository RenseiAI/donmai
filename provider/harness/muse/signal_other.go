//go:build !unix && !windows

package muse

import "syscall"

func sigTERM() syscall.Signal { return syscall.SIGTERM }
func sigKILL() syscall.Signal { return syscall.SIGKILL }
