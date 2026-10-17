package claude

import (
	"context"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strings"
	"testing"

	"github.com/RenseiAI/donmai/agent"
)

// ownedSessionShape is the CLI's session-id shape (UUID). The spawn and
// resume tests below assert the minted token is shaped like one, so a
// regression that mints a bare random string (which the CLI would reject
// or a sibling could guess) fails loudly.
var ownedSessionShape = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)

// ownedSessionFakeCLI is a fake harness process that records its argv and,
// on a fresh spawn, echoes the owned --session-id back on the init line
// the way the real CLI does; on a --resume spawn it echoes the resumed id.
// The resume branch exits non-zero with the CLI's own "No conversation
// found" text when the id is unknown, so the async-missing path is
// observable on the event stream.
const ownedSessionFakeCLI = `#!/bin/sh
printf '%s\n' "$@" > "$PWD/argv.txt"
owned=""
resume=""
prev=""
for arg in "$@"; do
  if [ "$prev" = "--session-id" ]; then owned="$arg"; fi
  if [ "$prev" = "--resume" ]; then resume="$arg"; fi
  prev="$arg"
done
if [ -n "$resume" ]; then
  case "$resume" in
    sess-live-1) id="$resume";;
    *) printf 'No conversation found with session ID: %s\n' "$resume" >&2; exit 1;;
  esac
else
  id="$owned"
fi
printf '{"type":"system","subtype":"init","session_id":"%s"}\n' "$id"
printf '{"type":"result","subtype":"success","is_error":false,"num_turns":1}\n'
`

func readSpawnArgv(t *testing.T, workdir string) []string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(workdir, "argv.txt"))
	if err != nil {
		t.Fatalf("fake CLI did not record argv: %v", err)
	}
	var out []string
	for _, line := range strings.Split(string(raw), "\n") {
		if line != "" {
			out = append(out, line)
		}
	}
	return out
}

// TestSpawn_OwnsSessionIDAtLaunch drives the production headless entry
// point: a fresh spawn must pass a minted --session-id (the resume token
// known before the first turn), and the handle must expose it as its
// session id immediately — before any event is drained.
func TestSpawn_OwnsSessionIDAtLaunch(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("fake CLI uses /bin/sh; skip on windows")
	}
	workdir := t.TempDir()
	cli := writeFakeCLI(t, "fake-claude-owned-session.sh", ownedSessionFakeCLI)
	p, err := New(Options{Binary: cli, LookPath: func(string) (string, error) { return cli, nil }})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	h, err := p.Spawn(context.Background(), agent.Spec{Prompt: "first turn", Cwd: workdir})
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	defer func() { _ = h.Stop(context.Background()) }()

	// The resume token is known before the first turn: no event drained.
	owned := h.SessionID()
	if owned == "" {
		t.Fatal("SessionID is empty right after Spawn; the owned token must be known before the first turn")
	}
	if !ownedSessionShape.MatchString(owned) {
		t.Fatalf("owned session id %q is not UUIDv4-shaped", owned)
	}

	// The CLI echoes the owned id on the init line: confirmation, not a
	// second identity.
	waitForHeadlessTerminal(t, h)
	if h.SessionID() != owned {
		t.Fatalf("SessionID after init = %q, want the owned %q", h.SessionID(), owned)
	}

	// Read argv after the terminal event: the fake records it on entry,
	// but asserting before the process is scheduled is a race.
	argv := readSpawnArgv(t, workdir)
	idx := slices.Index(argv, "--session-id")
	if idx < 0 || idx+1 >= len(argv) {
		t.Fatalf("fresh spawn argv missing --session-id: %v", argv)
	}
	if argv[idx+1] != owned {
		t.Fatalf("--session-id = %q, want the owned %q", argv[idx+1], owned)
	}
	if slices.Contains(argv, "--resume") {
		t.Fatalf("fresh spawn must not carry --resume: %v", argv)
	}
}

// TestSpawn_OwnedSessionIDsAreUnique mints across spawns: two fresh
// sessions must not share a resume token.
func TestSpawn_OwnedSessionIDsAreUnique(t *testing.T) {
	t.Parallel()

	seen := map[string]struct{}{}
	for range 25 {
		id, err := generateOwnedSessionID()
		if err != nil {
			t.Fatalf("generateOwnedSessionID: %v", err)
		}
		if !ownedSessionShape.MatchString(id) {
			t.Fatalf("minted id %q is not UUIDv4-shaped", id)
		}
		if _, dup := seen[id]; dup {
			t.Fatalf("duplicate minted session id %q", id)
		}
		seen[id] = struct{}{}
	}
}

// TestResume_ContinuesOwnedConversation drives the production restore
// path: Resume must spawn `--resume <id>` (never a second --session-id)
// and the handle must carry the resumed id from return.
func TestResume_ContinuesOwnedConversation(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("fake CLI uses /bin/sh; skip on windows")
	}
	workdir := t.TempDir()
	cli := writeFakeCLI(t, "fake-claude-resume-owned.sh", ownedSessionFakeCLI)
	p, err := New(Options{Binary: cli, LookPath: func(string) (string, error) { return cli, nil }})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	h, err := p.Resume(context.Background(), "sess-live-1", agent.Spec{Prompt: "continue", Cwd: workdir})
	if err != nil {
		t.Fatalf("Resume: %v", err)
	}
	defer func() { _ = h.Stop(context.Background()) }()

	if h.SessionID() != "sess-live-1" {
		t.Fatalf("SessionID = %q, want the resumed %q before any event", h.SessionID(), "sess-live-1")
	}
	waitForHeadlessTerminal(t, h)
	argv := readSpawnArgv(t, workdir)
	idx := slices.Index(argv, "--resume")
	if idx < 0 || idx+1 >= len(argv) || argv[idx+1] != "sess-live-1" {
		t.Fatalf("resume spawn argv missing --resume sess-live-1: %v", argv)
	}
	if slices.Contains(argv, "--session-id") {
		t.Fatalf("resume spawn must not mint a second identity via --session-id: %v", argv)
	}
}

// TestBuildArgs_OwnedSessionIDFlag pins the argv mapping: fresh + owned
// emits --session-id, resume emits only --resume, neither emits both.
func TestBuildArgs_OwnedSessionIDFlag(t *testing.T) {
	t.Parallel()

	fresh, _ := buildArgs(agent.Spec{Prompt: "p"}, "", "", "owned-1")
	if idx := slices.Index(fresh, "--session-id"); idx < 0 || idx+1 >= len(fresh) || fresh[idx+1] != "owned-1" {
		t.Fatalf("fresh argv missing --session-id owned-1: %v", fresh)
	}
	if slices.Contains(fresh, "--resume") {
		t.Fatalf("fresh argv must not carry --resume: %v", fresh)
	}

	resumed, _ := buildArgs(agent.Spec{Prompt: "p"}, "", "sess-live-1", "owned-1")
	if slices.Contains(resumed, "--session-id") {
		t.Fatalf("resume argv must not carry --session-id: %v", resumed)
	}
	if idx := slices.Index(resumed, "--resume"); idx < 0 || resumed[idx+1] != "sess-live-1" {
		t.Fatalf("resume argv missing --resume sess-live-1: %v", resumed)
	}

	plain, _ := buildArgs(agent.Spec{Prompt: "p"}, "", "", "")
	if slices.Contains(plain, "--session-id") || slices.Contains(plain, "--resume") {
		t.Fatalf("unowned spawn must carry neither identity flag: %v", plain)
	}
}
