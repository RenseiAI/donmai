package codeintel

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestArchAssessRequireDiff(t *testing.T) {
	origView, origDiff := runGhPRView, runGhPRDiff
	t.Cleanup(func() { runGhPRView, runGhPRDiff = origView, origDiff })
	t.Setenv("DONMAI_ARCH_BIN", "")
	t.Setenv("PATH", t.TempDir())
	const metadata = `{"title":"Change","body":"","files":[{"path":"src/auth/login.ts","additions":1,"deletions":0}]}`
	const patch = "diff --git a/src/auth/login.ts b/src/auth/login.ts\n@@ -0,0 +1 @@\n+const r: Result<User, Error> = ok(user)\n"
	tests := []struct {
		name    string
		view    string
		patch   string
		viewErr error
		diffErr error
		wantErr string
	}{
		{name: "complete diff", view: metadata, patch: patch},
		{name: "missing gh", viewErr: ErrDiffFetchUnavailable, wantErr: "gh CLI not found"},
		{name: "authentication failure", viewErr: errors.New("authentication required"), wantErr: "authentication required"},
		{name: "malformed metadata", view: "{", wantErr: "decode gh pr view"},
		{name: "absent file list", view: `{}`, wantErr: "missing the changed-file list"},
		{name: "failed patches", view: metadata, diffErr: errors.New("patch download failed"), wantErr: "patch download failed"},
		{name: "empty patches", view: metadata, wantErr: "patch sections do not match"},
		{name: "wrong file", view: metadata, patch: "diff --git a/other.go b/other.go\n+wrong\n", wantErr: "missing patch section"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			runGhPRView = func(_ context.Context, ref string) ([]byte, error) {
				if ref != "https://github.com/org/repo/pull/7" {
					t.Errorf("fetch ref = %q", ref)
				}
				return []byte(tc.view), tc.viewErr
			}
			runGhPRDiff = func(_ context.Context, ref string) ([]byte, error) {
				if ref != "https://github.com/org/repo/pull/7" {
					t.Errorf("patch ref = %q", ref)
				}
				return []byte(tc.patch), tc.diffErr
			}
			out, err := New(t.TempDir()).ArchAssess(ArchAssessOptions{
				Repository: "github.com/org/repo", PrNumber: 7, RequireDiff: true,
			})
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) || out != nil {
					t.Fatalf("assessment = %v, error = %v; want no result and %q", out, err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			report, ok := out.(map[string]any)
			if !ok || report["mode"] != "native-diff-only" {
				t.Fatalf("report = %#v", out)
			}
			observations, ok := report["observations"].([]any)
			if !ok || len(observations) == 0 {
				t.Fatalf("real patch did not reach analysis: %#v", report)
			}
		})
	}
}

func TestArchAssessRequireDiffRejectsMissingTarget(t *testing.T) {
	t.Setenv("DONMAI_ARCH_BIN", "")
	t.Setenv("PATH", t.TempDir())
	for _, opts := range []ArchAssessOptions{
		{RequireDiff: true},
		{RequireDiff: true, Repository: "github.com/org/repo", PrNumber: -1},
		{RequireDiff: true, PrURL: "not-a-pr"},
	} {
		if out, err := New(t.TempDir()).ArchAssess(opts); err == nil || out != nil {
			t.Fatalf("invalid target returned result=%v error=%v", out, err)
		}
	}
}

func TestArchAssessRequireDiffRefusesLegacyBeforeExecution(t *testing.T) {
	dir := t.TempDir()
	shim := filepath.Join(dir, "arch-shim")
	if err := os.WriteFile(shim, []byte("#!/bin/sh\nprintf ran > executed\nprintf '{\"gated\":false}'\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("DONMAI_ARCH_BIN", "/bin/sh "+shim)
	if out, err := New(dir).ArchAssess(ArchAssessOptions{RequireDiff: true, PrURL: "https://github.com/org/repo/pull/7"}); err == nil || out != nil || !strings.Contains(err.Error(), "requires the native implementation") {
		t.Fatalf("legacy assessment returned result=%v error=%v", out, err)
	}
	if _, err := os.Stat(filepath.Join(dir, "executed")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("legacy shim ran: %v", err)
	}
}
