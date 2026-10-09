package daemon

import (
	"fmt"
	"sync"

	"github.com/RenseiAI/donmai/afclient"
	"github.com/RenseiAI/donmai/installer/servicepriority"
)

// processPriorityUnknown is the Mode reported when the running process's
// priority could not be observed.
const processPriorityUnknown = "unknown"

// processPriorityObservation is what the running daemon process observably has,
// as opposed to what was configured for it.
type processPriorityObservation struct {
	// mode is the observed mode; empty when the observation failed.
	mode servicepriority.Mode
	// evidence is the raw observation behind mode.
	evidence string
	// warning is set when the observation failed.
	warning string
}

// processPriorityCache runs an OS probe at most once.
//
// The probe is not free: on macOS it forks ps. The process's priority class is
// fixed when the service manager launches it, so the answer cannot change
// under a running daemon and one observation serves every later request. That
// keeps /status, which hostwatch polls every 1.5 s, off the subprocess path.
type processPriorityCache struct {
	probe     func() (obs processPriorityObservation, supported bool)
	once      sync.Once
	obs       processPriorityObservation
	supported bool
}

func (c *processPriorityCache) get() (processPriorityObservation, bool) {
	c.once.Do(func() { c.obs, c.supported = c.probe() })
	return c.obs, c.supported
}

var daemonProcessPriorityCache = &processPriorityCache{probe: observeProcessPriority}

// warmProcessPriorityStatus starts the one-time observation in the background
// so the first status request does not pay for it.
func warmProcessPriorityStatus() {
	go func() { _, _ = daemonProcessPriorityCache.get() }()
}

// daemonProcessPriorityStatus is the daemon's process-priority self-check for
// /status and /doctor. It returns nil on an OS where the mode cannot be
// observed, so the field is omitted rather than reported as something it is
// not.
//
// The observed mode comes from the cached probe. The configured mode is read
// fresh on each call (one small file) so a reinstall that has not restarted the
// daemon yet shows up as a mismatch immediately.
func daemonProcessPriorityStatus() *afclient.DaemonProcessPriorityStatus {
	obs, supported := daemonProcessPriorityCache.get()
	if !supported {
		return nil
	}
	configured, found, err := servicepriority.Load()
	if err != nil {
		found = false
	}
	return composeProcessPriorityStatus(obs, configured, found)
}

// composeProcessPriorityStatus combines the observed mode with the configured
// one into the wire status.
func composeProcessPriorityStatus(obs processPriorityObservation, configured servicepriority.Mode, haveConfigured bool) *afclient.DaemonProcessPriorityStatus {
	status := &afclient.DaemonProcessPriorityStatus{
		Mode:     string(obs.mode),
		Evidence: obs.evidence,
		Warning:  obs.warning,
	}
	if obs.mode == "" {
		status.Mode = processPriorityUnknown
	}
	if !haveConfigured {
		return status
	}
	status.ConfiguredMode = string(configured)
	if obs.mode != "" && configured != obs.mode {
		status.Warning = fmt.Sprintf("installed setting is %s but the running daemon is %s; restart the daemon to apply it", configured, obs.mode)
	}
	return status
}
