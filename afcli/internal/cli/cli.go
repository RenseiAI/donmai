// Package cli holds small helpers shared across the afcli command families.
//
// It lives under afcli/internal so it is importable by afcli and its command
// sub-packages (afcli/linearcmd, …) but NOT by external embedders — the
// embedder surface stays exactly RegisterCommands + Config. These helpers were
// hoisted out of afcli/linear.go when the Linear command family moved to
// afcli/linearcmd, so both the moved package and the remaining afcli commands
// (admin, github, logs) share one copy rather than duplicating them.
package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
)

// WriteJSON encodes v as indented JSON to w.
func WriteJSON(w io.Writer, v any) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

// APIKey resolves the Linear API key from the environment.
// Accepts both LINEAR_API_KEY and LINEAR_ACCESS_TOKEN.
func APIKey() string {
	if v := os.Getenv("LINEAR_API_KEY"); v != "" {
		return v
	}
	return os.Getenv("LINEAR_ACCESS_TOKEN")
}

// UserError returns an error with an optional remediation hint on a second line.
// Use this for validation failures that should print a clean user-facing message.
func UserError(msg, hint string) error {
	if hint != "" {
		return fmt.Errorf("%s\n\nHint: %s", msg, hint)
	}
	return errors.New(msg)
}

// RunGitCommand executes a git subcommand and returns its trimmed stdout.
func RunGitCommand(args ...string) (string, error) {
	out, err := exec.Command("git", args...).Output() //nolint:gosec
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}
