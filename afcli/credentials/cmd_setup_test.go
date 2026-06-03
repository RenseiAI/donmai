package credentials

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// stubOp scripts responses for individual `op …` subcommands the wizard
// invokes. The key is args[0] (e.g. "--version", "whoami", "signin",
// "vault"). Missing keys return ErrOpNotScripted to surface scripting
// gaps loudly.
type stubOp struct {
	responses map[string]stubResp
	calls     []string
}

type stubResp struct {
	out []byte
	err error
}

var errOpNotScripted = errors.New("stubOp: subcommand not scripted")

func newStubOp(scripted map[string]stubResp) *stubOp {
	return &stubOp{responses: scripted}
}

func (s *stubOp) run(_ context.Context, args ...string) ([]byte, error) {
	key := ""
	if len(args) > 0 {
		key = args[0]
	}
	s.calls = append(s.calls, strings.Join(args, " "))
	resp, ok := s.responses[key]
	if !ok {
		return nil, fmt.Errorf("%w: %s", errOpNotScripted, key)
	}
	return resp.out, resp.err
}

// newFakeGitRoot creates t.TempDir()/proj/.git/ and returns the project
// dir. The wizard's findGitRoot walks up from CWD, so any cwd at-or-
// below proj resolves to proj.
func newFakeGitRoot(t *testing.T) string {
	t.Helper()
	proj := filepath.Join(t.TempDir(), "proj")
	if err := os.MkdirAll(filepath.Join(proj, ".git"), 0o750); err != nil {
		t.Fatalf("mkdir .git: %v", err)
	}
	return proj
}

// drive builds a wizardEnv with the given scripted stdin and stub op
// runner, runs runSetup, and returns the captured stdout, stderr, and
// the run error.
func drive(t *testing.T, cwd, stdin string, op *stubOp, opPathErr error) (string, string, error) {
	t.Helper()
	var out, errBuf bytes.Buffer
	env := &wizardEnv{
		In:  strings.NewReader(stdin),
		Out: &out,
		Err: &errBuf,
		CWD: cwd,
		LookPath: func(name string) (string, error) {
			if opPathErr != nil {
				return "", opPathErr
			}
			return "/usr/local/bin/" + name, nil
		},
		RunOp: op.run,
	}
	err := runSetup(context.Background(), env)
	return out.String(), errBuf.String(), err
}

func TestRunSetup_HappyPath_WritesEnvLocal(t *testing.T) {
	proj := newFakeGitRoot(t)
	op := newStubOp(map[string]stubResp{
		"--version": {out: []byte("2.30.0\n")},
		"whoami":    {out: []byte(`{"email":"dev@example.test","url":"team.1password.com"}`)},
		"vault":     {out: []byte("ok")},
	})

	// stdin script: accept current account (Y/empty), select default vault.
	stdin := "\n\n"

	out, _, err := drive(t, proj, stdin, op, nil)
	if err != nil {
		t.Fatalf("runSetup: %v", err)
	}

	envPath := filepath.Join(proj, ".env.local")
	info, statErr := os.Stat(envPath)
	if statErr != nil {
		t.Fatalf("stat .env.local: %v", statErr)
	}
	if mode := info.Mode().Perm(); mode != 0o600 {
		t.Errorf("file mode = %#o, want 0600", mode)
	}

	body, readErr := os.ReadFile(envPath) //nolint:gosec // test-controlled path
	if readErr != nil {
		t.Fatalf("read .env.local: %v", readErr)
	}
	content := string(body)
	for _, must := range []string{
		"af creds setup",
		"Precedence",
		"Blocklist",
		"ANTHROPIC_API_KEY=op://Private/Anthropic/credential",
		"LINEAR_API_KEY=op://Private/Linear/credential",
		"OPENAI_API_KEY=op://Private/OpenAI/credential",
	} {
		if !strings.Contains(content, must) {
			t.Errorf("env.local missing %q\n---\n%s", must, content)
		}
	}
	if !strings.Contains(out, "Wrote") {
		t.Errorf("stdout missing write confirmation:\n%s", out)
	}
}

func TestRunSetup_OpNotInPath_SkipsTo_EnvLocal(t *testing.T) {
	proj := newFakeGitRoot(t)
	op := newStubOp(map[string]stubResp{})

	stdin := "" // no questions to answer because op was absent

	out, _, err := drive(t, proj, stdin, op, errors.New("not found"))
	if err != nil {
		t.Fatalf("runSetup: %v", err)
	}

	if !strings.Contains(out, "not found in PATH") {
		t.Errorf("stdout missing 'not found in PATH' notice:\n%s", out)
	}
	if _, statErr := os.Stat(filepath.Join(proj, ".env.local")); statErr != nil {
		t.Errorf("expected .env.local written when op absent: %v", statErr)
	}
	if len(op.calls) != 0 {
		t.Errorf("expected no `op` invocations when LookPath errors, got %v", op.calls)
	}
}

func TestRunSetup_OpNotSignedIn_UserDeclines(t *testing.T) {
	proj := newFakeGitRoot(t)
	op := newStubOp(map[string]stubResp{
		"--version": {out: []byte("2.30.0")},
		"whoami":    {err: errors.New("not signed in")},
	})

	// stdin: decline sign-in.
	stdin := "n\n"

	out, _, err := drive(t, proj, stdin, op, nil)
	if err != nil {
		t.Fatalf("runSetup: %v", err)
	}

	if !strings.Contains(out, "Skipping 1Password") {
		t.Errorf("expected skip message in stdout:\n%s", out)
	}
	if _, statErr := os.Stat(filepath.Join(proj, ".env.local")); statErr != nil {
		t.Errorf("expected .env.local written after skip: %v", statErr)
	}
}

func TestRunSetup_OpSignsInOnRequest(t *testing.T) {
	proj := newFakeGitRoot(t)
	op := newStubOp(map[string]stubResp{
		"--version": {out: []byte("2.30.0")},
		"whoami":    {err: errors.New("not signed in")},
		"signin":    {out: []byte("session-token-here\n")},
		"vault":     {out: []byte("ok")},
	})

	// stdin: accept signin, default vault.
	stdin := "y\n\n"

	out, _, err := drive(t, proj, stdin, op, nil)
	if err != nil {
		t.Fatalf("runSetup: %v", err)
	}

	if !strings.Contains(out, "Signed in.") {
		t.Errorf("expected 'Signed in.' line in stdout:\n%s", out)
	}
	if !contains(op.calls, "signin --raw") {
		t.Errorf("expected `op signin --raw` invocation, got %v", op.calls)
	}
}

func TestRunSetup_VaultMissingRetriesUntilFound(t *testing.T) {
	proj := newFakeGitRoot(t)

	// vault calls fail twice, then succeed.
	vaultAttempts := 0
	op := &stubOp{
		responses: map[string]stubResp{
			"--version": {out: []byte("2.30.0")},
			"whoami":    {out: []byte(`{"email":"dev@example.test"}`)},
		},
	}
	// Override RunOp via a closure that handles the vault loop.
	runOp := func(_ context.Context, args ...string) ([]byte, error) {
		op.calls = append(op.calls, strings.Join(args, " "))
		if len(args) > 0 && args[0] == "vault" {
			vaultAttempts++
			if vaultAttempts < 3 {
				return nil, errors.New("vault not found")
			}
			return []byte("ok"), nil
		}
		key := ""
		if len(args) > 0 {
			key = args[0]
		}
		resp, ok := op.responses[key]
		if !ok {
			return nil, fmt.Errorf("%w: %s", errOpNotScripted, key)
		}
		return resp.out, resp.err
	}

	// stdin: accept current account, supply bad vault, bad vault again,
	// then a good one.
	stdin := "\nWrongVault\nStillWrong\nGoodVault\n"

	var outBuf, errBuf bytes.Buffer
	env := &wizardEnv{
		In:       strings.NewReader(stdin),
		Out:      &outBuf,
		Err:      &errBuf,
		CWD:      proj,
		LookPath: func(string) (string, error) { return "/usr/local/bin/op", nil },
		RunOp:    runOp,
	}
	if err := runSetup(context.Background(), env); err != nil {
		t.Fatalf("runSetup: %v", err)
	}

	if vaultAttempts != 3 {
		t.Errorf("vault attempts = %d, want 3", vaultAttempts)
	}

	body, _ := os.ReadFile(filepath.Join(proj, ".env.local")) //nolint:gosec // test-controlled path
	if !strings.Contains(string(body), "op://GoodVault/Anthropic/credential") {
		t.Errorf(".env.local missing GoodVault reference:\n%s", string(body))
	}
}

func TestRunSetup_EnvLocalExists_UserDeclinesOverwrite(t *testing.T) {
	proj := newFakeGitRoot(t)
	envPath := filepath.Join(proj, ".env.local")
	original := []byte("EXISTING=value\n")
	if err := os.WriteFile(envPath, original, 0o600); err != nil {
		t.Fatalf("seed .env.local: %v", err)
	}

	op := newStubOp(map[string]stubResp{
		"--version": {out: []byte("2.30.0")},
		"whoami":    {out: []byte(`{"email":"dev@example.test"}`)},
		"vault":     {out: []byte("ok")},
	})

	// stdin: accept account, default vault, then DECLINE overwrite.
	stdin := "\n\nn\n"

	out, _, err := drive(t, proj, stdin, op, nil)
	if err != nil {
		t.Fatalf("runSetup: %v", err)
	}

	got, _ := os.ReadFile(envPath) //nolint:gosec // test-controlled path
	if !bytes.Equal(got, original) {
		t.Errorf("file was overwritten\nbefore=%q\nafter=%q", original, got)
	}
	if !strings.Contains(out, "preserved") {
		t.Errorf("stdout missing 'preserved' notice:\n%s", out)
	}
}

func TestRunSetup_EnvLocalExists_UserAcceptsOverwrite(t *testing.T) {
	proj := newFakeGitRoot(t)
	envPath := filepath.Join(proj, ".env.local")
	if err := os.WriteFile(envPath, []byte("OLD=1\n"), 0o600); err != nil {
		t.Fatalf("seed .env.local: %v", err)
	}

	op := newStubOp(map[string]stubResp{
		"--version": {out: []byte("2.30.0")},
		"whoami":    {out: []byte(`{"email":"dev@example.test"}`)},
		"vault":     {out: []byte("ok")},
	})

	// stdin: accept account, default vault, accept overwrite.
	stdin := "\n\ny\n"

	if _, _, err := drive(t, proj, stdin, op, nil); err != nil {
		t.Fatalf("runSetup: %v", err)
	}

	body, _ := os.ReadFile(envPath) //nolint:gosec // test-controlled path
	if strings.Contains(string(body), "OLD=1") {
		t.Errorf("expected file to be overwritten, still contains OLD=1: %s", body)
	}
	if !strings.Contains(string(body), "ANTHROPIC_API_KEY") {
		t.Errorf("expected new content, got: %s", body)
	}
}

func TestRunSetup_NotInGitRepo_FailsClearly(t *testing.T) {
	// CWD is a tempdir with no .git anywhere up the chain. The walk
	// terminates at the filesystem root.
	cwd := t.TempDir()

	op := newStubOp(map[string]stubResp{
		"--version": {out: []byte("2.30.0")},
		"whoami":    {out: []byte(`{"email":"dev@example.test"}`)},
		"vault":     {out: []byte("ok")},
	})
	stdin := "\n\n"

	_, _, err := drive(t, cwd, stdin, op, nil)
	if err == nil {
		t.Fatalf("expected error when CWD is not inside a git repo")
	}
	if !strings.Contains(err.Error(), "no git repository") {
		t.Errorf("error = %v, want 'no git repository' substring", err)
	}
}

func TestRunSetup_HeaderAndSamplesPresent(t *testing.T) {
	proj := newFakeGitRoot(t)
	op := newStubOp(map[string]stubResp{
		"--version": {out: []byte("2.30.0")},
		"whoami":    {out: []byte(`{"email":"dev@example.test"}`)},
		"vault":     {out: []byte("ok")},
	})
	stdin := "\n\n"

	if _, _, err := drive(t, proj, stdin, op, nil); err != nil {
		t.Fatalf("runSetup: %v", err)
	}

	body, _ := os.ReadFile(filepath.Join(proj, ".env.local")) //nolint:gosec // test-controlled path
	content := string(body)

	headerMarkers := []string{
		"Generated by `af creds setup`",
		"Precedence:",
		"Blocklist:",
		"File mode is 0600",
	}
	for _, m := range headerMarkers {
		if !strings.Contains(content, m) {
			t.Errorf("missing header line %q", m)
		}
	}

	// Four commented sample entries (Gemini added for first-class support).
	count := strings.Count(content, "=op://Private/")
	if count != 4 {
		t.Errorf("expected 4 op:// samples, got %d:\n%s", count, content)
	}
	for _, key := range []string{"ANTHROPIC_API_KEY", "GEMINI_API_KEY", "LINEAR_API_KEY", "OPENAI_API_KEY"} {
		want := "# " + key + "=op://Private/"
		if !strings.Contains(content, want) {
			t.Errorf("missing commented sample %q", want)
		}
	}
}

func TestRunSetup_FilePermissionsExplicit(t *testing.T) {
	proj := newFakeGitRoot(t)
	op := newStubOp(map[string]stubResp{
		"--version": {out: []byte("2.30.0")},
		"whoami":    {out: []byte(`{"email":"dev@example.test"}`)},
		"vault":     {out: []byte("ok")},
	})
	stdin := "\n\n"

	if _, _, err := drive(t, proj, stdin, op, nil); err != nil {
		t.Fatalf("runSetup: %v", err)
	}

	info, statErr := os.Stat(filepath.Join(proj, ".env.local"))
	if statErr != nil {
		t.Fatalf("stat: %v", statErr)
	}
	if mode := info.Mode() & fs.ModePerm; mode != 0o600 {
		t.Errorf("file mode = %#o, want 0600", mode)
	}
}

// contains reports whether haystack has needle as one of its elements.
func contains(haystack []string, needle string) bool {
	for _, s := range haystack {
		if s == needle {
			return true
		}
	}
	return false
}
