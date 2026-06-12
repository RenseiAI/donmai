package credentials

import (
	"bytes"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// newTempGitRoot returns an absolute path to a fresh temp dir that
// LoadLocalSource will treat as a gitRoot. Cleanup is handled by t.
func newTempGitRoot(t *testing.T) string {
	t.Helper()
	return t.TempDir()
}

// writeEnvLocal writes content to ${dir}/.env.local with mode perm.
func writeEnvLocal(t *testing.T, dir, content string, perm os.FileMode) string {
	t.Helper()
	p := filepath.Join(dir, ".env.local")
	if err := os.WriteFile(p, []byte(content), perm); err != nil {
		t.Fatalf("write .env.local: %v", err)
	}
	return p
}

// loadWithStderr is a test helper that captures stderr into buf.
func loadWithStderr(t *testing.T, gitRoot string, buf *bytes.Buffer) *LocalSource {
	t.Helper()
	s, err := LoadLocalSource(gitRoot)
	if err != nil {
		t.Fatalf("LoadLocalSource: %v", err)
	}
	s.stderr = buf
	// Re-run the load with the redirected stderr so warning lines we
	// expect to assert against are captured. LoadLocalSource emits its
	// warnings during the os.Stat/parse path on initial load — to
	// capture those we hand-build a fresh LocalSource and call the
	// loader directly.
	fresh := &LocalSource{
		processEnv: snapshotProcessEnv(),
		fileEnv:    map[string]string{},
		sources:    map[string]sourceLabel{},
		stderr:     buf,
	}
	if gitRoot != "" {
		fresh.envLocalPath = filepath.Join(gitRoot, ".env.local")
		if err := fresh.loadEnvLocal(fresh.envLocalPath); err != nil {
			t.Fatalf("loadEnvLocal: %v", err)
		}
		for k := range fresh.processEnv {
			fresh.sources[k] = SourceProcess
		}
		for k := range fresh.fileEnv {
			if _, ok := fresh.sources[k]; !ok {
				fresh.sources[k] = SourceFile
			}
		}
	}
	return fresh
}

func TestLoadLocalSource_ProcessEnvOnly(t *testing.T) {
	t.Setenv("AF_TEST_PROC_ONLY", "from-process")

	root := newTempGitRoot(t)
	s, err := LoadLocalSource(root)
	if err != nil {
		t.Fatalf("LoadLocalSource: %v", err)
	}
	v, src, ok := s.Resolve("AF_TEST_PROC_ONLY")
	if !ok {
		t.Fatalf("Resolve: variable not found")
	}
	if v != "from-process" {
		t.Errorf("value = %q, want %q", v, "from-process")
	}
	if src != string(SourceProcess) {
		t.Errorf("source = %q, want %q", src, SourceProcess)
	}
}

func TestLoadLocalSource_EnvLocalHappyPath(t *testing.T) {
	root := newTempGitRoot(t)
	writeEnvLocal(t, root, "AF_TEST_FROM_FILE=from-file\n", 0o600)

	s, err := LoadLocalSource(root)
	if err != nil {
		t.Fatalf("LoadLocalSource: %v", err)
	}
	v, src, ok := s.Resolve("AF_TEST_FROM_FILE")
	if !ok {
		t.Fatalf("Resolve: not found")
	}
	if v != "from-file" {
		t.Errorf("value = %q, want %q", v, "from-file")
	}
	if src != string(SourceFile) {
		t.Errorf("source = %q, want %q", src, SourceFile)
	}
}

func TestLoadLocalSource_ProcessWinsOverFile(t *testing.T) {
	// Make sure the var is exported in process env BEFORE we load.
	t.Setenv("AF_TEST_COLLIDE", "from-process")

	root := newTempGitRoot(t)
	writeEnvLocal(t, root, "AF_TEST_COLLIDE=from-file\n", 0o600)

	s, err := LoadLocalSource(root)
	if err != nil {
		t.Fatalf("LoadLocalSource: %v", err)
	}
	v, src, ok := s.Resolve("AF_TEST_COLLIDE")
	if !ok {
		t.Fatalf("Resolve: not found")
	}
	if v != "from-process" {
		t.Errorf("value = %q, want %q (process should win)", v, "from-process")
	}
	if src != string(SourceProcess) {
		t.Errorf("source = %q, want %q (process should win)", src, SourceProcess)
	}
}

func TestLoadLocalSource_MissingBoth_WarnMissing(t *testing.T) {
	root := newTempGitRoot(t)
	// Use a unique random-looking name to avoid collision with the host env.
	missing := "AF_TEST_DEFINITELY_MISSING_XYZZY42"
	if _, ok := os.LookupEnv(missing); ok {
		t.Skip("host env unexpectedly defines " + missing)
	}

	var buf bytes.Buffer
	s := loadWithStderr(t, root, &buf)
	s.WarnMissing([]string{missing})
	out := buf.String()
	wantSubstr := "[creds] no source for " + missing + "; agent may fail"
	if !strings.Contains(out, wantSubstr) {
		t.Errorf("WarnMissing stderr did not contain %q\n  got: %q", wantSubstr, out)
	}
}

func TestLoadLocalSource_MalformedLineLogged(t *testing.T) {
	root := newTempGitRoot(t)
	content := strings.Join([]string{
		"VALID_KEY=value",
		"this line has no equals sign and is malformed",
		"ANOTHER_KEY=ok",
	}, "\n") + "\n"
	writeEnvLocal(t, root, content, 0o600)

	var buf bytes.Buffer
	s := loadWithStderr(t, root, &buf)

	// Partial load — well-formed entries should still be present.
	if v, _, ok := s.Resolve("VALID_KEY"); !ok || v != "value" {
		t.Errorf("VALID_KEY: got (%q, %v), want (%q, true)", v, ok, "value")
	}
	if v, _, ok := s.Resolve("ANOTHER_KEY"); !ok || v != "ok" {
		t.Errorf("ANOTHER_KEY: got (%q, %v), want (%q, true)", v, ok, "ok")
	}

	// Warning must reference the line number but NOT the line content.
	out := buf.String()
	if !strings.Contains(out, "line 2 malformed") {
		t.Errorf("malformed-line warning missing line number:\n  got: %q", out)
	}
	if strings.Contains(out, "no equals sign") {
		t.Errorf("malformed-line warning leaked line content:\n  got: %q", out)
	}
}

func TestLoadLocalSource_WorldReadableWarning(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX file modes only")
	}
	root := newTempGitRoot(t)
	writeEnvLocal(t, root, "FOO=bar\n", 0o644) // world-readable

	var buf bytes.Buffer
	_ = loadWithStderr(t, root, &buf)
	out := buf.String()
	if !strings.Contains(out, "is world-readable") {
		t.Errorf("world-readable warning missing:\n  got: %q", out)
	}
	if !strings.Contains(out, "chmod 600") {
		t.Errorf("world-readable warning missing chmod hint:\n  got: %q", out)
	}
}

func TestLoadLocalSource_BlocklistEnforced_ProcessSource(t *testing.T) {
	t.Setenv("DONMAI_DAEMON_JWT", "must-not-forward")

	root := newTempGitRoot(t)
	s, err := LoadLocalSource(root)
	if err != nil {
		t.Fatalf("LoadLocalSource: %v", err)
	}
	// Resolve still sees it (it IS present in process env — we just
	// refuse to forward it to children).
	if _, _, ok := s.Resolve("DONMAI_DAEMON_JWT"); !ok {
		t.Errorf("Resolve(DONMAI_DAEMON_JWT) ok=false, want true")
	}
	out := s.ApplyToChildEnv(nil)
	for _, e := range out {
		if strings.HasPrefix(e, "DONMAI_DAEMON_JWT=") {
			t.Errorf("ApplyToChildEnv forwarded blocked key: %s", e)
		}
	}
	merged := s.MergeIntoBaseEnv(map[string]string{})
	if _, ok := merged["DONMAI_DAEMON_JWT"]; ok {
		t.Errorf("MergeIntoBaseEnv forwarded blocked key")
	}
}

func TestLoadLocalSource_BlocklistEnforced_FileSource(t *testing.T) {
	root := newTempGitRoot(t)
	writeEnvLocal(t, root, "WORKER_API_KEY=rsk_must_not_forward\nLEGIT=value\n", 0o600)

	s, err := LoadLocalSource(root)
	if err != nil {
		t.Fatalf("LoadLocalSource: %v", err)
	}
	merged := s.MergeIntoBaseEnv(map[string]string{})
	if _, ok := merged["WORKER_API_KEY"]; ok {
		t.Errorf("MergeIntoBaseEnv forwarded blocked key from .env.local")
	}
	if v, ok := merged["LEGIT"]; !ok || v != "value" {
		t.Errorf("non-blocked key dropped: got (%q, %v)", v, ok)
	}
}

func TestApplyToChildEnv_ChildWins(t *testing.T) {
	t.Setenv("AF_TEST_CHILD_WINS", "from-process")
	root := newTempGitRoot(t)
	s, err := LoadLocalSource(root)
	if err != nil {
		t.Fatalf("LoadLocalSource: %v", err)
	}
	childEnv := []string{"AF_TEST_CHILD_WINS=from-child"}
	out := s.ApplyToChildEnv(childEnv)
	// Should appear exactly once, as "from-child".
	matches := 0
	for _, e := range out {
		if strings.HasPrefix(e, "AF_TEST_CHILD_WINS=") {
			matches++
			if e != "AF_TEST_CHILD_WINS=from-child" {
				t.Errorf("collision resolved wrong way: %s", e)
			}
		}
	}
	if matches != 1 {
		t.Errorf("AF_TEST_CHILD_WINS appears %d times in merged env, want 1", matches)
	}
}

func TestApplyToChildEnv_AppendsMissing(t *testing.T) {
	root := newTempGitRoot(t)
	writeEnvLocal(t, root, "AF_TEST_APPEND_ME=hello\n", 0o600)
	s, err := LoadLocalSource(root)
	if err != nil {
		t.Fatalf("LoadLocalSource: %v", err)
	}
	out := s.ApplyToChildEnv(nil)
	found := false
	for _, e := range out {
		if e == "AF_TEST_APPEND_ME=hello" {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("ApplyToChildEnv did not forward .env.local entry AF_TEST_APPEND_ME")
	}
}

func TestParseDotenvLine_QuotedValuesStripped(t *testing.T) {
	t.Parallel()
	cases := []struct {
		in        string
		key, want string
	}{
		{`FOO="quoted value"`, "FOO", "quoted value"},
		{`FOO='single quoted'`, "FOO", "single quoted"},
		{`FOO="a # not a comment"`, "FOO", "a # not a comment"},
		{`FOO="   leading-space-inside"`, "FOO", "   leading-space-inside"},
		{`FOO="trailing-space-inside   "`, "FOO", "trailing-space-inside   "},
		{`FOO=bar`, "FOO", "bar"},
		{`FOO=`, "FOO", ""},
	}
	for _, c := range cases {
		k, v, ok := parseDotenvLine(c.in)
		if !ok {
			t.Errorf("parseDotenvLine(%q) ok=false", c.in)
			continue
		}
		if k != c.key || v != c.want {
			t.Errorf("parseDotenvLine(%q) = (%q, %q), want (%q, %q)", c.in, k, v, c.key, c.want)
		}
	}
}

func TestParseDotenvLine_CommentsAndBlanks(t *testing.T) {
	t.Parallel()
	cases := []string{
		"",
		"   ",
		"# this is a comment",
		"   # indented comment",
	}
	for _, in := range cases {
		k, _, ok := parseDotenvLine(in)
		if !ok {
			t.Errorf("parseDotenvLine(%q) ok=false, want true (blank/comment is silent skip)", in)
		}
		if k != "" {
			t.Errorf("parseDotenvLine(%q) key = %q, want empty (blank/comment yields empty key)", in, k)
		}
	}
}

func TestParseDotenvLine_Malformed(t *testing.T) {
	t.Parallel()
	cases := []string{
		"no equals sign here",
		"=just-a-value",
	}
	for _, in := range cases {
		_, _, ok := parseDotenvLine(in)
		if ok {
			t.Errorf("parseDotenvLine(%q) ok=true, want false (malformed)", in)
		}
	}
}

func TestParseDotenvLine_ExportPrefix(t *testing.T) {
	t.Parallel()
	k, v, ok := parseDotenvLine(`export FOO=bar`)
	if !ok || k != "FOO" || v != "bar" {
		t.Errorf("parseDotenvLine(export FOO=bar) = (%q, %q, %v), want (FOO, bar, true)", k, v, ok)
	}
}

func TestParseDotenvLine_InlineComment(t *testing.T) {
	t.Parallel()
	// Unquoted value with ' #' tail → comment is stripped.
	k, v, ok := parseDotenvLine(`FOO=bar # trailing comment`)
	if !ok || k != "FOO" || v != "bar" {
		t.Errorf("parseDotenvLine inline-comment = (%q, %q, %v), want (FOO, bar, true)", k, v, ok)
	}
}

func TestLoadLocalSource_NoEnvLocalIsSilent(t *testing.T) {
	root := newTempGitRoot(t)
	// No .env.local written.
	var buf bytes.Buffer
	s := loadWithStderr(t, root, &buf)
	if buf.Len() != 0 {
		t.Errorf("LoadLocalSource emitted stderr for missing .env.local:\n  got: %q", buf.String())
	}
	if len(s.FileEnvKeys()) != 0 {
		t.Errorf("FileEnvKeys non-empty for missing .env.local: %v", s.FileEnvKeys())
	}
}

func TestLoadLocalSource_EmptyGitRoot(t *testing.T) {
	// gitRoot == "" should skip the file lookup entirely.
	s, err := LoadLocalSource("")
	if err != nil {
		t.Fatalf("LoadLocalSource(\"\"): %v", err)
	}
	if s.EnvLocalPath() != "" {
		t.Errorf("EnvLocalPath = %q, want empty", s.EnvLocalPath())
	}
	if len(s.FileEnvKeys()) != 0 {
		t.Errorf("FileEnvKeys non-empty when gitRoot is empty")
	}
}
