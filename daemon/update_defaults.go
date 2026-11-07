package daemon

import (
	"encoding/json"
	"fmt"
	"net/url"
	"strings"
)

// UpdateOrigin names which layer supplies one effective update value.
//
// The union is additive: a heartbeat reply that omits the update-defaults
// member leaves every layer untouched, so an older server keeps working
// byte-identically. Each active field resolves independently:
//
//	non-empty explicit host-local value, then non-null system snapshot,
//	then the compiled binary default.
//
// A system clear removes only the system layer for the named field. An
// absent member, an unavailable reply, or malformed data preserves the
// last-known-good system layer with a typed diagnostic and never triggers
// an update. A fresh process without a successful snapshot uses its local
// and binary configuration. The snapshot is in-memory only and is never
// written into the local config file.
type UpdateOrigin string

const (
	// UpdateOriginLocal means the effective value comes from the explicit
	// host-local configuration file.
	UpdateOriginLocal UpdateOrigin = "local"
	// UpdateOriginSystem means the effective value comes from the
	// platform-provided system snapshot carried on the heartbeat reply.
	UpdateOriginSystem UpdateOrigin = "system"
	// UpdateOriginDefault means neither layer names a value, so the
	// compiled binary default applies.
	UpdateOriginDefault UpdateOrigin = "default"
)

// SystemUpdateDefaults is the in-memory snapshot of platform-provided
// update defaults. A nil pointer means no system value for that field:
// either never received or explicitly cleared. Non-nil always holds a
// validated, trimmed value. Unknown or reserved members (signing trust,
// release URLs, executable content) are never represented here and can
// never change verification behavior.
type SystemUpdateDefaults struct {
	Channel *string
	CDNBase *string
}

// UpdateDefaultsError is the typed diagnostic recorded when a heartbeat
// reply carries malformed update defaults. The last-known-good snapshot
// is preserved; the beat itself still succeeds.
type UpdateDefaultsError struct {
	Field  string
	Reason string
}

func (e *UpdateDefaultsError) Error() string {
	if e == nil {
		return ""
	}
	if e.Field == "" {
		return fmt.Sprintf("update defaults: %s", e.Reason)
	}
	return fmt.Sprintf("update defaults %s: %s", e.Field, e.Reason)
}

// isValidUpdateChannel reports whether s names a known release channel.
func isValidUpdateChannel(s string) bool {
	switch UpdateChannel(s) {
	case ChannelStable, ChannelBeta, ChannelMain:
		return true
	default:
		return false
	}
}

// isValidUpdateCDNBase reports whether s names a usable update origin.
// Empty is never valid here: callers normalize empty to a clear before
// validation. A non-empty value must be an http(s) URL with a host.
func isValidUpdateCDNBase(s string) bool {
	trimmed := strings.TrimSpace(s)
	if trimmed == "" {
		return false
	}
	u, err := url.Parse(trimmed)
	if err != nil {
		return false
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return false
	}
	return strings.TrimSpace(u.Host) != ""
}

// resolveEffectiveChannel implements the per-field precedence for the
// release channel: explicit host-local, then system snapshot, then the
// compiled default.
func resolveEffectiveChannel(local UpdateChannel, system *string) (UpdateChannel, UpdateOrigin) {
	if strings.TrimSpace(string(local)) != "" {
		return local, UpdateOriginLocal
	}
	if system != nil && strings.TrimSpace(*system) != "" {
		return UpdateChannel(strings.TrimSpace(*system)), UpdateOriginSystem
	}
	return ChannelStable, UpdateOriginDefault
}

// resolveEffectiveCDNBase implements the per-field precedence for the
// update origin: explicit host-local, then system snapshot, then the
// compiled default.
func resolveEffectiveCDNBase(local string, system *string) (string, UpdateOrigin) {
	if strings.TrimSpace(local) != "" {
		return strings.TrimSpace(local), UpdateOriginLocal
	}
	if system != nil {
		return *system, UpdateOriginSystem
	}
	return effectiveUpdateCDNBase(AutoUpdateConfig{}), UpdateOriginDefault
}

// applySystemUpdateDefaults consumes one heartbeat reply's update-defaults
// member. A nil raw means the server omitted the member (older server):
// the current snapshot is left untouched. A JSON null clears the whole
// system layer. An object applies per-field presence: an absent member
// leaves that field's layer untouched, an explicit null clears only that
// field, a valid string sets it, and any other shape preserves that
// field's last-good value with a typed diagnostic. Unknown members are
// ignored so reserved trust fields can never change verification.
func (d *Daemon) applySystemUpdateDefaults(raw json.RawMessage) {
	if d == nil {
		return
	}
	if raw == nil {
		return
	}
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" {
		return
	}
	if trimmed == "null" {
		d.systemUpdateMu.Lock()
		d.systemUpdate = SystemUpdateDefaults{}
		d.systemUpdateDiag = ""
		d.systemUpdateMu.Unlock()
		return
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(raw, &obj); err != nil {
		d.systemUpdateMu.Lock()
		d.systemUpdateDiag = (&UpdateDefaultsError{Field: "updateDefaults", Reason: fmt.Sprintf("malformed: %v", err)}).Error()
		d.systemUpdateMu.Unlock()
		return
	}
	d.systemUpdateMu.Lock()
	defer d.systemUpdateMu.Unlock()
	if channelRaw, ok := obj["channel"]; ok {
		if strings.TrimSpace(string(channelRaw)) == "null" {
			d.systemUpdate.Channel = nil
		} else {
			var s string
			if err := json.Unmarshal(channelRaw, &s); err != nil {
				d.systemUpdateDiag = (&UpdateDefaultsError{Field: "channel", Reason: fmt.Sprintf("malformed: %v", err)}).Error()
			} else {
				switch {
				case strings.TrimSpace(s) == "":
					d.systemUpdate.Channel = nil
				case !isValidUpdateChannel(strings.TrimSpace(s)):
					d.systemUpdateDiag = (&UpdateDefaultsError{Field: "channel", Reason: fmt.Sprintf("unknown channel %q", s)}).Error()
				default:
					v := strings.TrimSpace(s)
					d.systemUpdate.Channel = &v
				}
			}
		}
	}
	if baseRaw, ok := obj["cdnBase"]; ok {
		if strings.TrimSpace(string(baseRaw)) == "null" {
			d.systemUpdate.CDNBase = nil
		} else {
			var s string
			if err := json.Unmarshal(baseRaw, &s); err != nil {
				d.systemUpdateDiag = (&UpdateDefaultsError{Field: "cdnBase", Reason: fmt.Sprintf("malformed: %v", err)}).Error()
			} else {
				switch {
				case strings.TrimSpace(s) == "":
					d.systemUpdate.CDNBase = nil
				case !isValidUpdateCDNBase(s):
					d.systemUpdateDiag = (&UpdateDefaultsError{Field: "cdnBase", Reason: fmt.Sprintf("malformed origin %q", s)}).Error()
				default:
					v := strings.TrimSpace(s)
					d.systemUpdate.CDNBase = &v
				}
			}
		}
	}
}

// SystemUpdateSnapshot returns a copy of the current in-memory system
// snapshot. Nil pointers mean no system value for that field.
func (d *Daemon) SystemUpdateSnapshot() SystemUpdateDefaults {
	if d == nil {
		return SystemUpdateDefaults{}
	}
	d.systemUpdateMu.Lock()
	defer d.systemUpdateMu.Unlock()
	out := SystemUpdateDefaults{}
	if d.systemUpdate.Channel != nil {
		v := *d.systemUpdate.Channel
		out.Channel = &v
	}
	if d.systemUpdate.CDNBase != nil {
		v := *d.systemUpdate.CDNBase
		out.CDNBase = &v
	}
	return out
}

// SystemUpdateDiagnostic returns the last typed diagnostic recorded while
// consuming system update defaults. Empty means the last consumption
// succeeded or no consumption has been attempted.
func (d *Daemon) SystemUpdateDiagnostic() string {
	if d == nil {
		return ""
	}
	d.systemUpdateMu.Lock()
	defer d.systemUpdateMu.Unlock()
	return d.systemUpdateDiag
}

// EffectiveUpdateChannel resolves the release channel through the
// production precedence: explicit host-local, then system snapshot, then
// the compiled default. It also reports which layer supplied the value.
func (d *Daemon) EffectiveUpdateChannel() (UpdateChannel, UpdateOrigin) {
	var local UpdateChannel
	if cfg := d.Config(); cfg != nil {
		local = cfg.AutoUpdate.Channel
	}
	snap := d.SystemUpdateSnapshot()
	return resolveEffectiveChannel(local, snap.Channel)
}

// EffectiveUpdateCDNBase resolves the update origin through the production
// precedence: explicit host-local, then system snapshot, then the compiled
// default. It also reports which layer supplied the value.
func (d *Daemon) EffectiveUpdateCDNBase() (string, UpdateOrigin) {
	var local string
	if cfg := d.Config(); cfg != nil {
		local = cfg.AutoUpdate.CDNBase
	}
	snap := d.SystemUpdateSnapshot()
	return resolveEffectiveCDNBase(local, snap.CDNBase)
}

// EffectiveAutoUpdateConfig returns the local auto-update block with the
// two system-driven fields resolved through the production precedence.
// Signing trust, schedule, and timeouts always come from the local file:
// the system layer can never change verification behavior.
func (d *Daemon) EffectiveAutoUpdateConfig() AutoUpdateConfig {
	var local AutoUpdateConfig
	if cfg := d.Config(); cfg != nil {
		local = cfg.AutoUpdate
	}
	snap := d.SystemUpdateSnapshot()
	channel, _ := resolveEffectiveChannel(local.Channel, snap.Channel)
	base, _ := resolveEffectiveCDNBase(local.CDNBase, snap.CDNBase)
	local.Channel = channel
	local.CDNBase = base
	return local
}
