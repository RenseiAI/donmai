package codex

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"
)

type failingStatusOwner struct {
	cmd      *exec.Cmd
	waitGate <-chan struct{}
	waited   chan struct{}
}

func (o *failingStatusOwner) wait() error {
	if o.waitGate != nil {
		<-o.waitGate
	}
	err := o.cmd.Wait()
	if o.waited != nil {
		close(o.waited)
	}
	return err
}

func (*failingStatusOwner) stop(time.Duration) error {
	return errors.New("synthetic owned-group inspection failure")
}

func retainedSyntheticAuth(t *testing.T, root string) {
	t.Helper()
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), codexHomePrefix) {
			if _, err := os.Stat(filepath.Join(root, entry.Name(), codexAuthFileName)); err != nil {
				t.Fatalf("retained private home lost its synthetic auth link: %v", err)
			}
			return
		}
	}
	t.Fatal("unproved process cleanup removed the private home")
}

func syntheticStatusBinary(t *testing.T, body string) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("synthetic shell fixture requires POSIX")
	}
	path := filepath.Join(t.TempDir(), "codex-fixture")
	file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.WriteString("#!/bin/sh\n" + body); err != nil {
		_ = file.Close()
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	// Owner read+execute permits the synthetic binary without granting group
	// or other access. This is a fixture file in t.TempDir only.
	if err := syscall.Chmod(path, 0o500); err != nil {
		t.Fatal(err)
	}
	return path
}

const syntheticChatGPTAuth = `{"auth_mode":"chatgpt","tokens":{"id_token":"e30.e30.signature","access_token":"synthetic-access","refresh_token":"synthetic-refresh"}}`

func syntheticHostAuth(t *testing.T, payload string) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("CODEX_HOME", home)
	path := filepath.Join(home, "auth.json")
	if payload != "" {
		if err := os.WriteFile(path, []byte(payload), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return path
}

const syntheticStatusScript = `
[ "$1" = '-c' ] || exit 7
[ "$2" = 'cli_auth_credentials_store="file"' ] || exit 7
[ "$3" = 'login' ] && [ "$4" = 'status' ] || exit 7
[ "$PWD" = "$CODEX_HOME" ] || exit 7
[ -z "$OPENAI_API_KEY" ] && [ -z "$CODEX_ACCESS_TOKEN" ] || exit 7
case "$(cat "$CODEX_HOME/auth.json")" in
  *'"auth_mode":"chatgpt"'*) printf 'Logged in using ChatGPT\n' >&2 ;;
  *'"auth_mode":"api_key"'*) printf 'Logged in using an API key - ***\n' >&2 ;;
  *) printf 'Not logged in\n' >&2; exit 1 ;;
esac
`

func TestCheckHostSessionLoginUsesProjectedFileOnlyAndLeavesBytesUntouched(t *testing.T) {
	payload := syntheticChatGPTAuth
	path := syntheticHostAuth(t, payload)
	before, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("OPENAI_API_KEY", "synthetic-poison")
	binary := syntheticStatusBinary(t, syntheticStatusScript)
	if err := CheckHostSessionLogin(context.Background(), binary); err != nil {
		t.Fatalf("synthetic host login: %v", err)
	}
	after, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	// Hard-link creation/removal changes ctime and link count transiently;
	// unchanged payload, inode, size and mtime are the read-only control.
	if !bytes.Equal(body, []byte(payload)) || !os.SameFile(before, after) || before.Size() != after.Size() || !before.ModTime().Equal(after.ModTime()) {
		t.Fatal("synthetic host auth changed during status check")
	}
}

func TestCheckHostSessionLoginRejectsMissingMalformedAPIKeyAndDiagnostics(t *testing.T) {
	binary := syntheticStatusBinary(t, syntheticStatusScript)
	for _, payload := range []string{"", "{malformed", "{}", `{"tokens":null}`, `{"tokens":{}}`, `{"auth_mode":"chatgpt","tokens":{"id_token":"e30.e30.signature","access_token":"","refresh_token":"synthetic"}}`, `{"auth_mode":"api_key","OPENAI_API_KEY":"synthetic"}`} {
		path := syntheticHostAuth(t, payload)
		if err := CheckHostSessionLogin(context.Background(), binary); err == nil {
			t.Fatalf("unsupported synthetic auth accepted: %q", payload)
		}
		if payload == "" {
			if _, err := os.Stat(path); !os.IsNotExist(err) {
				t.Fatalf("missing auth appeared: %v", err)
			}
		}
	}
	path := syntheticHostAuth(t, syntheticChatGPTAuth)
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(t.TempDir(), "target")
	if err := os.WriteFile(target, []byte(syntheticChatGPTAuth), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, path); err != nil {
		t.Fatal(err)
	}
	if err := CheckHostSessionLogin(context.Background(), binary); err == nil {
		t.Fatal("symlink auth accepted")
	}

	path = syntheticHostAuth(t, syntheticChatGPTAuth)
	_ = path
	noisy := syntheticStatusBinary(t, "printf 'synthetic-secret\\n' >&2\nexit 1\n")
	if err := CheckHostSessionLogin(context.Background(), noisy); err == nil || strings.Contains(err.Error(), "synthetic-secret") {
		t.Fatalf("native diagnostics escaped: %v", err)
	}
}

func TestCheckHostSessionLoginRejectsEmptyObjectDespiteNativeStatus(t *testing.T) {
	binary := syntheticStatusBinary(t, "printf 'Logged in using ChatGPT\\n' >&2\n")
	for _, fixture := range []struct{ name, payload string }{
		{"empty-object", "{}"},
		{"missing-tokens", `{"auth_mode":"chatgpt"}`},
		{"null-tokens", `{"tokens":null}`},
		{"empty-tokens", `{"tokens":{}}`},
		{"empty-access", `{"tokens":{"id_token":"e30.e30.signature","access_token":"","refresh_token":"synthetic"}}`},
		{"api-key", `{"auth_mode":"api_key","OPENAI_API_KEY":"synthetic","tokens":{"id_token":"e30.e30.signature","access_token":"synthetic","refresh_token":"synthetic"}}`},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			syntheticHostAuth(t, fixture.payload)
			if err := CheckHostSessionLogin(context.Background(), binary); err == nil {
				t.Fatal("incomplete or alternative credential accepted after native status")
			}
		})
	}
}

func TestCheckHostSessionLoginRejectsOversizedAuth(t *testing.T) {
	binary := syntheticStatusBinary(t, "printf 'Logged in using ChatGPT\\n' >&2\n")
	payload := strings.TrimSuffix(syntheticChatGPTAuth, "}") + `,"padding":"` + strings.Repeat("x", (1<<20)+256) + `"}`
	syntheticHostAuth(t, payload)
	if err := CheckHostSessionLogin(context.Background(), binary); err == nil {
		t.Fatal("oversized synthetic auth accepted")
	}
}

func TestCheckHostSessionLoginBoundsDetachedStdio(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("synthetic detached process fixture requires POSIX")
	}
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("synthetic detached process fixture requires python3")
	}
	syntheticHostAuth(t, syntheticChatGPTAuth)
	// The synthetic child exits on its own. It retains stderr outside the
	// wrapper's process group, so no PID outside our owned group is signalled.
	body := fmt.Sprintf("%q -c 'import os,time; p=os.fork(); os._exit(0) if p else None; os.setsid(); time.sleep(2)'\nprintf 'Logged in using ChatGPT\\n' >&2\n", python)
	binary := syntheticStatusBinary(t, body)
	start := time.Now()
	if err := CheckHostSessionLogin(context.Background(), binary); err == nil || time.Since(start) > time.Second {
		t.Fatalf("detached stdout/stderr child left status unbounded or accepted: %v elapsed=%s", err, time.Since(start))
	}
	// Permit the fixture's self-terminating child to exit without signalling it.
	time.Sleep(2 * time.Second)
}

func TestCheckHostSessionLoginStopsSameGroupStdioChild(t *testing.T) {
	privateRoot := t.TempDir()
	t.Setenv("TMPDIR", privateRoot)
	syntheticHostAuth(t, syntheticChatGPTAuth)
	binary := syntheticStatusBinary(t, "sleep 2 >&2 &\nprintf 'Logged in using ChatGPT\\n' >&2\n")
	started := time.Now()
	if err := CheckHostSessionLogin(context.Background(), binary); err != nil {
		t.Fatalf("owned group cleanup refused safe status: %v", err)
	}
	if time.Since(started) > time.Second {
		t.Fatal("same-group child was allowed to hold status pipes until its own exit")
	}
	entries, err := os.ReadDir(privateRoot)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), codexHomePrefix) {
			t.Fatal("quiescent owned group left private auth home")
		}
	}
}

func TestCheckHostSessionLoginRetainsBoundaryOnUnprovedStop(t *testing.T) {
	privateRoot := t.TempDir()
	t.Setenv("TMPDIR", privateRoot)
	syntheticHostAuth(t, syntheticChatGPTAuth)
	binary := syntheticStatusBinary(t, "printf 'Logged in using ChatGPT\\n' >&2\n")
	err := checkHostSessionLogin(context.Background(), binary, func(cmd *exec.Cmd) hostStatusOwner { return &failingStatusOwner{cmd: cmd} })
	if err == nil || strings.Contains(err.Error(), "synthetic owned-group") {
		t.Fatalf("unproved stop accepted or exposed diagnostics: %v", err)
	}
	retainedSyntheticAuth(t, privateRoot)
}

func TestCheckHostSessionLoginCancellationDoesNotJoinFailedStop(t *testing.T) {
	privateRoot := t.TempDir()
	t.Setenv("TMPDIR", privateRoot)
	syntheticHostAuth(t, syntheticChatGPTAuth)
	binary := syntheticStatusBinary(t, "sleep 1\nprintf 'Logged in using ChatGPT\\n' >&2\n")
	gate := make(chan struct{})
	waited := make(chan struct{})
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	start := time.Now()
	err := checkHostSessionLogin(ctx, binary, func(cmd *exec.Cmd) hostStatusOwner {
		return &failingStatusOwner{cmd: cmd, waitGate: gate, waited: waited}
	})
	if err == nil || time.Since(start) > 500*time.Millisecond {
		t.Fatalf("failed stop blocked cancellation: %v elapsed=%s", err, time.Since(start))
	}
	retainedSyntheticAuth(t, privateRoot)
	close(gate)
	select {
	case <-waited:
	case <-time.After(2 * time.Second):
		t.Fatal("synthetic child did not exit and reap itself")
	}
}

func TestCheckHostSessionLoginRefusesChangedAuthAndContextTimeout(t *testing.T) {
	path := syntheticHostAuth(t, syntheticChatGPTAuth)
	changing := syntheticStatusBinary(t, "printf 'x' >> \"$CODEX_HOME/auth.json\"\nprintf 'Logged in using ChatGPT\\n' >&2\n")
	if err := CheckHostSessionLogin(context.Background(), changing); err == nil {
		t.Fatal("native write through linked synthetic auth accepted")
	}
	if body, err := os.ReadFile(path); err != nil || !bytes.HasSuffix(body, []byte("x")) {
		t.Fatalf("negative fixture did not exercise linked inode: %v", err)
	}
	path = syntheticHostAuth(t, syntheticChatGPTAuth)
	_ = path
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := CheckHostSessionLogin(ctx, syntheticStatusBinary(t, syntheticStatusScript)); err == nil {
		t.Fatal("cancelled check accepted")
	}
	deadline, stop := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer stop()
	started := time.Now()
	if err := CheckHostSessionLogin(deadline, syntheticStatusBinary(t, "while :; do :; done\n")); err == nil || time.Since(started) > 2*time.Second {
		t.Fatalf("status timeout was not bounded: %v elapsed=%s", err, time.Since(started))
	}
}

func TestHostStatusCaptureCopyBound(t *testing.T) {
	var output statusCapture
	source := struct{ io.Reader }{strings.NewReader(strings.Repeat("x", 8192))}
	copied, err := io.Copy(&output, source)
	if err != nil || copied != 8192 || !output.overflow || output.Len() > 4096 {
		t.Fatalf("copy bytes=%d error=%v overflow=%t retained=%d; want drained output with bounded overflow", copied, err, output.overflow, output.Len())
	}
}
