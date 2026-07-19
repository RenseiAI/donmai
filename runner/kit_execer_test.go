package runner

import (
	"os"
	"runtime"
	"testing"
)

func TestShellExecer_ChildEnvSanitized(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shellExecer uses sh; skip on windows")
	}
	t.Setenv("ATTACH_TOKEN", "parent-secret")
	t.Setenv("ATTACH_TOKEN_FILE", "/parent/token")
	t.Setenv("ATTACH_URL", "wss://parent.invalid/v1/rooms/room-1")

	report := t.TempDir() + "/env-report.txt"
	execer := shellExecer{baseEnv: map[string]string{
		"SAFE_KIT_ENV":      "base",
		"ATTACH_TOKEN":      "base-secret",
		"ATTACH_TOKEN_FILE": "/base/token",
		"ATTACH_URL":        "wss://base.invalid/v1/rooms/room-1",
	}}
	code, err := execer.Exec(t.Context(), t.TempDir(),
		`status=leaked; if [ "${ATTACH_TOKEN+x}${ATTACH_TOKEN_FILE+x}${ATTACH_URL+x}" = "" ] && [ "$SAFE_KIT_ENV" = "command" ]; then status=clean; fi; printf '%s' "$status" > "$REPORT_PATH"`,
		map[string]string{
			"REPORT_PATH":       report,
			"SAFE_KIT_ENV":      "command",
			"ATTACH_TOKEN":      "command-secret",
			"ATTACH_TOKEN_FILE": "/command/token",
			"ATTACH_URL":        "wss://command.invalid/v1/rooms/room-1",
		})
	if err != nil || code != 0 {
		t.Fatalf("Exec: code=%d err=%v", code, err)
	}
	body, err := os.ReadFile(report) //nolint:gosec // report is a test-owned temp file
	if err != nil {
		t.Fatalf("read env report: %v", err)
	}
	if got, want := string(body), "clean"; got != want {
		t.Fatalf("kit child env report = %q, want %q", got, want)
	}
}
