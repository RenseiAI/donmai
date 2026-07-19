package orchestrator

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/RenseiAI/donmai/internal/linear"
)

func TestProviderDispatcher_ChildEnvSanitized(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("fake provider CLI uses /bin/sh; skip on windows")
	}
	t.Setenv("ATTACH_TOKEN", "parent-secret")
	t.Setenv("ATTACH_TOKEN_FILE", "/parent/token")
	t.Setenv("ATTACH_URL", "wss://parent.invalid/v1/rooms/room-1")

	dir := t.TempDir()
	report := filepath.Join(dir, "env-report.txt")
	providerBin := filepath.Join(dir, "claude")
	script := "#!/bin/sh\n" +
		"status=leaked\n" +
		"if [ \"${ATTACH_TOKEN+x}${ATTACH_TOKEN_FILE+x}${ATTACH_URL+x}\" = \"\" ]; then status=clean; fi\n" +
		"printf '%s:%s:%s\\n' \"$status\" \"$LINEAR_ISSUE_ID\" \"$LINEAR_ISSUE_IDENTIFIER\" > " + report + "\n"
	if err := os.WriteFile(providerBin, []byte(script), 0o600); err != nil { //nolint:gosec // test fixture
		t.Fatalf("write fake provider: %v", err)
	}
	if err := os.Chmod(providerBin, 0o700); err != nil { //nolint:gosec // test fixture needs exec bit
		t.Fatalf("chmod fake provider: %v", err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))

	issue := linear.Issue{ID: "issue-id", Identifier: "ENG-42", Title: "secure dispatch"}
	issue.Project.Name = "Project"
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	if _, err := (&providerDispatcher{}).Dispatch(ctx, issue, Config{}); err != nil {
		t.Fatalf("Dispatch: %v", err)
	}

	deadline := time.Now().Add(5 * time.Second)
	for {
		body, err := os.ReadFile(report) //nolint:gosec // report is a test-owned temp file
		if err == nil {
			if got, want := strings.TrimSpace(string(body)), "clean:issue-id:ENG-42"; got != want {
				t.Fatalf("provider env report = %q, want %q", got, want)
			}
			return
		}
		if !os.IsNotExist(err) {
			t.Fatalf("read env report: %v", err)
		}
		if time.Now().After(deadline) {
			t.Fatal("timed out waiting for provider env report")
		}
		time.Sleep(10 * time.Millisecond)
	}
}
