package claude

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/RenseiAI/donmai/agent"
)

// TestWriteCardSkillsPluginDir_OutsideWorktree proves the per-session plugin
// directory sits outside the worktree: no SKILL.md lands in the checkout, so
// `git status` stays clean and the agent cannot commit its own skills.
func TestWriteCardSkillsPluginDir_OutsideWorktree(t *testing.T) {
	t.Parallel()

	worktree := t.TempDir()
	skills := []agent.CardSkill{{ID: "go-review", Body: "review carefully"}}
	dir, err := writeCardSkillsPluginDir(skills)
	if err != nil {
		t.Fatalf("writeCardSkillsPluginDir: %v", err)
	}
	t.Cleanup(func() { _ = removeCardSkillsPluginDir(dir) })

	if dir == "" {
		t.Fatal("plugin dir is empty for non-empty skills")
	}
	if !filepath.IsAbs(dir) {
		t.Fatalf("plugin dir %q is not absolute", dir)
	}
	// Outside the worktree: the worktree is neither the dir nor a parent.
	rel, err := filepath.Rel(worktree, dir)
	if err != nil {
		t.Fatalf("Rel: %v", err)
	}
	if rel == "." || (!strings.HasPrefix(rel, ".."+string(os.PathSeparator)) && rel != ".." && !filepath.IsAbs(rel)) {
		// Rel without a leading .. means dir is inside worktree.
		if !strings.HasPrefix(rel, "..") {
			t.Fatalf("plugin dir %q is inside worktree %q (rel %q)", dir, worktree, rel)
		}
	}
	// No skill file under the worktree.
	var found []string
	_ = filepath.Walk(worktree, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return nil
		}
		if !info.IsDir() && (info.Name() == "SKILL.md" || info.Name() == "plugin.json") {
			found = append(found, path)
		}
		return nil
	})
	if len(found) > 0 {
		t.Fatalf("skill files inside worktree %q: %q", worktree, found)
	}
	// The plugin dir itself carries the skill.
	skillPath := filepath.Join(dir, "skills", "go-review", "SKILL.md")
	body, err := os.ReadFile(skillPath) //nolint:gosec // path from our own temp dir
	if err != nil {
		t.Fatalf("read SKILL.md: %v", err)
	}
	if !strings.Contains(string(body), "review carefully") {
		t.Fatalf("SKILL.md missing body: %q", body)
	}
	if !strings.Contains(string(body), "name: go-review") {
		t.Fatalf("SKILL.md missing frontmatter name: %q", body)
	}
	manifest, err := os.ReadFile(filepath.Join(dir, ".claude-plugin", "plugin.json")) //nolint:gosec // own temp dir
	if err != nil {
		t.Fatalf("read plugin.json: %v", err)
	}
	if !strings.Contains(string(manifest), "donmai-card-skills") {
		t.Fatalf("plugin.json missing plugin name: %q", manifest)
	}
	// sha256 readback: the file on disk hashes to the bytes written.
	want := sha256.Sum256(body)
	back, err := os.ReadFile(skillPath) //nolint:gosec // own temp dir
	if err != nil {
		t.Fatalf("read back SKILL.md: %v", err)
	}
	if got := sha256.Sum256(back); got != want {
		t.Fatalf("readback mismatch: want %s, got %s", hex.EncodeToString(want[:]), hex.EncodeToString(got[:]))
	}
}

// TestWriteCardSkillsPluginDir_EmptyIsNoop proves an empty or whitespace-only
// skill set emits no flag: the spawn argv stays byte-identical.
func TestWriteCardSkillsPluginDir_EmptyIsNoop(t *testing.T) {
	t.Parallel()

	for _, skills := range [][]agent.CardSkill{
		nil,
		{},
		{{ID: "blank", Body: "   \n\t "}},
	} {
		dir, err := writeCardSkillsPluginDir(skills)
		if err != nil {
			t.Fatalf("writeCardSkillsPluginDir(%v): %v", skills, err)
		}
		if dir != "" {
			_ = removeCardSkillsPluginDir(dir)
			t.Fatalf("writeCardSkillsPluginDir(%v) = %q, want empty", skills, dir)
		}
	}
}

// TestWriteCardSkillsPluginDir_RejectsOversize proves bodies over the size
// cap fail closed before spawn — never truncated.
func TestWriteCardSkillsPluginDir_RejectsOversize(t *testing.T) {
	t.Parallel()

	big := strings.Repeat("x", maxCardSkillBodyBytes+1)
	if _, err := writeCardSkillsPluginDir([]agent.CardSkill{{ID: "big", Body: big}}); err == nil {
		t.Fatal("oversize skill: expected error, got nil")
	} else if !isSpawnFailed(err) {
		t.Fatalf("oversize skill error %v should wrap ErrSpawnFailed", err)
	}
	// Exactly at the cap is admitted.
	atCap := strings.Repeat("y", maxCardSkillBodyBytes)
	dir, err := writeCardSkillsPluginDir([]agent.CardSkill{{ID: "atcap", Body: atCap}})
	if err != nil {
		t.Fatalf("at-cap skill should succeed: %v", err)
	}
	_ = removeCardSkillsPluginDir(dir)
}

// TestWriteCardSkillsPluginDir_RejectsUnsafePaths proves ids that could
// escape the plugin directory fail closed.
func TestWriteCardSkillsPluginDir_RejectsUnsafePaths(t *testing.T) {
	t.Parallel()

	unsafe := []string{
		"",
		".",
		"..",
		"../evil",
		"a/b",
		`a\b`,
		"/abs",
		"has space",
		"has.dot",
		"-leading-dash",
		"_leading_underscore",
		strings.Repeat("a", maxCardSkillIDLength+1),
	}
	for _, id := range unsafe {
		t.Run("id="+id, func(t *testing.T) {
			t.Parallel()
			_, err := writeCardSkillsPluginDir([]agent.CardSkill{{ID: id, Body: "body"}})
			if err == nil {
				t.Fatalf("unsafe id %q: expected error, got nil", id)
			}
			if !isSpawnFailed(err) {
				t.Fatalf("unsafe id %q error %v should wrap ErrSpawnFailed", id, err)
			}
		})
	}
	t.Run("duplicate ids", func(t *testing.T) {
		t.Parallel()
		_, err := writeCardSkillsPluginDir([]agent.CardSkill{
			{ID: "dup", Body: "one"},
			{ID: "dup", Body: "two"},
		})
		if err == nil {
			t.Fatal("duplicate ids: expected error, got nil")
		}
		if !isSpawnFailed(err) {
			t.Fatalf("duplicate error %v should wrap ErrSpawnFailed", err)
		}
	})
}

// TestBuildArgs_PluginDirFlag proves --plugin-dir rides the headless argv
// exactly when a plugin directory exists, and is absent otherwise.
func TestBuildArgs_PluginDirFlag(t *testing.T) {
	t.Parallel()

	t.Run("present", func(t *testing.T) {
		t.Parallel()
		argv, _ := buildArgs(agent.Spec{Prompt: "p"}, "", "/tmp/plug-123", "", "")
		idx := slices.Index(argv, "--plugin-dir")
		if idx < 0 || idx+1 >= len(argv) || argv[idx+1] != "/tmp/plug-123" {
			t.Fatalf("argv missing --plugin-dir /tmp/plug-123: %q", argv)
		}
	})
	t.Run("absent keeps byte-identical shape", func(t *testing.T) {
		t.Parallel()
		argv, _ := buildArgs(agent.Spec{Prompt: "p"}, "", "", "", "")
		if slices.Contains(argv, "--plugin-dir") {
			t.Fatalf("skill-less argv carries --plugin-dir: %q", argv)
		}
	})
	t.Run("interactive matches headless", func(t *testing.T) {
		t.Parallel()
		headless, _ := buildArgs(agent.Spec{Prompt: "ship it"}, "", "/tmp/plug-9", "", "")
		got := interactiveArgsWith(agent.Spec{Prompt: "ship it"}, "", "/tmp/plug-9", "")
		for _, argv := range [][]string{headless, got} {
			idx := slices.Index(argv, "--plugin-dir")
			if idx < 0 || argv[idx+1] != "/tmp/plug-9" {
				t.Fatalf("argv missing --plugin-dir: %q", argv)
			}
		}
	})
}

// TestSpawn_Resume_RepassesPluginDir is the load-bearing proof for the
// resume path: the flag is passed on first launch AND on resume. RED proof:
// drop the plugin-dir fragment from the resume spawn (buildArgs without the
// plugin dir on the resume path, or a driver that vetoes it) and the resume
// assertion fails while the first-launch assertion still passes.
func TestSpawn_Resume_RepassesPluginDir(t *testing.T) {
	t.Parallel()

	worktree := t.TempDir()
	traceFile := filepath.Join(t.TempDir(), "argv-trace.log")
	// Fake CLI records its argv per invocation and answers with a minimal
	// completed-turn envelope. The parent (fresh) and the resume (--resume)
	// invocations both hit this script; the trace distinguishes them.
	script := "#!/bin/sh\n" +
		"echo \"INVOCATION:$@\" >> " + shQuote(traceFile) + "\n" +
		`printf '%s\n' '{"type":"system","subtype":"init","session_id":"sess-plugin-1"}'` + "\n" +
		`printf '%s\n' '{"type":"result","subtype":"success","is_error":false,"num_turns":1,"usage":{}}'` + "\n"
	cli := writeFakeCLI(t, "fake-claude-plugin-dir.sh", script)
	p, err := New(Options{Binary: cli, LookPath: func(name string) (string, error) { return name, nil }})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	skills := []agent.CardSkill{{ID: "go-review", Body: "review carefully"}}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	h, err := p.Spawn(ctx, agent.Spec{Prompt: "first", Cwd: worktree, CardSkills: skills})
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	_ = drainAllWithIdle(t, h.Events(), 5*time.Second, 30*time.Second)
	// Capture the first-launch plugin dir before Stop removes it.
	firstDir := pluginDirFromTrace(t, traceFile, 0)
	if firstDir == "" {
		t.Fatalf("first-launch argv carries no --plugin-dir:\n%s", readTrace(t, traceFile))
	}
	assertPluginDirOutsideWorktree(t, firstDir, worktree)
	assertNoSkillFileInWorktree(t, worktree)
	if err := h.Stop(context.Background()); err != nil {
		t.Fatalf("Stop: %v", err)
	}

	// Resume must re-materialize and re-pass its own plugin directory.
	h2, err := p.Resume(ctx, "sess-plugin-1", agent.Spec{Prompt: "second", Cwd: worktree, CardSkills: skills})
	if err != nil {
		t.Fatalf("Resume: %v", err)
	}
	_ = drainAllWithIdle(t, h2.Events(), 5*time.Second, 30*time.Second)
	trace := readTrace(t, traceFile)
	invocations := strings.Split(strings.TrimSpace(trace), "\n")
	if len(invocations) < 2 {
		t.Fatalf("want parent + resume invocations, got %d:\n%s", len(invocations), trace)
	}
	resumeLine := invocations[len(invocations)-1]
	if !strings.Contains(resumeLine, "--plugin-dir") {
		t.Fatalf("resume argv drops --plugin-dir (RED if resume omits the flag):\n%s", trace)
	}
	if !strings.Contains(resumeLine, "--resume sess-plugin-1") {
		t.Fatalf("resume argv missing --resume sess-plugin-1:\n%s", trace)
	}
	resumeDir := pluginDirFromLine(resumeLine)
	assertPluginDirOutsideWorktree(t, resumeDir, worktree)
	assertNoSkillFileInWorktree(t, worktree)
	if err := h2.Stop(context.Background()); err != nil {
		t.Fatalf("Stop resume: %v", err)
	}
	if _, err := os.Stat(resumeDir); !os.IsNotExist(err) {
		t.Fatalf("resume plugin dir %q was not removed on Stop", resumeDir)
	}
}

// TestHandle_Inject_RepassesPluginDir proves the between-turn inject
// subprocess re-passes --plugin-dir: the CLI's --resume does not restore
// session-only plugin directories on its own.
func TestHandle_Inject_RepassesPluginDir(t *testing.T) {
	t.Parallel()

	worktree := t.TempDir()
	traceFile := filepath.Join(t.TempDir(), "argv-trace.log")
	script := "#!/bin/sh\n" +
		"echo \"INVOCATION:$@\" >> " + shQuote(traceFile) + "\n" +
		`printf '%s\n' '{"type":"system","subtype":"init","session_id":"sess-inject-plugin"}'` + "\n" +
		`printf '%s\n' '{"type":"result","subtype":"success","is_error":false,"num_turns":1,"usage":{}}'` + "\n"
	cli := writeFakeCLI(t, "fake-claude-inject-plugin.sh", script)
	p, err := New(Options{Binary: cli, LookPath: func(name string) (string, error) { return name, nil }})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	h, err := p.Spawn(ctx, agent.Spec{
		Prompt:     "first",
		Cwd:        worktree,
		CardSkills: []agent.CardSkill{{ID: "go-review", Body: "review carefully"}},
	})
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	t.Cleanup(func() { _ = h.Stop(context.Background()) })
	_ = drainAllWithIdle(t, h.Events(), 5*time.Second, 30*time.Second)

	if err := h.Inject(ctx, "follow up"); err != nil {
		t.Fatalf("Inject: %v", err)
	}
	_ = drainAllWithIdle(t, h.Events(), 5*time.Second, 30*time.Second)

	trace := readTrace(t, traceFile)
	lines := strings.Split(strings.TrimSpace(trace), "\n")
	if len(lines) < 2 {
		t.Fatalf("want parent + inject invocations, got %d:\n%s", len(lines), trace)
	}
	injectLine := lines[len(lines)-1]
	if !strings.Contains(injectLine, "--resume sess-inject-plugin") {
		t.Fatalf("inject argv missing --resume:\n%s", trace)
	}
	if !strings.Contains(injectLine, "--plugin-dir") {
		t.Fatalf("inject argv drops --plugin-dir (RED if inject omits the flag):\n%s", trace)
	}
	parentDir := pluginDirFromLine(lines[0])
	injectDir := pluginDirFromLine(injectLine)
	if parentDir == "" || injectDir == "" {
		t.Fatalf("could not parse plugin dirs:\n%s", trace)
	}
	if parentDir != injectDir {
		t.Fatalf("inject reuses the parent plugin dir: parent %q inject %q", parentDir, injectDir)
	}
	assertNoSkillFileInWorktree(t, worktree)
}

func isSpawnFailed(err error) bool {
	if err == nil {
		return false
	}
	for err != nil {
		if err == agent.ErrSpawnFailed {
			return true
		}
		u, ok := err.(interface{ Unwrap() error })
		if !ok {
			return strings.Contains(err.Error(), agent.ErrSpawnFailed.Error())
		}
		err = u.Unwrap()
	}
	return false
}

func readTrace(t *testing.T, path string) string {
	t.Helper()
	raw, err := os.ReadFile(path) //nolint:gosec // test-owned temp file
	if err != nil {
		t.Fatalf("read trace: %v", err)
	}
	return string(raw)
}

func pluginDirFromTrace(t *testing.T, traceFile string, idx int) string {
	t.Helper()
	lines := strings.Split(strings.TrimSpace(readTrace(t, traceFile)), "\n")
	if idx >= len(lines) {
		t.Fatalf("trace has %d lines, want index %d", len(lines), idx)
	}
	return pluginDirFromLine(lines[idx])
}

func pluginDirFromLine(line string) string {
	parts := strings.Fields(line)
	for i, p := range parts {
		if p == "--plugin-dir" && i+1 < len(parts) {
			return parts[i+1]
		}
	}
	return ""
}

func assertPluginDirOutsideWorktree(t *testing.T, dir, worktree string) {
	t.Helper()
	if dir == "" {
		t.Fatal("plugin dir is empty")
	}
	if !filepath.IsAbs(dir) {
		t.Fatalf("plugin dir %q is not absolute", dir)
	}
	rel, err := filepath.Rel(worktree, dir)
	if err != nil {
		t.Fatalf("Rel: %v", err)
	}
	if !strings.HasPrefix(rel, "..") {
		t.Fatalf("plugin dir %q is inside worktree %q", dir, worktree)
	}
}

// TestSpawn_Interactive_PluginDirDeliveredAndCleaned proves the interactive
// REPL carries --plugin-dir and removes the directory when the child exits:
// the flag must not diverge between the headless and interactive lanes.
func TestSpawn_Interactive_PluginDirDeliveredAndCleaned(t *testing.T) {
	t.Parallel()
	if isWindows() {
		t.Skip("pty spawn tests are unix-only")
	}
	workdir := t.TempDir()
	p := newFakeInteractiveProvider(t, `printf '%s\n' "$@" > "$PWD/argv.txt"`)
	h, err := p.Spawn(context.Background(), agent.Spec{
		Prompt:     "hello",
		Cwd:        workdir,
		CardSkills: []agent.CardSkill{{ID: "go-review", Body: "review carefully"}},
		Interactive: &agent.InteractiveSpec{Cols: 80, Rows: 24},
	})
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	t.Cleanup(func() { _ = h.Stop(context.Background()) })
	deadline := time.After(15 * time.Second)
	for {
		select {
		case _, ok := <-h.Events():
			if !ok {
				goto exited
			}
		case <-deadline:
			t.Fatal("timed out waiting for fake Claude exit")
		}
	}
exited:
	raw, err := os.ReadFile(filepath.Join(workdir, "argv.txt"))
	if err != nil {
		t.Fatalf("read echoed argv: %v", err)
	}
	text := string(raw)
	if !strings.Contains(text, "--plugin-dir") {
		t.Fatalf("interactive child argv missing --plugin-dir:\n%s", text)
	}
	// The directory recorded in argv must be gone after exit (cleanup).
	dir := ""
	lines := strings.Split(strings.TrimSpace(text), "\n")
	for i, line := range lines {
		if strings.TrimSpace(line) == "--plugin-dir" && i+1 < len(lines) {
			dir = strings.TrimSpace(lines[i+1])
			break
		}
	}
	if dir == "" {
		t.Fatalf("could not parse plugin dir from argv:\n%s", text)
	}
	if !filepath.IsAbs(dir) {
		t.Fatalf("plugin dir %q is not absolute", dir)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("per-session plugin dir was not removed on child exit: %q", dir)
	}
	assertNoSkillFileInWorktree(t, workdir)
}

func isWindows() bool {
	return runtime.GOOS == "windows"
}

func assertNoSkillFileInWorktree(t *testing.T, worktree string) {
	t.Helper()
	var found []string
	_ = filepath.Walk(worktree, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return nil
		}
		if !info.IsDir() && (info.Name() == "SKILL.md" || info.Name() == "plugin.json") {
			found = append(found, path)
		}
		return nil
	})
	if len(found) > 0 {
		t.Fatalf("skill files inside worktree: %q", found)
	}
}
