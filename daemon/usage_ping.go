package daemon

import (
	"errors"
	"fmt"
	"strings"

	"github.com/RenseiAI/donmai/internal/statepath"
	"github.com/RenseiAI/donmai/internal/usageping"
)

// usage_ping.go — the opt-in anonymous weekly usage ping.
//
// The ping counts opted-in installations in public usage numbers. It is
// disabled by default and sends nothing unless the operator explicitly
// opts in during `host setup` (or via `host set`) AND configures an
// endpoint. With no endpoint configured the ping is inert — consent
// alone never dials any default infrastructure, and the no-telemetry
// principle holds: the update checker gains no new traffic.
//
// Payload: the random installation id minted at opt-in (not a machine,
// host, or organization identifier), the binary version, and the OS.
// No hostnames, no repository names, no IPs stored.
//
// Scheduling: at most one attempt per seven days. The next eligible
// time is persisted before transmission so response loss or a restart
// cannot cause a retry storm. Disabling cancels queued work and
// prevents future transmission.

// UsagePingConfig holds the opt-in anonymous usage-count consent in
// daemon.yaml. The zero value keeps the ping disabled.
type UsagePingConfig struct {
	// Enabled is the operator's explicit opt-in. False (the default)
	// sends no request at all.
	Enabled bool `yaml:"enabled,omitempty" json:"enabled,omitempty"`
	// InstallationID is the cryptographically random id minted at
	// opt-in. It is not derived from any machine, host, or
	// organization identifier. Empty unless opted in.
	InstallationID string `yaml:"installationId,omitempty" json:"installationId,omitempty"`
	// Endpoint is the operator-configured HTTPS receiver URL. Empty
	// means the ping is inert even when enabled: consent alone never
	// selects a destination.
	Endpoint string `yaml:"endpoint,omitempty" json:"endpoint,omitempty"`
}

// UsagePingEnabled reports whether the operator opted in AND supplied
// enough state to send: enabled, a valid installation id, and an
// endpoint. Anything less sends no request at all.
func (c *Config) UsagePingEnabled() bool {
	if c == nil {
		return false
	}
	u := c.UsagePing
	if !u.Enabled {
		return false
	}
	if !usageping.ValidateInstallationID(u.InstallationID) {
		return false
	}
	return strings.TrimSpace(u.Endpoint) != ""
}

// UsagePingConsent converts the config block to the usageping contract.
func (c *Config) UsagePingConsent() usageping.Consent {
	if c == nil {
		return usageping.Consent{}
	}
	return usageping.Consent{
		Enabled:        c.UsagePing.Enabled,
		InstallationID: c.UsagePing.InstallationID,
		Endpoint:       strings.TrimSpace(c.UsagePing.Endpoint),
	}
}

// DefaultUsagePingStatePath returns the path to the usage-ping state
// file under the brand state dir. The file records the last attempt
// time; it is separate from daemon registration, credentials, and
// auto-update state.
func DefaultUsagePingStatePath() string {
	return statepath.Resolve("usage-ping.json", "/tmp/.donmai/usage-ping.json")
}

// validateUsagePing enforces the consent invariants: a valid random id
// when enabled, and an https endpoint when one is configured. Enabled
// with an empty endpoint is valid-but-inert: the setup prompt and the
// config reference both promise that empty disables sending, and the
// scheduler stays inert via UsagePingEnabled. Consent alone never
// selects a destination.
func validateUsagePing(u UsagePingConfig) error {
	if !u.Enabled {
		return nil
	}
	if !usageping.ValidateInstallationID(u.InstallationID) {
		return errors.New("usagePing.installationId must be the random id minted at opt-in")
	}
	endpoint := strings.TrimSpace(u.Endpoint)
	if endpoint == "" {
		return nil
	}
	if !strings.HasPrefix(endpoint, "https://") {
		return fmt.Errorf("usagePing.endpoint must be https")
	}
	return nil
}
