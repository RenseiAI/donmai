package daemon

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"github.com/fsnotify/fsnotify"
)

// startYamlWatcher watches the daemon's config file for direct edits and
// hot-reloads `projects[]` into the running daemon (in-memory config +
// spawner allowlist). The heartbeat goroutine's GetAllowlist callback
// then picks up the new entries on the next beat, which lets the
// platform emit a daemon.allowlist.reported audit event without operator
// action.
//
// Phase 3b of platform/runs/2026-05-18-daemon-config-sync-DESIGN.md.
//
// Watcher scope:
//   - Watches the file's PARENT directory (not the file directly) because
//     atomic-write tools like `os.Rename` swap the inode out from under a
//     file-level watcher — most editors do this. Watching the directory
//     and filtering by basename catches both the rename and direct writes.
//   - Filters to Write + Create + Rename events (not Chmod / Remove —
//     those are noisy and not actionable).
//
// Echo suppression:
//   - The daemon's own mutation-apply path (mutation_apply.go) writes the
//     yaml then calls SetProjects on the spawner; the in-memory config is
//     already updated. The watcher's reload reads the same content back
//     and finds no diff to apply, so the cost is one file read per write.
//     No timestamp/checksum gate needed.
//
// Lifecycle:
//   - Returns a stop function the caller invokes during Daemon.Stop.
//   - Errors during initial setup are returned synchronously; runtime
//     errors are logged at WARN and the watcher continues.
func startYamlWatcher(
	ctx context.Context,
	configPath string,
	onChange func(*Config),
) (stop func(), err error) {
	if configPath == "" {
		return nil, errors.New("yaml-watcher: configPath empty")
	}
	absPath, err := filepath.Abs(configPath)
	if err != nil {
		return nil, err
	}
	dir := filepath.Dir(absPath)
	base := filepath.Base(absPath)

	w, err := fsnotify.NewWatcher()
	if err != nil {
		return nil, err
	}
	if err := w.Add(dir); err != nil {
		_ = w.Close()
		return nil, err
	}

	// Coalesce rapid bursts of events (editors emit several per save).
	// 250ms is short enough that operator edits feel instant but long
	// enough that a vim :w (rename + write + chmod) only triggers one
	// reload.
	const coalesceWindow = 250 * time.Millisecond

	go func() {
		var pending *time.Timer
		fire := func() {
			cfg, err := LoadConfig(configPath)
			if err != nil {
				slog.Warn("[yaml-watcher] reload failed",
					"path", configPath, "err", err.Error())
				return
			}
			if cfg == nil {
				// File deleted while we were watching. Don't push an empty
				// config — leave the daemon's in-memory state intact and
				// let the next operator action restore the file.
				slog.Warn("[yaml-watcher] config missing", "path", configPath)
				return
			}
			onChange(cfg)
		}

		for {
			select {
			case <-ctx.Done():
				_ = w.Close()
				return
			case event, ok := <-w.Events:
				if !ok {
					return
				}
				if filepath.Base(event.Name) != base {
					continue
				}
				if event.Op&(fsnotify.Write|fsnotify.Create|fsnotify.Rename) == 0 {
					continue
				}
				if pending != nil {
					pending.Stop()
				}
				pending = time.AfterFunc(coalesceWindow, fire)
			case wErr, ok := <-w.Errors:
				if !ok {
					return
				}
				slog.Warn("[yaml-watcher] error", "err", wErr.Error())
			}
		}
	}()

	// Best-effort: confirm the file exists at startup so a misconfigured
	// path fails loudly rather than silently never firing.
	if _, statErr := os.Stat(absPath); statErr != nil {
		slog.Warn("[yaml-watcher] config path missing at startup",
			"path", absPath, "err", statErr.Error())
	}

	stop = func() {
		_ = w.Close()
	}
	return stop, nil
}
