package daemon

import (
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
)

// Persisted policy: one file per registration under ObservationPolicyDir,
// holding exactly the last applied observations.policy payload. The file
// name carries the registration key, so a file whose content is corrupt
// still names the registration it must fail closed for.
const (
	observationPolicyPrimaryFile = "policy-primary.json"
	observationPolicyFilePrefix  = "policy-"
	observationPolicyFileSuffix  = ".json"
)

// writeObservationPolicyFile writes the payload into the temp file; it is
// a seam so tests can fail a write midway.
var writeObservationPolicyFile = func(f *os.File, data []byte) error {
	_, err := f.Write(data)
	return err
}

func observationPolicyFileName(registration string) string {
	if registration == primaryObservationRegistration {
		return observationPolicyPrimaryFile
	}
	return observationPolicyFilePrefix + hex.EncodeToString([]byte(registration)) + observationPolicyFileSuffix
}

func observationPolicyRegistration(name string) (string, bool) {
	if name == observationPolicyPrimaryFile {
		return primaryObservationRegistration, true
	}
	core, ok := strings.CutPrefix(name, observationPolicyFilePrefix)
	if !ok {
		return "", false
	}
	core, ok = strings.CutSuffix(core, observationPolicyFileSuffix)
	raw, err := hex.DecodeString(core)
	if !ok || err != nil || len(raw) == 0 {
		return "", false
	}
	return string(raw), true
}

// persistObservationPolicy atomically replaces registration's persisted
// policy: write a temp file, sync, rename. A failure leaves the previous
// file intact and removes the temp file.
func (d *Daemon) persistObservationPolicy(registration string, raw json.RawMessage) error {
	dir := d.opts.ObservationPolicyDir
	if dir == "" {
		return nil
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("persist policy: %w", err)
	}
	tmp, err := os.CreateTemp(dir, ".policy-*.tmp")
	if err != nil {
		return fmt.Errorf("persist policy: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			_ = tmp.Close()
			_ = os.Remove(tmp.Name())
		}
	}()
	if err := writeObservationPolicyFile(tmp, raw); err != nil {
		return fmt.Errorf("persist policy: write: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		return fmt.Errorf("persist policy: sync: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("persist policy: close: %w", err)
	}
	if err := os.Rename(tmp.Name(), filepath.Join(dir, observationPolicyFileName(registration))); err != nil {
		return fmt.Errorf("persist policy: rename: %w", err)
	}
	committed = true
	return nil
}

// loadObservationPolicies restores every persisted policy at boot, so a
// registration that was ever governed is enforced from its last-known
// policy until a fresh push. A file that cannot be read or decoded fails
// closed: the registration gets an empty policy, which disables every
// login. A registration with no file keeps the never-governed
// pass-through. An unreadable directory fails the primary registration
// closed, since its files cannot be told apart.
func (d *Daemon) loadObservationPolicies() {
	dir := d.opts.ObservationPolicyDir
	if dir == "" {
		return
	}
	entries, err := os.ReadDir(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return
	}
	if err != nil {
		slog.Warn("daemon: observation policy directory unreadable; subscription logins disabled until a fresh push", "err", err)
		d.setObservationPolicy(primaryObservationRegistration, map[observationSlotKey]observationPolicyItem{})
		return
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		slog.Warn("daemon: observation policy directory unreadable; subscription logins disabled until a fresh push", "err", err)
		d.setObservationPolicy(primaryObservationRegistration, map[observationSlotKey]observationPolicyItem{})
		return
	}
	defer func() { _ = root.Close() }()
	for _, e := range entries {
		registration, ok := observationPolicyRegistration(e.Name())
		if !ok || e.IsDir() {
			continue
		}
		raw, err := root.ReadFile(e.Name())
		var items map[observationSlotKey]observationPolicyItem
		if err == nil {
			items, err = decodeObservationPolicy(raw)
		}
		if err != nil {
			slog.Warn("daemon: persisted observation policy unreadable; subscription logins disabled until a fresh push",
				"file", e.Name(), "err", err)
			items = map[observationSlotKey]observationPolicyItem{}
		}
		d.setObservationPolicy(registration, items)
	}
}
