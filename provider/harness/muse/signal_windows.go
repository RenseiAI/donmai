//go:build windows

package muse

import "os"

func sigTERM() os.Signal { return os.Interrupt }
func sigKILL() os.Signal { return os.Kill }
