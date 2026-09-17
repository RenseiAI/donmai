package mcpheaders

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestBuildHelperCommandForOS(t *testing.T) {
	tests := []struct{ goos, executable, tokenFile, want string }{
		{"linux", "/opt/Rensei Agent/donmai's", "/tmp/session token", `'` + `/opt/Rensei Agent/donmai` + `'"'"'` + `s' mcp gateway-headers --token-file '/tmp/session token'`},
		{"windows", `C:\Program Files\Rensei\rensei.exe`, `C:\Users\A B\token`, `"C:\Program Files\Rensei\rensei.exe" mcp gateway-headers --token-file "C:\Users\A B\token"`},
	}
	for _, tc := range tests {
		got, err := buildHelperCommandForOS(tc.goos, tc.executable, tc.tokenFile)
		if err != nil {
			t.Fatalf("%s: %v", tc.goos, err)
		}
		if got != tc.want {
			t.Fatalf("%s command = %q, want %q", tc.goos, got, tc.want)
		}
		if strings.Contains(got, "Bearer") {
			t.Fatalf("%s command contains credential syntax: %q", tc.goos, got)
		}
	}
}

func TestBuildHelperCommandRejectsNonCanonicalPaths(t *testing.T) {
	for _, path := range []string{"relative", t.TempDir() + "/x/../token", string([]byte{'/', 'x', 0, 'y'})} {
		if _, err := BuildHelperCommand(path, "/tmp/token"); err == nil {
			t.Fatalf("BuildHelperCommand(%q) succeeded", path)
		}
	}
}

func TestReadAuthorizationJSONRereadsAtomicRotation(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "token")
	writePrivate(t, path, "launch-token\n")
	assertReadJSON(t, path, `{"Authorization":"Bearer launch-token"}`)
	replacement := filepath.Join(dir, "replacement")
	writePrivate(t, replacement, "refreshed-token")
	if err := os.Rename(replacement, path); err != nil {
		t.Fatal(err)
	}
	assertReadJSON(t, path, `{"Authorization":"Bearer refreshed-token"}`)
}

func TestReadAuthorizationJSONRejectsInvalidFilesWithoutLeaking(t *testing.T) {
	dir := t.TempDir()
	sentinel := "SECRET-SENTINEL"
	tests := []struct {
		name, content string
		mode          os.FileMode
	}{
		{"blank", " \n", 0o600},
		{"interior whitespace", "abc def", 0o600},
		{"control", "abc\x1fdef", 0o600},
		{"del", "abc\x7fdef", 0o600},
		{"non ascii", "abcé", 0o600},
		{"non private", sentinel, 0o644},
		{"oversized", strings.Repeat("x", MaxBearerFileBytes+1), 0o600},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(dir, strings.ReplaceAll(tc.name, " ", "-"))
			if err := os.WriteFile(path, []byte(tc.content), tc.mode); err != nil {
				t.Fatal(err)
			}
			got, err := ReadAuthorizationJSON(path)
			if err == nil {
				t.Fatalf("got %q, want error", got)
			}
			if strings.Contains(err.Error(), sentinel) {
				t.Fatalf("error leaked sentinel: %v", err)
			}
		})
	}
	missing := filepath.Join(dir, "missing")
	if _, err := ReadAuthorizationJSON(missing); err == nil {
		t.Fatal("missing file succeeded")
	}
	link := filepath.Join(dir, "link")
	if runtime.GOOS != "windows" {
		writePrivate(t, filepath.Join(dir, "target"), "token")
		if err := os.Symlink(filepath.Join(dir, "target"), link); err != nil {
			t.Fatal(err)
		}
		if _, err := ReadAuthorizationJSON(link); err == nil {
			t.Fatal("symlink succeeded")
		}
	}
	if _, err := ReadAuthorizationJSON(dir); err == nil {
		t.Fatal("directory succeeded")
	}
}

func assertReadJSON(t *testing.T, path, want string) {
	t.Helper()
	got, err := ReadAuthorizationJSON(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != want {
		t.Fatalf("JSON = %s, want %s", got, want)
	}
}

func writePrivate(t *testing.T, path, value string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(value), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		t.Fatal(err)
	}
}
