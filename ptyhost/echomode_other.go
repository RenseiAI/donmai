//go:build !darwin && !linux

package ptyhost

import "github.com/RenseiAI/donmai/attachwire"

// echoModeOfFd is the fallback for platforms without a wired-up termios read:
// echoMode is reported unknown (0xFF), which biases predictive echo to
// SUPPRESSED (§10) — the safe default.
func echoModeOfFd(uintptr) uint8 { return attachwire.EchoUnknown }
