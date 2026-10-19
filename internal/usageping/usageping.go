// Package usageping implements the opt-in anonymous weekly usage ping.
//
// The ping counts opted-in installations in public usage numbers. It is
// disabled by default and sends nothing unless the operator explicitly
// opts in during `host setup` (or via `host set`). The payload carries
// only a cryptographically random installation id minted at opt-in, the
// binary version, and the OS — never hostnames, repository names,
// organization or machine identity, IPs, or credentials.
//
// The receiver endpoint is an operations decision: the operator configures
// the HTTPS endpoint explicitly. With no endpoint configured the ping is
// inert — consent alone never dials any default infrastructure.
package usageping

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"runtime"
	"strings"
	"time"
)

// PingInterval is the minimum interval between ping attempts. At most one
// attempt is made per seven days; the next eligible time is persisted
// before transmission so response loss or a restart cannot cause a
// retry storm.
const PingInterval = 7 * 24 * time.Hour

// MaxPayloadBytes bounds the encoded ping body. The payload is three
// short strings; anything larger is a programming error, not a ping.
const MaxPayloadBytes = 4 * 1024

// Consent records the operator's opt-in choice. Enabled is false unless
// the operator explicitly answered yes; InstallationID is the random id
// minted at that moment and is empty when disabled.
type Consent struct {
	Enabled        bool
	InstallationID string
	Endpoint       string
}

// Ping is the wire payload. Exactly these three fields — no hostname,
// no repository, no organization or machine identity.
type Ping struct {
	InstallationID string `json:"installationId"`
	Version        string `json:"version"`
	OS             string `json:"os"`
}

// MintInstallationID generates a cryptographically random installation id.
// It is created only on opt-in and is not derived from any machine, host,
// or organization identifier.
func MintInstallationID() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("usageping: generate installation id: %w", err)
	}
	return hex.EncodeToString(b), nil
}

// ValidateInstallationID reports whether id looks like a minted id:
// 32 lowercase hex characters (16 random bytes).
func ValidateInstallationID(id string) bool {
	if len(id) != 32 {
		return false
	}
	for _, c := range id {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// Build constructs the ping payload for the given consent and version. It
// returns nil when consent is disabled or incomplete, so callers send no
// request at all without explicit opt-in.
func Build(consent Consent, version string) *Ping {
	if !consent.Enabled {
		return nil
	}
	if !ValidateInstallationID(consent.InstallationID) {
		return nil
	}
	if strings.TrimSpace(consent.Endpoint) == "" {
		return nil
	}
	version = strings.TrimSpace(version)
	if version == "" {
		return nil
	}
	return &Ping{
		InstallationID: consent.InstallationID,
		Version:        version,
		OS:             runtime.GOOS,
	}
}

// Encode strictly encodes the ping: exactly the three approved fields,
// rejected when oversize. Decoding uses DisallowUnknownFields on the
// receiver side; the encoder here emits only the approved shape.
func Encode(p *Ping) ([]byte, error) {
	if p == nil {
		return nil, fmt.Errorf("usageping: nil ping")
	}
	body, err := json.Marshal(p)
	if err != nil {
		return nil, fmt.Errorf("usageping: encode ping: %w", err)
	}
	if len(body) > MaxPayloadBytes {
		return nil, fmt.Errorf("usageping: ping body %d bytes exceeds %d", len(body), MaxPayloadBytes)
	}
	return body, nil
}

// Decode strictly decodes a received ping body: unknown fields and
// oversize/invalid values are rejected.
func Decode(body []byte) (*Ping, error) {
	if len(body) > MaxPayloadBytes {
		return nil, fmt.Errorf("usageping: ping body %d bytes exceeds %d", len(body), MaxPayloadBytes)
	}
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.DisallowUnknownFields()
	var p Ping
	if err := dec.Decode(&p); err != nil {
		return nil, fmt.Errorf("usageping: decode ping: %w", err)
	}
	if !ValidateInstallationID(p.InstallationID) {
		return nil, fmt.Errorf("usageping: invalid installation id")
	}
	if strings.TrimSpace(p.Version) == "" || len(p.Version) > 64 {
		return nil, fmt.Errorf("usageping: invalid version")
	}
	if strings.TrimSpace(p.OS) == "" || len(p.OS) > 32 {
		return nil, fmt.Errorf("usageping: invalid os")
	}
	// Trailing garbage after the first JSON value is rejected: a body of
	// `{...} {...}` must not decode as one ping.
	var extra json.RawMessage
	if err := dec.Decode(&extra); err != io.EOF {
		return nil, fmt.Errorf("usageping: trailing data after ping object")
	}
	return &p, nil
}

// Sender transmits pings to the configured endpoint.
type Sender struct {
	HTTPClient *http.Client
	Endpoint   string
	// AllowHTTP permits http:// endpoints. Tests only: production
	// endpoints must always be https so the installation id never
	// travels in cleartext.
	AllowHTTP bool
}

// Send POSTs one ping. Success is reported only on a 2xx response.
func (s *Sender) Send(ctx context.Context, p *Ping) error {
	if s == nil || p == nil {
		return fmt.Errorf("usageping: nil sender or ping")
	}
	endpoint := strings.TrimSpace(s.Endpoint)
	if endpoint == "" {
		return fmt.Errorf("usageping: no endpoint configured")
	}
	if !strings.HasPrefix(endpoint, "https://") {
		if !s.AllowHTTP || !strings.HasPrefix(endpoint, "http://") {
			return fmt.Errorf("usageping: endpoint must be https")
		}
	}
	body, err := Encode(p)
	if err != nil {
		return err
	}
	client := s.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("usageping: build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "donmai-usage-ping/"+p.Version)
	res, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("usageping: send ping: %w", err)
	}
	defer func() { _ = res.Body.Close() }()
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		return fmt.Errorf("usageping: ping HTTP %d", res.StatusCode)
	}
	return nil
}

// NextEligible returns the next time a ping may be attempted: last plus
// the weekly interval. The zero last means due now.
func NextEligible(last time.Time) time.Time {
	if last.IsZero() {
		return time.Time{}
	}
	return last.Add(PingInterval)
}

// Due reports whether a ping attempt is due at now given the last
// attempt time. A zero last means due now (first attempt after opt-in).
func Due(last, now time.Time) bool {
	if last.IsZero() {
		return true
	}
	return !now.Before(last.Add(PingInterval))
}
