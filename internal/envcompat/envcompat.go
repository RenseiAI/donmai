// Package envcompat provides one-release backward-compat shims for renamed
// environment variables. Each Get* function reads the new DONMAI_* name
// first; if unset it falls back to the legacy AGENTFACTORY_* / AF_* name
// and emits a single deprecation warning on stderr.
//
// All RENSEI_* variables are left untouched — those belong to the
// closed-source rensei-tui binary and must not be renamed here.
package envcompat

import (
	"fmt"
	"os"
	"sync"
)

// warnOnce guards per-variable deprecation warnings so each legacy variable
// triggers at most one warning per process lifetime.
var warnOnce sync.Map

func warnDeprecated(legacy, replacement string) {
	if _, loaded := warnOnce.LoadOrStore(legacy, struct{}{}); !loaded {
		fmt.Fprintf(os.Stderr, "warning: %s is deprecated, use %s (legacy var will be removed in next release)\n", legacy, replacement)
	}
}

// GetProvisioningToken reads DONMAI_PROVISIONING_TOKEN with fallback to
// AF_PROVISIONING_TOKEN.
func GetProvisioningToken() string {
	if v := os.Getenv("DONMAI_PROVISIONING_TOKEN"); v != "" {
		return v
	}
	if v := os.Getenv("AF_PROVISIONING_TOKEN"); v != "" {
		warnDeprecated("AF_PROVISIONING_TOKEN", "DONMAI_PROVISIONING_TOKEN")
		return v
	}
	return ""
}

// GetBaseURL reads DONMAI_BASE_URL with fallback to AF_BASE_URL.
func GetBaseURL() string {
	if v := os.Getenv("DONMAI_BASE_URL"); v != "" {
		return v
	}
	if v := os.Getenv("AF_BASE_URL"); v != "" {
		warnDeprecated("AF_BASE_URL", "DONMAI_BASE_URL")
		return v
	}
	return ""
}

// GetDaemonSentinel reads DONMAI_DAEMON with fallback to AF_DAEMON.
// Used as a re-exec sentinel in the daemonize path.
func GetDaemonSentinel() string {
	if v := os.Getenv("DONMAI_DAEMON"); v != "" {
		return v
	}
	if v := os.Getenv("AF_DAEMON"); v != "" {
		warnDeprecated("AF_DAEMON", "DONMAI_DAEMON")
		return v
	}
	return ""
}

// GetCodeBin reads DONMAI_CODE_BIN with fallback to AGENTFACTORY_CODE_BIN.
func GetCodeBin() string {
	if v := os.Getenv("DONMAI_CODE_BIN"); v != "" {
		return v
	}
	if v := os.Getenv("AGENTFACTORY_CODE_BIN"); v != "" {
		warnDeprecated("AGENTFACTORY_CODE_BIN", "DONMAI_CODE_BIN")
		return v
	}
	return ""
}

// GetArchBin reads DONMAI_ARCH_BIN with fallback to AGENTFACTORY_ARCH_BIN.
func GetArchBin() string {
	if v := os.Getenv("DONMAI_ARCH_BIN"); v != "" {
		return v
	}
	if v := os.Getenv("AGENTFACTORY_ARCH_BIN"); v != "" {
		warnDeprecated("AGENTFACTORY_ARCH_BIN", "DONMAI_ARCH_BIN")
		return v
	}
	return ""
}
