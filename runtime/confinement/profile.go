package confinement

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// profileName is a profile file name for one rendering: the harness and
// session ids reduced to a safe alphabet plus a random suffix, so two
// renderings never share a file.
func profileName(r *Resolved) string {
	return safeName(r.HarnessID) + "-" + safeName(r.SessionID) + "-" + randomSuffix() + ".sb"
}

func safeName(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
			b.WriteRune(r)
		default:
			b.WriteRune('_')
		}
		if b.Len() >= 48 {
			break
		}
	}
	if b.Len() == 0 {
		return "x"
	}
	return b.String()
}

// writeProfile writes a profile atomically with owner-only permissions.
func writeProfile(dir, name string, text []byte) (string, error) {
	tmp, err := os.CreateTemp(dir, ".profile-*")
	if err != nil {
		return "", fmt.Errorf("confinement: write profile: %w", err)
	}
	defer func() { _ = os.Remove(tmp.Name()) }()
	if _, err := tmp.Write(text); err != nil {
		_ = tmp.Close()
		return "", fmt.Errorf("confinement: write profile: %w", err)
	}
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return "", fmt.Errorf("confinement: write profile: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return "", fmt.Errorf("confinement: write profile: %w", err)
	}
	path := filepath.Join(dir, name)
	if err := os.Rename(tmp.Name(), path); err != nil {
		return "", fmt.Errorf("confinement: write profile: %w", err)
	}
	return path, nil
}
