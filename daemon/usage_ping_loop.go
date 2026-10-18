package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"github.com/RenseiAI/donmai/internal/usageping"
)

// usage_ping_loop.go — the daemon-side weekly scheduler for the opt-in
// anonymous usage ping.
//
// The loop starts with the daemon and ticks daily; each tick checks the
// live config (so `host set` takes effect without a restart) and sends
// at most one attempt per seven days. The last-attempt time is persisted
// to the state file BEFORE transmission so response loss or a restart
// cannot cause a retry storm. No request at all is made when disabled;
// disabling cancels queued work and prevents future transmission.
//
// The update checker is untouched: this loop sends only the approved
// three-field payload to the operator-configured endpoint, never to any
// default infrastructure.

// usagePingState is the on-disk record: the last attempt time. Consent
// itself lives in daemon.yaml; this file only paces retries.
type usagePingState struct {
	LastAttempt time.Time `json:"lastAttempt,omitempty"`
}

// usagePingLoop owns the background goroutine. Nil-safe: a daemon that
// never starts the loop stops nothing.
type usagePingLoop struct {
	statePath string
	version   func() string
	config    func() *Config
	sender    func(endpoint string) *usageping.Sender
	now       func() time.Time
}

// startUsagePingLoop launches the weekly scheduler until ctx ends. The
// first check runs immediately so a host that was due while stopped
// reports promptly; later checks run once a day. A nil config getter
// disables the loop (tests, embedders that never wire it).
func (d *Daemon) startUsagePingLoop(ctx context.Context) {
	getter := func() *Config { return d.Config() }
	loop := &usagePingLoop{
		statePath: DefaultUsagePingStatePath(),
		version:   d.EffectiveVersion,
		config:    getter,
		sender:    func(endpoint string) *usageping.Sender { return &usageping.Sender{Endpoint: endpoint} },
		now:       time.Now,
	}
	go loop.run(ctx, 24*time.Hour)
}

// run is the blocking loop body: one immediate check, then one per
// interval. The interval is a parameter so tests observe ticks
// without waiting a day.
func (l *usagePingLoop) run(ctx context.Context, interval time.Duration) {
	if l == nil {
		return
	}
	l.check(ctx)
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			l.check(ctx)
		}
	}
}

// check sends one ping when consent is live and an attempt is due.
// Every early return is deliberate: disabled, unconfigured, or
// not-yet-due all send nothing and touch nothing.
func (l *usagePingLoop) check(ctx context.Context) {
	if l == nil || l.config == nil {
		return
	}
	cfg := l.config()
	if cfg == nil || !cfg.UsagePingEnabled() {
		return
	}
	consent := cfg.UsagePingConsent()
	version := ""
	if l.version != nil {
		version = l.version()
	}
	ping := usageping.Build(consent, version)
	if ping == nil {
		return
	}
	now := time.Now().UTC()
	if l.now != nil {
		now = l.now().UTC()
	}
	state, err := l.loadState()
	if err != nil {
		slog.Warn("usage-ping: read state; skipping this round", "error", err)
		return
	}
	if !state.LastAttempt.IsZero() && now.Before(state.LastAttempt.Add(usageping.PingInterval)) {
		return
	}
	// Persist BEFORE transmission: a lost response or a restart must
	// not cause a retry storm.
	if err := l.storeState(usagePingState{LastAttempt: now}); err != nil {
		slog.Warn("usage-ping: persist attempt; skipping send", "error", err)
		return
	}
	sender := l.sender(consent.Endpoint)
	if sender == nil {
		sender = &usageping.Sender{Endpoint: consent.Endpoint}
	}
	sendCtx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	if err := sender.Send(sendCtx, ping); err != nil {
		slog.Info("usage-ping: attempt failed; next try in a week", "error", err)
		return
	}
	slog.Info("usage-ping: weekly count sent")
}

// loadState reads the last-attempt record. A missing file means no
// attempt yet; a corrupt file is reported (not silently ignored) and
// the round is skipped.
func (l *usagePingLoop) loadState() (usagePingState, error) {
	var state usagePingState
	data, err := os.ReadFile(l.statePath) //nolint:gosec // intentional state file
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return state, nil
		}
		return state, fmt.Errorf("read %s: %w", l.statePath, err)
	}
	if err := json.Unmarshal(data, &state); err != nil {
		return state, fmt.Errorf("parse %s: %w", l.statePath, err)
	}
	return state, nil
}

// storeState atomically records the last-attempt time.
func (l *usagePingLoop) storeState(state usagePingState) error {
	data, err := json.Marshal(state)
	if err != nil {
		return fmt.Errorf("encode usage-ping state: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(l.statePath), 0o700); err != nil {
		return fmt.Errorf("create state dir: %w", err)
	}
	tmp := l.statePath + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return fmt.Errorf("write temp state: %w", err)
	}
	if err := os.Rename(tmp, l.statePath); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("rename temp state: %w", err)
	}
	return nil
}

// cancelUsagePing clears any queued attempt state. Called when the
// operator disables the ping so no stale schedule survives.
func cancelUsagePing(statePath string) {
	_ = os.Remove(statePath) //nolint:gosec // state path is daemon-owned under the brand state dir
}
