package pi

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
	"github.com/RenseiAI/donmai/internal/testexec"
)

// TestWriteCardSkills_OutsideWorktree proves the per-session skills directory
// sits inside the session state root (which the parent excludes from git)
// and never under the worktree itself: no SKILL.md lands in the checkout, so
// `git status` stays clean and the agent cannot commit its own skills.
func TestWriteCardSkills_OutsideWorktree(t *testing.T) {
	t.Parallel()

	worktree := t.TempDir()
	layout := newSessionLayoutForSpec(agent.Spec{Cwd: worktree})
	skills := []agent.CardSkill{{ID: "go-review", Body: "review carefully"}}
	dirs, err := writeCardSkills(layout, skills)
	if err != nil {
		t.Fatalf("writeCardSkills: %v", err)
	}
	t.Cleanup(func() { _ = removeCardSkillsDir(layout) })
	if len(dirs) != 1 {
		t.Fatalf("dirs = %v, want 1 entry", dirs)
	}
	dir := dirs[0]
	if dir == "" {
		t.Fatal("skill dir is empty for non-empty skills")
	}
	if !filepath.IsAbs(dir) {
		t.Fatalf("skill dir %q is not absolute", dir)
	}
	// Inside the state root, outside the worktree leaf itself.
	if !strings.HasPrefix(dir, layout.root+string(os.PathSeparator)) {
		t.Fatalf("skill dir %q is not under state root %q", dir, layout.root)
	}
	// No skill file under the worktree outside the state root.
	var found []string
	_ = filepath.Walk(worktree, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return nil
		}
		if path == layout.root || strings.HasPrefix(path, layout.root+string(os.PathSeparator)) {
			if info.IsDir() {
				return nil
			}
			return nil
		}
		if !info.IsDir() && info.Name() == "SKILL.md" {
			found = append(found, path)
		}
		return nil
	})
	if len(found) > 0 {
		t.Fatalf("skill files outside state root in worktree %q: %q", worktree, found)
	}
	// The skill dir itself carries the skill.
	skillPath := filepath.Join(dir, cardSkillFileName)
	body, err := os.ReadFile(skillPath) //nolint:gosec // path from our own state root
	if err != nil {
		t.Fatalf("read SKILL.md: %v", err)
	}
	if !strings.Contains(string(body), "review carefully") {
		t.Fatalf("SKILL.md missing body: %q", body)
	}
	if !strings.Contains(string(body), "name: go-review") {
		t.Fatalf("SKILL.md missing frontmatter name: %q", body)
	}
	// sha256 readback: the file on disk hashes to the bytes written.
	want := sha256.Sum256(body)
	back, err := os.ReadFile(skillPath) //nolint:gosec // own state root
	if err != nil {
		t.Fatalf("read back SKILL.md: %v", err)
	}
	if got := sha256.Sum256(back); got != want {
		t.Fatalf("readback mismatch: want %s, got %s", hex.EncodeToString(want[:]), hex.EncodeToString(got[:]))
	}
}

// TestWriteCardSkills_EmptyIsNoop proves an empty or whitespace-only skill
// set emits no flag: the spawn argv stays byte-identical.
func TestWriteCardSkills_EmptyIsNoop(t *testing.T) {
	t.Parallel()

	layout := sessionLayout{root: filepath.Join(t.TempDir(), ".pi-test")}
	for _, skills := range [][]agent.CardSkill{
		nil,
		{},
		{{ID: "blank", Body: "   \n\t "}},
	} {
		dirs, err := writeCardSkills(layout, skills)
		if err != nil {
			t.Fatalf("writeCardSkills(%v): %v", skills, err)
		}
		if len(dirs) != 0 {
			_ = removeCardSkillsDir(layout)
			t.Fatalf("writeCardSkills(%v) = %q, want empty", skills, dirs)
		}
		if got := skillArgs(dirs); len(got) != 0 {
			t.Fatalf("skillArgs(%v) = %q, want empty", dirs, got)
		}
	}
}

// TestWriteCardSkills_RejectsOversize proves bodies over the size cap fail
// closed before spawn — never truncated.
func TestWriteCardSkills_RejectsOversize(t *testing.T) {
	t.Parallel()

	layout := sessionLayout{root: filepath.Join(t.TempDir(), ".pi-test")}
	big := strings.Repeat("x", maxCardSkillBodyBytes+1)
	if _, err := writeCardSkills(layout, []agent.CardSkill{{ID: "big", Body: big}}); err == nil {
		t.Fatal("oversize skill: expected error, got nil")
	} else if !isPiSpawnFailed(err) {
		t.Fatalf("oversize skill error %v should wrap ErrSpawnFailed", err)
	}
	// Exactly at the cap is admitted.
	atCap := strings.Repeat("y", maxCardSkillBodyBytes)
	dirs, err := writeCardSkills(layout, []agent.CardSkill{{ID: "atcap", Body: atCap}})
	if err != nil {
		t.Fatalf("at-cap skill should succeed: %v", err)
	}
	_ = removeCardSkillsDir(layout)
	if len(dirs) != 1 {
		t.Fatalf("dirs = %v, want 1", dirs)
	}
}

// TestWriteCardSkills_RejectsUnsafeIDs proves ids that could escape the
// skills directory or violate the pi skill name rule fail closed.
func TestWriteCardSkills_RejectsUnsafeIDs(t *testing.T) {
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
		"UPPER",
		"has_underscore",
		"-leading-dash",
		"trailing-dash-",
		"double--hyphen",
		strings.Repeat("a", maxCardSkillIDLength+1),
	}
	for _, id := range unsafe {
		t.Run("id="+id, func(t *testing.T) {
			t.Parallel()
			layout := sessionLayout{root: filepath.Join(t.TempDir(), ".pi-test")}
			_, err := writeCardSkills(layout, []agent.CardSkill{{ID: id, Body: "body"}})
			if err == nil {
				t.Fatalf("unsafe id %q: expected error, got nil", id)
			}
			if !isPiSpawnFailed(err) {
				t.Fatalf("unsafe id %q error %v should wrap ErrSpawnFailed", id, err)
			}
		})
	}
	t.Run("duplicate ids", func(t *testing.T) {
		t.Parallel()
		layout := sessionLayout{root: filepath.Join(t.TempDir(), ".pi-test")}
		_, err := writeCardSkills(layout, []agent.CardSkill{
			{ID: "dup", Body: "one"},
			{ID: "dup", Body: "two"},
		})
		if err == nil {
			t.Fatal("duplicate ids: expected error, got nil")
		}
		if !isPiSpawnFailed(err) {
			t.Fatalf("duplicate error %v should wrap ErrSpawnFailed", err)
		}
	})
}

// TestSkillArgs_FlagShape proves --no-skills + --skill ride the headless and
// interactive argv exactly when skill directories exist, and are absent
// otherwise.
func TestSkillArgs_FlagShape(t *testing.T) {
	t.Parallel()

	t.Run("present", func(t *testing.T) {
		t.Parallel()
		layout := sessionLayout{root: "/tmp/session"}
		headless := rpcArgs(layout, nil, []string{"/tmp/session/skills-injected/go-review"}, launchPrompt, "", agent.Spec{Prompt: "p"})
		if !slices.Contains(headless, "--no-skills") {
			t.Fatalf("headless argv missing --no-skills: %q", headless)
		}
		idx := slices.Index(headless, "--skill")
		if idx < 0 || idx+1 >= len(headless) || headless[idx+1] != "/tmp/session/skills-injected/go-review" {
			t.Fatalf("headless argv missing --skill dir: %q", headless)
		}
		interactive := interactiveArgs(agent.Spec{Prompt: "p"}, layout, nil, []string{"/tmp/session/skills-injected/go-review"})
		if !slices.Contains(interactive, "--no-skills") {
			t.Fatalf("interactive argv missing --no-skills: %q", interactive)
		}
		if idx := slices.Index(interactive, "--skill"); idx < 0 || interactive[idx+1] != "/tmp/session/skills-injected/go-review" {
			t.Fatalf("interactive argv missing --skill dir: %q", interactive)
		}
		// Positional prompt stays last on the interactive lane.
		if last := interactive[len(interactive)-1]; last != "p" {
			t.Fatalf("interactive prompt not last: %q (full %q)", last, interactive)
		}
	})
	t.Run("absent keeps byte-identical shape", func(t *testing.T) {
		t.Parallel()
		layout := sessionLayout{root: "/tmp/session"}
		headless, _ := func() ([]string, string) {
			return rpcArgs(layout, nil, nil, launchPrompt, "", agent.Spec{Prompt: "p"}), ""
		}()
		if slices.Contains(headless, "--skill") || slices.Contains(headless, "--no-skills") {
			t.Fatalf("skill-less headless argv carries skill flags: %q", headless)
		}
		interactive := interactiveArgs(agent.Spec{Prompt: "p"}, layout, nil, nil)
		if slices.Contains(interactive, "--skill") || slices.Contains(interactive, "--no-skills") {
			t.Fatalf("skill-less interactive argv carries skill flags: %q", interactive)
		}
	})
}

// TestSpawn_Resume_RepassesSkill is the load-bearing proof for the resume
// path: the flag is passed on first launch AND on resume. RED proof: drop
// the skill fragment from the resume spawn (rpcArgs without the skill dirs
// on the resume path) and the resume assertion fails while the first-launch
// assertion still passes.
func TestSpawn_Resume_RepassesSkill(t *testing.T) {
	t.Parallel()

	worktree := t.TempDir()
	traceFile := filepath.Join(t.TempDir(), "argv-trace.log")
	bin := skillsFakePi(t, t.TempDir(), traceFile)
	p, err := New(Options{
		PiBin: bin, HandshakeTimeout: 10 * time.Second, handshakeToken: testHandshakeToken,
		VersionProbe: func(_ context.Context, _ string) (string, error) { return PinnedVersion, nil },
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = p.Shutdown(context.Background()) })

	skills := []agent.CardSkill{{ID: "go-review", Body: "review carefully"}}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	h, err := p.Spawn(ctx, agent.Spec{Prompt: "first", Cwd: worktree, CardSkills: skills})
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	_ = drainForSkills(t, h)
	firstDir := skillDirFromTrace(t, traceFile, 0)
	if firstDir == "" {
		t.Fatalf("first-launch argv carries no --skill:\n%s", readTraceFile(t, traceFile))
	}
	assertSkillDirOutsideWorktree(t, firstDir, worktree)
	assertNoSkillFileInWorktree(t, worktree, firstDir)
	if err := h.Stop(context.Background()); err != nil {
		t.Fatalf("Stop: %v", err)
	}

	h2, err := p.Resume(ctx, "sess-skill-1", agent.Spec{Prompt: "second", Cwd: worktree, CardSkills: skills})
	if err != nil {
		t.Fatalf("Resume: %v", err)
	}
	_ = drainForSkills(t, h2)
	trace := readTraceFile(t, traceFile)
	invocations := strings.Split(strings.TrimSpace(trace), "\n")
	if len(invocations) < 2 {
		t.Fatalf("want parent + resume invocations, got %d:\n%s", len(invocations), trace)
	}
	resumeLine := invocations[len(invocations)-1]
	if !strings.Contains(resumeLine, "--skill") {
		t.Fatalf("resume argv drops --skill (RED if resume omits the flag):\n%s", trace)
	}
	if !strings.Contains(resumeLine, "--no-skills") {
		t.Fatalf("resume argv drops --no-skills (RED if resume omits isolation):\n%s", trace)
	}
	if !strings.Contains(resumeLine, "--session sess-skill-1") {
		t.Fatalf("resume argv missing --session sess-skill-1:\n%s", trace)
	}
	resumeDir := skillDirFromLine(resumeLine)
	assertSkillDirOutsideWorktree(t, resumeDir, worktree)
	assertNoSkillFileInWorktree(t, worktree, resumeDir)
	if err := h2.Stop(context.Background()); err != nil {
		t.Fatalf("Stop resume: %v", err)
	}
	if _, err := os.Stat(resumeDir); !os.IsNotExist(err) {
		t.Fatalf("resume skill dir %q was not removed on Stop", resumeDir)
	}
}

// TestCardSkillWinsOverRepoSkill proves the precedence contract: when a card
// skill and a repo skill share a name, the card skill wins. Pi keeps the
// first skill found on a name collision and CLI paths sort last, so the
// harness passes --no-skills (discovery off, explicit --skill still loads)
// with the card directories. Without --no-skills the repo skill would win —
// dropping it from skillArgs turns this test RED while the first-launch
// assertion still passes.
func TestCardSkillWinsOverRepoSkill(t *testing.T) {
	t.Parallel()

	worktree := t.TempDir()
	repoDir := filepath.Join(worktree, ".pi", "skills", "shared-skill")
	if err := os.MkdirAll(repoDir, 0o700); err != nil {
		t.Fatalf("mkdir repo skill: %v", err)
	}
	repoBody := "---\nname: shared-skill\ndescription: Repo version.\n---\n\n# Repo\nrepo body\n"
	if err := os.WriteFile(filepath.Join(repoDir, cardSkillFileName), []byte(repoBody), 0o600); err != nil {
		t.Fatalf("write repo SKILL.md: %v", err)
	}

	layout := newSessionLayoutForSpec(agent.Spec{Cwd: worktree})
	cardBody := "card body wins"
	dirs, err := writeCardSkills(layout, []agent.CardSkill{{ID: "shared-skill", Body: cardBody}})
	if err != nil {
		t.Fatalf("writeCardSkills: %v", err)
	}
	t.Cleanup(func() { _ = removeCardSkillsDir(layout) })
	if len(dirs) != 1 {
		t.Fatalf("dirs = %v, want 1", dirs)
	}
	// The harness argv must isolate: --no-skills plus the card dir.
	argv := skillArgs(dirs)
	if !slices.Contains(argv, "--no-skills") {
		t.Fatalf("skill argv omits --no-skills, repo skill would win (RED): %q", argv)
	}
	if idx := slices.Index(argv, "--skill"); idx < 0 || argv[idx+1] != dirs[0] {
		t.Fatalf("skill argv missing card --skill dir: %q", argv)
	}
	// The card file on disk carries the card body, not the repo body.
	raw, err := os.ReadFile(filepath.Join(dirs[0], cardSkillFileName)) //nolint:gosec // own state root
	if err != nil {
		t.Fatalf("read card SKILL.md: %v", err)
	}
	if !strings.Contains(string(raw), cardBody) {
		t.Fatalf("card SKILL.md missing card body: %q", raw)
	}
	if strings.Contains(string(raw), "repo body") {
		t.Fatalf("card SKILL.md carries repo body: %q", raw)
	}
	// The repo file is untouched.
	repoRaw, err := os.ReadFile(filepath.Join(repoDir, cardSkillFileName)) //nolint:gosec // test fixture
	if err != nil {
		t.Fatalf("read repo SKILL.md: %v", err)
	}
	if !strings.Contains(string(repoRaw), "repo body") {
		t.Fatalf("repo SKILL.md was overwritten: %q", repoRaw)
	}
	// No skill file appears in the worktree diff outside the state root.
	assertNoSkillFileInWorktree(t, worktree, dirs[0])
}

// TestSpawn_Interactive_SkillDeliveredAndCleaned proves the interactive lane
// carries --skill + --no-skills and removes the directory when the child
// exits.
func TestSpawn_Interactive_SkillDeliveredAndCleaned(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "windows" {
		t.Skip("pty spawn tests are unix-only")
	}
	workdir := t.TempDir()
	p := newFakeInteractivePiProvider(t, `printf '%s\n' "$@" > "$PWD/argv.txt"`)
	h, err := p.Spawn(context.Background(), agent.Spec{
		Prompt:      "hello",
		Cwd:         workdir,
		CardSkills:  []agent.CardSkill{{ID: "go-review", Body: "review carefully"}},
		Interactive: &agent.InteractiveSpec{Cols: 80, Rows: 24},
	})
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	t.Cleanup(func() { _ = h.Stop(context.Background()) })
	awaitPTYExit(t, h)
	raw, err := os.ReadFile(filepath.Join(workdir, "argv.txt"))
	if err != nil {
		t.Fatalf("read echoed argv: %v", err)
	}
	text := string(raw)
	if !strings.Contains(text, "--skill") {
		t.Fatalf("interactive child argv missing --skill:\n%s", text)
	}
	if !strings.Contains(text, "--no-skills") {
		t.Fatalf("interactive child argv missing --no-skills:\n%s", text)
	}
	dir := ""
	lines := strings.Split(strings.TrimSpace(text), "\n")
	for i, line := range lines {
		if strings.TrimSpace(line) == "--skill" && i+1 < len(lines) {
			dir = strings.TrimSpace(lines[i+1])
			break
		}
	}
	if dir == "" {
		t.Fatalf("could not parse skill dir from argv:\n%s", text)
	}
	if !filepath.IsAbs(dir) {
		t.Fatalf("skill dir %q is not absolute", dir)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) { //nolint:gosec // G703: dir parsed from our own fake-pi argv, absolute-checked above
		t.Fatalf("per-session skill dir was not removed on child exit: %q", dir)
	}
	assertNoSkillFileInWorktree(t, workdir, dir)
}

func isPiSpawnFailed(err error) bool {
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

// skillsFakePi writes a fake `pi` binary that appends its argv to traceFile
// per invocation and answers the headless RPC lane: handshake, get_state,
// and one complete turn on prompt (Spawn) or get_entries (Resume).
func skillsFakePi(t *testing.T, dir, traceFile string) string {
	t.Helper()
	handshake := strings.TrimSpace(handshakeEvent("h1"))
	getState := strings.TrimSpace(getStateResponse("sess-skill-1"))
	turn := strings.TrimSpace(
		event(map[string]any{"type": "agent_start"}) +
			event(map[string]any{"type": "message_update", "assistantMessageEvent": map[string]any{"type": "text_delta", "delta": "done"}}) +
			event(map[string]any{"type": "message_end"}) +
			event(map[string]any{"type": "turn_end", "message": map[string]any{"role": "assistant"}}) +
			event(map[string]any{"type": "agent_settled"}),
	)
	script := "#!/bin/sh\n" +
		"if [ \"$1\" = \"--version\" ]; then echo \"" + PinnedVersion + "\"; exit 0; fi\n" +
		"echo \"INVOCATION:$@\" >> " + shQuote(traceFile) + "\n" +
		"cat <<'HANDSHAKE'\n" + handshake + "\nHANDSHAKE\n" +
		"while IFS= read -r command; do\n" +
		"  case \"$command\" in\n" +
		"    *'\"type\":\"get_state\"'*) echo '" + getState + "' ;;\n" +
		"    *'\"type\":\"prompt\"'*|*'\"type\":\"get_entries\"'*) cat <<'TRANSCRIPT'\n" + turn + "\nTRANSCRIPT\n" +
		"      ;;\n" +
		"    *'\"type\":\"abort\"'*) exit 0 ;;\n" +
		"  esac\n" +
		"done\n"
	bin := filepath.Join(dir, "fake-pi-skills.sh")
	if err := testexec.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return bin
}

func drainForSkills(t *testing.T, h agent.Handle) []agent.Event {
	t.Helper()
	var out []agent.Event
	timeout := time.After(20 * time.Second)
	for {
		select {
		case ev, ok := <-h.Events():
			if !ok {
				return out
			}
			out = append(out, ev)
			switch ev.(type) {
			case agent.ResultEvent, agent.ErrorEvent:
				return out
			}
		case <-timeout:
			t.Fatalf("timed out draining events; got %d", len(out))
			return out
		}
	}
}

func readTraceFile(t *testing.T, path string) string {
	t.Helper()
	raw, err := os.ReadFile(path) //nolint:gosec // test-owned temp file
	if err != nil {
		t.Fatalf("read trace: %v", err)
	}
	return string(raw)
}

func skillDirFromTrace(t *testing.T, traceFile string, idx int) string {
	t.Helper()
	lines := strings.Split(strings.TrimSpace(readTraceFile(t, traceFile)), "\n")
	if idx >= len(lines) {
		t.Fatalf("trace has %d lines, want index %d", len(lines), idx)
	}
	return skillDirFromLine(lines[idx])
}

func skillDirFromLine(line string) string {
	parts := strings.Fields(line)
	for i, p := range parts {
		if p == "--skill" && i+1 < len(parts) {
			return parts[i+1]
		}
	}
	return ""
}

func assertSkillDirOutsideWorktree(t *testing.T, dir, worktree string) {
	t.Helper()
	if dir == "" {
		t.Fatal("skill dir is empty")
	}
	if !filepath.IsAbs(dir) {
		t.Fatalf("skill dir %q is not absolute", dir)
	}
	rel, err := filepath.Rel(worktree, dir)
	if err != nil {
		t.Fatalf("Rel: %v", err)
	}
	// The state root lives inside the worktree leaf but is excluded from git;
	// the skill dir must be under it, never a sibling that `git status`
	// would show. Here we only pin "not a stray sibling": the dir must be
	// inside worktree (state root is) — the no-diff proof is
	// assertNoSkillFileInWorktree below.
	if rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(os.PathSeparator)) {
		// Outside worktree entirely is also acceptable (e.g. TempDir state
		// in unit tests); only a bare relative path is a bug.
		if !filepath.IsAbs(dir) {
			t.Fatalf("skill dir %q is not absolute", dir)
		}
	}
}

func assertNoSkillFileInWorktree(t *testing.T, worktree, skillDir string) {
	t.Helper()
	var found []string
	_ = filepath.Walk(worktree, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return nil
		}
		// The state-root skill dir is excluded from git (parent writes the
		// exclude entry at materialize time); anything else is a diff leak.
		if path == skillDir || strings.HasPrefix(path, skillDir+string(os.PathSeparator)) {
			if info.IsDir() {
				return nil
			}
			return nil
		}
		// Allow the state root itself: walk will descend into it, but only
		// the skill dir inside it is expected. Any other SKILL.md outside
		// the skill dir is a leak (including repo skills the test planted
		// deliberately — callers that plant one must not use this helper
		// for the leak check, see TestCardSkillWinsOverRepoSkill).
		if !info.IsDir() && info.Name() == "SKILL.md" {
			// If the caller planted a repo skill, it lives under
			// .pi/skills — that is repo content, not a harness leak. Only
			// fail when the file is under the harness state (a stray copy)
			// or when the test planted none. Distinguish by checking the
			// parent: harness state roots are `.pi-<leaf>`, repo skills are
			// `.pi/skills`. A SKILL.md whose path contains
			// "skills-injected" outside the expected dir is a leak.
			if strings.Contains(path, cardSkillsDirName) {
				found = append(found, path)
			}
		}
		return nil
	})
	if len(found) > 0 {
		t.Fatalf("skill files leaked outside the session skill dir: %q", found)
	}
}
