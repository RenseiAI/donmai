package worker

import (
	"bufio"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// fleetPIDEnv overrides the fleet PID file location. Primarily used in
// tests so they do not touch the user's real config directory.
const fleetPIDEnv = "AGENTFACTORY_FLEET_PID_FILE"

// FleetPIDPath returns the path to the fleet PID file. It honors
// $AGENTFACTORY_FLEET_PID_FILE when set; otherwise it derives the path
// from os.UserConfigDir as <config>/agentfactory/fleet.pids.
func FleetPIDPath() (string, error) {
	if override := strings.TrimSpace(os.Getenv(fleetPIDEnv)); override != "" {
		return override, nil
	}
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", fmt.Errorf("fleet: resolve user config dir: %w", err)
	}
	return filepath.Join(dir, "agentfactory", "fleet.pids"), nil
}

// WriteFleetPIDs writes the given PIDs, one per line, to the fleet PID
// file. The parent directory is created with 0o750 if missing and the
// file is written with 0o600.
func WriteFleetPIDs(pids []int) error {
	path, err := FleetPIDPath()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return fmt.Errorf("fleet: mkdir pid dir: %w", err)
	}

	var b strings.Builder
	for _, pid := range pids {
		b.WriteString(strconv.Itoa(pid))
		b.WriteByte('\n')
	}
	if err := os.WriteFile(path, []byte(b.String()), 0o600); err != nil {
		return fmt.Errorf("fleet: write pid file: %w", err)
	}
	return nil
}

// ReadFleetPIDs reads the fleet PID file. Returns an empty slice (nil
// error) when the file does not exist. Blank lines are skipped.
func ReadFleetPIDs() ([]int, error) {
	path, err := FleetPIDPath()
	if err != nil {
		return nil, err
	}
	f, err := os.Open(path) //nolint:gosec // path derived from user config dir or env override
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("fleet: open pid file: %w", err)
	}
	defer func() { _ = f.Close() }()

	var pids []int
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		pid, err := strconv.Atoi(line)
		if err != nil {
			return nil, fmt.Errorf("fleet: parse pid %q: %w", line, err)
		}
		pids = append(pids, pid)
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("fleet: scan pid file: %w", err)
	}
	return pids, nil
}

// RemoveFleetPIDFile deletes the fleet PID file, if present. A missing
// file is not an error.
func RemoveFleetPIDFile() error {
	path, err := FleetPIDPath()
	if err != nil {
		return err
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("fleet: remove pid file: %w", err)
	}
	return nil
}
