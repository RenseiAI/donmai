package afcli

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// pidDir returns the path to the PID directory (~/.config/agentfactory/pids/),
// creating it if it does not exist.
func pidDir() (string, error) {
	configDir, err := os.UserConfigDir()
	if err != nil {
		return "", fmt.Errorf("user config dir: %w", err)
	}
	dir := filepath.Join(configDir, "agentfactory", "pids")
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return "", fmt.Errorf("create pid dir: %w", err)
	}
	return dir, nil
}

// savePID writes the given PID to ~/.config/agentfactory/pids/<name>.pid.
func savePID(name string, pid int) error {
	dir, err := pidDir()
	if err != nil {
		return err
	}
	path := filepath.Join(dir, name+".pid")
	data := []byte(strconv.Itoa(pid) + "\n")
	if err := os.WriteFile(path, data, 0o600); err != nil { //nolint:gosec // path is constructed from config dir + known name + ".pid"
		return fmt.Errorf("write pid file: %w", err)
	}
	return nil
}

// loadPID reads the PID from ~/.config/agentfactory/pids/<name>.pid.
// Returns an error if the file does not exist or contains invalid data.
func loadPID(name string) (int, error) {
	dir, err := pidDir()
	if err != nil {
		return 0, err
	}
	path := filepath.Join(dir, name+".pid")
	data, err := os.ReadFile(path) //nolint:gosec // path is constructed from config dir + known name
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return 0, fmt.Errorf("no pid file for %s", name)
		}
		return 0, fmt.Errorf("read pid file: %w", err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil {
		return 0, fmt.Errorf("invalid pid in %s: %w", path, err)
	}
	return pid, nil
}

// removePIDFile deletes the PID file for the given name, ignoring
// errors if the file does not exist.
func removePIDFile(name string) error {
	dir, err := pidDir()
	if err != nil {
		return err
	}
	path := filepath.Join(dir, name+".pid")
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove pid file: %w", err)
	}
	return nil
}
