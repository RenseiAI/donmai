package daemon

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/RenseiAI/donmai/installer/launchd"
)

func TestProcessPriorityProbe_BackgroundService(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	plistPath := filepath.Join(t.TempDir(), "daemon.plist")
	plist, err := launchd.GeneratePlistWithOptions("/usr/local/bin/af", "/tmp/o.log", "/tmp/e.log", launchd.InstallOptions{ProcessPriority: launchd.ProcessPriorityBackground})
	if err != nil {
		t.Fatalf("GeneratePlistWithOptions: %v", err)
	}
	if err := os.WriteFile(plistPath, []byte(plist), 0o600); err != nil {
		t.Fatalf("write plist: %v", err)
	}
	probe := processPriorityProbe{
		pid:           4242,
		readFile:      os.ReadFile,
		plistForLabel: func(string) (string, error) { return plistPath, nil },
		run: func(_ context.Context, name string, _ ...string) ([]byte, error) {
			switch {
			case name == "ps":
				return []byte("4\n"), nil
			case name == "launchctl":
				return []byte(`{"PID" = 4242; "Label" = "` + launchd.LaunchdLabel + `";}`), nil
			default:
				return nil, errors.New("unexpected command")
			}
		},
	}
	status := probe.inspect()
	if status.Mode != string(launchd.ProcessPriorityBackground) {
		t.Fatalf("Mode = %q, want background", status.Mode)
	}
	if status.ProcessPriority != 4 {
		t.Fatalf("ProcessPriority = %d, want 4", status.ProcessPriority)
	}
	if status.IOPolicy != "background" || status.QoSClass != "background" {
		t.Fatalf("background status = %+v", status)
	}
}

func TestProcessPriorityProbe_UtilityService(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	plistPath := filepath.Join(t.TempDir(), "daemon.plist")
	plist, err := launchd.GeneratePlistWithOptions("/usr/local/bin/af", "/tmp/o.log", "/tmp/e.log", launchd.InstallOptions{ProcessPriority: launchd.ProcessPriorityUtility})
	if err != nil {
		t.Fatalf("GeneratePlistWithOptions: %v", err)
	}
	if err := os.WriteFile(plistPath, []byte(plist), 0o600); err != nil {
		t.Fatalf("write plist: %v", err)
	}
	probe := processPriorityProbe{
		pid:           4242,
		readFile:      os.ReadFile,
		plistForLabel: func(string) (string, error) { return plistPath, nil },
		run: func(_ context.Context, name string, _ ...string) ([]byte, error) {
			switch {
			case name == "ps":
				return []byte("20\n"), nil
			case name == "launchctl":
				return []byte(`{"PID" = 4242; "Label" = "` + launchd.LaunchdLabel + `";}`), nil
			default:
				return nil, errors.New("unexpected command")
			}
		},
	}
	status := probe.inspect()
	if status.Mode != string(launchd.ProcessPriorityUtility) {
		t.Fatalf("Mode = %q, want utility", status.Mode)
	}
	if status.ProcessPriority != 20 {
		t.Fatalf("ProcessPriority = %d, want 20", status.ProcessPriority)
	}
	if status.IOPolicy != "throttled" || status.QoSClass != "utility" {
		t.Fatalf("utility status = %+v", status)
	}
	if !strings.Contains(status.Evidence, "taskpolicy -c utility") {
		t.Fatalf("utility evidence = %q, want taskpolicy wrapper", status.Evidence)
	}
}

func TestProcessPriorityProbe_DefaultWhenNotLaunchdManaged(t *testing.T) {
	probe := processPriorityProbe{
		pid:           4242,
		readFile:      os.ReadFile,
		plistForLabel: func(string) (string, error) { return "", errors.New("not expected") },
		run: func(_ context.Context, name string, _ ...string) ([]byte, error) {
			switch name {
			case "ps":
				return []byte("20\n"), nil
			case "launchctl":
				return []byte(`{"PID" = 7; "Label" = "` + launchd.LaunchdLabel + `";}`), nil
			default:
				return nil, errors.New("unexpected command")
			}
		},
	}
	status := probe.inspect()
	if status.Mode != string(launchd.ProcessPriorityDefault) {
		t.Fatalf("Mode = %q, want default", status.Mode)
	}
	if status.LaunchdMode != "" {
		t.Fatalf("LaunchdMode = %q, want empty when another service PID owns the label", status.LaunchdMode)
	}
}

func TestProcessPriorityProbe_MismatchReportsWarning(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	plistPath := filepath.Join(t.TempDir(), "daemon.plist")
	plist, err := launchd.GeneratePlistWithOptions("/usr/local/bin/af", "/tmp/o.log", "/tmp/e.log", launchd.InstallOptions{ProcessPriority: launchd.ProcessPriorityBackground})
	if err != nil {
		t.Fatalf("GeneratePlistWithOptions: %v", err)
	}
	if err := os.WriteFile(plistPath, []byte(plist), 0o600); err != nil {
		t.Fatalf("write plist: %v", err)
	}
	probe := processPriorityProbe{
		pid:           4242,
		readFile:      os.ReadFile,
		plistForLabel: func(string) (string, error) { return plistPath, nil },
		run: func(_ context.Context, name string, _ ...string) ([]byte, error) {
			switch name {
			case "ps":
				return []byte("20\n"), nil
			case "launchctl":
				return []byte(`{"PID" = 4242; "Label" = "` + launchd.LaunchdLabel + `";}`), nil
			default:
				return nil, errors.New("unexpected command")
			}
		},
	}
	status := probe.inspect()
	if status.Mode != "unknown" {
		t.Fatalf("Mode = %q, want unknown on mismatch", status.Mode)
	}
	if status.Warning == "" {
		t.Fatal("expected mismatch warning")
	}
}
