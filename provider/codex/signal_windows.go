//go:build windows

package codex

import "os"

// syscallSIGTERM returns os.Interrupt on windows since windows has no
// SIGTERM. Out-of-scope for Phase F (REN-1346 deferred), but the build
// constraint keeps the package buildable on windows for downstream
// consumers.
func syscallSIGTERM() os.Signal { return os.Interrupt }
