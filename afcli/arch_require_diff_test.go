package afcli

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"
)

// The command owns its exit codes, so exercise its real Cobra wiring in a
// child test process. A missing flag or dropped option cannot pass as success.
func TestArchRequireDiffCLI(t *testing.T) {
	if os.Getenv("DONMAI_TEST_ARCH_CHILD") == "1" {
		root := &cobra.Command{Use: "donmai", SilenceUsage: true, SilenceErrors: true}
		root.AddCommand(newArchCmd(Config{}))
		root.SetArgs([]string{"arch", "assess", "https://github.com/org/repo/pull/7", "--require-diff", "--gate-policy", "none"})
		if err := root.Execute(); err != nil {
			os.Exit(9)
		}
		os.Exit(0)
	}
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name     string
		script   string
		wantExit int
		wantText string
	}{
		{name: "missing gh", wantExit: 2, wantText: "required PR diff unavailable"},
		{name: "auth failure", script: "#!/bin/sh\necho auth-required >&2\nexit 4\n", wantExit: 2, wantText: "auth-required"},
		{name: "patch failure", script: "#!/bin/sh\nif [ \"$2\" = view ]; then\n printf '%s' '{\"title\":\"Change\",\"body\":\"\",\"files\":[{\"path\":\"src/auth/login.ts\",\"additions\":1,\"deletions\":0}]}'\nelse\n echo patch-unavailable >&2\n exit 1\nfi\n", wantExit: 2, wantText: "patch-unavailable"},
		{name: "actual diff", script: "#!/bin/sh\nif [ \"$2\" = view ]; then\n printf '%s' '{\"title\":\"Change\",\"body\":\"\",\"files\":[{\"path\":\"src/auth/login.ts\",\"additions\":1,\"deletions\":0}]}'\nelse\n printf '%s\\n' 'diff --git a/src/auth/login.ts b/src/auth/login.ts' '@@ -0,0 +1 @@' '+const r: Result<User, Error> = ok(user)'\nfi\n", wantText: `"native-diff-only"`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			if tc.script != "" {
				// gh invokes `pr view` / `pr diff`: a trusted shell reads the
				// private, non-executable `pr` fixture from this isolated cwd.
				if err := os.Symlink("/bin/sh", filepath.Join(dir, "gh")); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(dir, "pr"), []byte(strings.ReplaceAll(tc.script, `"$2"`, `"$1"`)), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, self, "-test.run=^TestArchRequireDiffCLI$")
			cmd.Dir = dir
			cmd.Env = []string{"DONMAI_TEST_ARCH_CHILD=1", "HOME=" + dir, "PATH=" + dir}
			var stdout, stderr bytes.Buffer
			cmd.Stdout, cmd.Stderr = &stdout, &stderr
			err := cmd.Run()
			gotExit := 0
			if err != nil {
				var exitErr *exec.ExitError
				if !errors.As(err, &exitErr) {
					t.Fatal(err)
				}
				gotExit = exitErr.ExitCode()
			}
			if gotExit != tc.wantExit || !strings.Contains(stdout.String()+stderr.String(), tc.wantText) {
				t.Fatalf("exit=%d want=%d stdout=%s stderr=%s", gotExit, tc.wantExit, stdout.String(), stderr.String())
			}
			if tc.wantExit != 0 && stdout.Len() != 0 {
				t.Fatalf("failed check emitted a result: %s", stdout.String())
			}
		})
	}
}
