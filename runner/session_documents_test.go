package runner

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/RenseiAI/donmai/runtime/workarea"
)

// trackedWorkareaFixture clones a repository whose committed history tracks
// project content under a top-level .workarea/ and a nested
// sub/.workarea/context/pack1/ (the exact shape of a published pack), then
// edits both. Those edits are the session's real work: nothing about the
// directory name makes them harness state.
func trackedWorkareaFixture(t *testing.T) (clone, base string) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	clone, _, _ = cloneForRescue(t)
	writeFile(t, clone, ".workarea/project-state.txt", "tracked v1\n")
	writeFile(t, clone, "sub/.workarea/context/pack1/brief.md", "nested tracked v1\n")
	gitRun(t, clone, "add", "-A")
	gitRun(t, clone, "commit", "-q", "-m", "track workarea content")
	gitRun(t, clone, "push", "-q", "origin", "HEAD")
	base = gitRun(t, clone, "rev-parse", "HEAD")
	writeFile(t, clone, ".workarea/project-state.txt", "top-level v2 session edit\n")
	writeFile(t, clone, "sub/.workarea/context/pack1/brief.md", "nested v2 session edit\n")
	return clone, base
}

// publishFlatPack materializes a real pack into the flat checkout and returns
// the session result a carrier session would hold afterwards.
func publishFlatPack(t *testing.T, checkout string) *Result {
	t.Helper()
	content := []byte("negotiated brief bytes\n")
	sum := sha256.Sum256(content)
	report, err := workarea.MaterializeContextPack(workarea.RootPath(checkout), workarea.ContextPackV1{
		Version: workarea.ContextPackProtocol,
		PackID:  "pack1",
		Documents: []workarea.ContextDocumentV1{{
			Path:          "brief.md",
			ContentBase64: base64.StdEncoding.EncodeToString(content),
			Digest:        "sha256:" + hex.EncodeToString(sum[:]),
		}},
	})
	if err != nil {
		t.Fatalf("MaterializeContextPack: %v", err)
	}
	res := &Result{}
	res.WorkareaRoot = checkout
	res.WorktreePath = checkout
	res.ContextPack = &report
	return res
}

func archivedPatch(t *testing.T, r *Runner, sessionID string, res *Result) string {
	t.Helper()
	if !r.preserveUnpublishedWork(QueuedWork{QueuedWork: queuedWorkBase(sessionID)}, res) {
		t.Fatal("preserveUnpublishedWork = false; want the work archived")
	}
	patches := rescuePatches(t, r.rescueDir)
	if len(patches) != 1 {
		t.Fatalf("rescue patches = %v; want exactly one (preservation reported success)", patches)
	}
	return readFile(t, patches[0])
}

func treePaths(t *testing.T, dir, rev string) string {
	t.Helper()
	return gitRun(t, dir, "ls-tree", "-r", "--name-only", rev)
}

func checkpointBlob(t *testing.T, dir, rev, path string) string {
	t.Helper()
	return gitRun(t, dir, "show", rev+":"+path)
}

// TestWorkareaEditsStayWorkWithoutSessionDocuments pins the legacy contract:
// a session that carried no negotiated documents treats every .workarea/ path
// exactly as project content, at the top level and nested. The rescue patch
// archives the edits, the WIP checkpoint carries them, and the backstop
// commits them.
//
// RED (with a `.workarea` row restored to the shared harness-state table):
// preservation reports success with no patch, the WIP checkpoint is skipped
// or drops the edits, and the backstop commit leaves them out.
func TestWorkareaEditsStayWorkWithoutSessionDocuments(t *testing.T) {
	clone, base := trackedWorkareaFixture(t)
	res := &Result{}
	res.WorktreePath = clone
	res.WorkareaRoot = clone
	recordRescueTargets(context.Background(), res, []rescueTarget{{path: clone}})
	if got := res.rescueTargets[0].sessionDocuments; got != "" {
		t.Fatalf("non-carrier session documents prefix = %q; want none", got)
	}
	assertWorkareaEditsAreWork(t, clone, base, res, "WORKAREA-LEGACY")
}

// TestSessionDocumentsExcludeOnlyTheirOwnPack pins the carrier contract in
// the flat layout: the published pack directory, and only it, is session
// machinery. Tracked edits elsewhere under .workarea/ and in a nested
// sub/.workarea/context/pack1/ are still archived, checkpointed and
// committed, while the pack stays out of all three and out of `git status`.
//
// RED (with the exclusion widened back to the whole .workarea/ leaf): the
// tracked edits vanish from the patch, the checkpoint and the commit. RED
// (with the session-scoped prefix dropped from any one consumer — the rescue
// filter, the WIP filter or the backstop filter): the pack enters that
// consumer's output. RED (with the pack ignore file no longer written): git
// status shows the pack.
func TestSessionDocumentsExcludeOnlyTheirOwnPack(t *testing.T) {
	clone, base := trackedWorkareaFixture(t)
	res := publishFlatPack(t, clone)
	recordRescueTargets(context.Background(), res, []rescueTarget{{path: clone}})
	if got := res.rescueTargets[0].sessionDocuments; got != ".workarea/context/pack1/" {
		t.Fatalf("carrier session documents prefix = %q; want the pack directory", got)
	}
	status := gitRun(t, clone, "status", "--porcelain", "--untracked-files=all")
	for _, line := range strings.Split(status, "\n") {
		if len(line) > 3 && strings.HasPrefix(line[3:], ".workarea/context/") {
			t.Fatalf("published pack visible to git status:\n%s", status)
		}
	}
	if !strings.Contains(status, ".workarea/project-state.txt") {
		t.Fatalf("tracked .workarea edit hidden from git status:\n%s", status)
	}
	// Worst case: the executor deleted the pack's ignore file and
	// force-staged the pack into the real index. Git now sees the pack
	// everywhere, so the session-scoped filter on each consumer (rescue,
	// WIP checkpoint, backstop) is the only thing keeping it out.
	if err := os.Remove(filepath.Join(clone, ".workarea", "context", "pack1", workarea.ContextPackIgnoreName)); err != nil {
		t.Fatalf("remove pack ignore file: %v", err)
	}
	gitRun(t, clone, "add", "-f", ".workarea/context/pack1/brief.md", ".workarea/context/pack1/pack.json")
	assertWorkareaEditsAreWork(t, clone, base, res, "WORKAREA-CARRIER")
}

func assertWorkareaEditsAreWork(t *testing.T, clone, base string, res *Result, id string) {
	t.Helper()
	const topEdit = "top-level v2 session edit"
	const nestedEdit = "nested v2 session edit"
	packPath := ".workarea/context/pack1/brief.md"

	// Rescue: the scratch-index patch archives both tracked edits.
	r := minimalRunner(t)
	r.rescueDir = t.TempDir()
	patch := archivedPatch(t, r, id, res)
	for _, want := range []string{topEdit, nestedEdit} {
		if !strings.Contains(patch, want) {
			t.Errorf("rescue patch lacks tracked edit %q:\n%s", want, patch)
		}
	}
	if strings.Contains(patch, "negotiated brief bytes") || strings.Contains(patch, " b/"+packPath) {
		t.Errorf("rescue patch archived the session's own pack:\n%s", patch)
	}

	// WIP checkpoint: the pushed checkpoint commit carries both edits.
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	qw := QueuedWork{QueuedWork: queuedWorkBase(id)}
	target := res.rescueTargets[0]
	target.base = base
	checkpoint, warnings, err := createWIPCheckpoint(ctx, qw, target, "refs/wip/"+qw.SessionID+"/1")
	if err != nil {
		t.Fatalf("createWIPCheckpoint: %v (warnings %v)", err, warnings)
	}
	if checkpoint == nil {
		t.Fatalf("WIP checkpoint skipped for real .workarea work (warnings %v)", warnings)
	}
	if got := checkpointBlob(t, clone, checkpoint.CommitSHA, ".workarea/project-state.txt"); got != topEdit {
		t.Errorf("checkpoint .workarea/project-state.txt = %q; want the session edit", got)
	}
	if got := checkpointBlob(t, clone, checkpoint.CommitSHA, "sub/.workarea/context/pack1/brief.md"); got != nestedEdit {
		t.Errorf("checkpoint nested edit = %q; want the session edit", got)
	}
	if tree := treePaths(t, clone, checkpoint.CommitSHA); strings.Contains(tree, "\n"+packPath) || strings.HasPrefix(tree, packPath) {
		t.Errorf("checkpoint carries the session's own pack:\n%s", tree)
	}

	// Backstop: the commit carries both edits and never the pack.
	before := gitRun(t, clone, "rev-parse", "HEAD")
	stubGhRecordingArgs(t, "https://github.com/example-o/test-r/pull/7")
	report := r.runBackstop(ctx, qw, "agent/"+qw.SessionID, res, nil)
	if report.Diagnostics != "" {
		t.Fatalf("backstop diagnostics = %q", report.Diagnostics)
	}
	committed := gitRun(t, clone, "diff", "--name-only", before, "HEAD")
	for _, want := range []string{".workarea/project-state.txt", "sub/.workarea/context/pack1/brief.md"} {
		if !strings.Contains(committed, want) {
			t.Errorf("backstop commit lacks tracked edit %s:\n%s", want, committed)
		}
	}
	for _, line := range strings.Split(committed, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), ".workarea/context/") {
			t.Errorf("backstop committed the session's own pack:\n%s", committed)
		}
	}
}

// TestSessionDocumentPrefixIsFlatCarrierOnly pins who gets a prefix: only a
// session that published a pack inside the very checkout being filtered.
func TestSessionDocumentPrefixIsFlatCarrierOnly(t *testing.T) {
	t.Parallel()
	root := filepath.Join(t.TempDir(), "root")
	pack := &workarea.ContextPackReport{PackID: "pack1", Dir: workarea.ContextPackDir("pack1")}
	cases := []struct {
		name     string
		res      *Result
		checkout string
		want     string
	}{
		{"nil result", nil, root, ""},
		{"no carrier", &Result{}, root, ""},
		{"flat carrier", &Result{ContextPack: pack}, root, ".workarea/context/pack1/"},
		{"declared leaf beside the pack", &Result{ContextPack: pack}, filepath.Join(root, "repo"), ""},
		{"no checkout", &Result{ContextPack: pack}, "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if tc.res != nil {
				tc.res.WorkareaRoot = root
			}
			if got := sessionDocumentPrefix(tc.res, tc.checkout); got != tc.want {
				t.Fatalf("sessionDocumentPrefix = %q; want %q", got, tc.want)
			}
		})
	}
	prefix := ".workarea/context/pack1/"
	for path, want := range map[string]bool{
		".workarea/context/pack1":              true,
		".workarea/context/pack1/brief.md":     true,
		".workarea/context/pack10/brief.md":    false,
		".workarea/project-state.txt":          false,
		"sub/.workarea/context/pack1/brief.md": false,
		"":                                     false,
	} {
		if got := underSessionDocuments(prefix, path); got != want {
			t.Errorf("underSessionDocuments(%q) = %v; want %v", path, got, want)
		}
	}
	if underSessionDocuments("", ".workarea/context/pack1/brief.md") {
		t.Error("an empty prefix matched a path")
	}
}
