package afcli

// Tests for `af admin` subcommand tree.
//
// Strategy:
//   - Queue/merge-queue tests spin up a miniredis instance and set REDIS_URL
//     to point at it.  They exercise command output rather than the AdminClient
//     internals (which are covered in afclient/queue/admin_test.go).
//   - cleanup tests only verify subcommand wiring and JSON shape; full git
//     shelling-out behaviour is integration-level and not tested here.
//   - All tests that call t.Setenv must NOT call t.Parallel (Go race detector
//     requirement).

import (
	"bytes"
	"encoding/json"
	"strconv"
	"strings"
	"testing"

	"github.com/alicebob/miniredis/v2"
	"github.com/spf13/cobra"
)

// ──────────────────────────────────────────────────────────────────────────────
// helpers
// ──────────────────────────────────────────────────────────────────────────────

// runAdminCmd wires the admin command into a root and executes it with args,
// capturing stdout.
func runAdminCmd(t *testing.T, args ...string) (string, error) {
	t.Helper()
	root := &cobra.Command{Use: "af"}
	root.AddCommand(newAdminCmd())

	buf := &bytes.Buffer{}
	root.SetOut(buf)
	root.SetErr(&bytes.Buffer{})

	fullArgs := append([]string{"admin"}, args...)
	root.SetArgs(fullArgs)
	err := root.Execute()
	return buf.String(), err
}

// startMiniredis starts a miniredis server, sets REDIS_URL in the test
// environment, and returns the server.  The server (and env var) are
// cleaned up automatically when the test ends.
// Note: must NOT be called from a parallel test (uses t.Setenv).
func startMiniredis(t *testing.T) *miniredis.Miniredis {
	t.Helper()
	mr := miniredis.RunT(t)
	t.Setenv("REDIS_URL", "redis://"+mr.Addr())
	return mr
}

// parseJSONObject decodes the first JSON object from s.
func parseJSONObject(t *testing.T, s string) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal([]byte(strings.TrimSpace(s)), &m); err != nil {
		t.Fatalf("parse JSON object: %v\ninput: %q", err, s)
	}
	return m
}

// ──────────────────────────────────────────────────────────────────────────────
// Admin command tree wiring
// ──────────────────────────────────────────────────────────────────────────────

func TestAdminCmd_SubcommandWiring(t *testing.T) {
	t.Parallel()

	cmd := newAdminCmd()

	wantSubs := []string{"cleanup", "queue", "merge-queue"}
	for _, want := range wantSubs {
		found := false
		for _, sub := range cmd.Commands() {
			if sub.Name() == want {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("admin missing subcommand %q", want)
		}
	}
}

func TestAdminQueueCmd_SubcommandWiring(t *testing.T) {
	t.Parallel()

	cmd := newAdminQueueCmd()
	wantSubs := []string{"list", "peek", "requeue", "drop"}
	for _, want := range wantSubs {
		found := false
		for _, sub := range cmd.Commands() {
			if sub.Name() == want {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("admin queue missing subcommand %q", want)
		}
	}
}

func TestAdminMergeQueueCmd_SubcommandWiring(t *testing.T) {
	t.Parallel()

	cmd := newAdminMergeQueueCmd()
	wantSubs := []string{"list", "dequeue", "force-merge"}
	for _, want := range wantSubs {
		found := false
		for _, sub := range cmd.Commands() {
			if sub.Name() == want {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("admin merge-queue missing subcommand %q", want)
		}
	}
}

// ──────────────────────────────────────────────────────────────────────────────
// queue list — empty state
// ──────────────────────────────────────────────────────────────────────────────

func TestAdminQueueList_Empty(t *testing.T) {
	startMiniredis(t)

	out, err := runAdminCmd(t, "queue", "list")
	if err != nil {
		t.Fatalf("admin queue list: %v", err)
	}

	m := parseJSONObject(t, out)
	if _, ok := m["items"]; !ok {
		t.Errorf("output missing key 'items'")
	}
	if _, ok := m["sessions"]; !ok {
		t.Errorf("output missing key 'sessions'")
	}
	if _, ok := m["workers"]; !ok {
		t.Errorf("output missing key 'workers'")
	}
}

// ──────────────────────────────────────────────────────────────────────────────
// queue list — items present
// ──────────────────────────────────────────────────────────────────────────────

func TestAdminQueueList_WithItems(t *testing.T) {
	mr := startMiniredis(t)

	// Seed a work item
	itemJSON := `{"issueIdentifier":"REN-77","workType":"development","priority":1}`
	mr.HSet("work:items", "sess-001", itemJSON)
	if _, err := mr.ZAdd("work:queue", 100, "sess-001"); err != nil {
		t.Fatalf("zadd: %v", err)
	}

	out, err := runAdminCmd(t, "queue", "list")
	if err != nil {
		t.Fatalf("admin queue list: %v", err)
	}

	m := parseJSONObject(t, out)
	items, ok := m["items"].([]any)
	if !ok {
		t.Fatalf("items is not array: %T", m["items"])
	}
	if len(items) != 1 {
		t.Errorf("want 1 item, got %d", len(items))
	}
}

// ──────────────────────────────────────────────────────────────────────────────
// queue peek — item present
// ──────────────────────────────────────────────────────────────────────────────

func TestAdminQueuePeek_ItemPresent(t *testing.T) {
	mr := startMiniredis(t)

	itemJSON := `{"issueIdentifier":"REN-88","workType":"qa"}`
	mr.HSet("work:items", "sess-peek", itemJSON)
	if _, err := mr.ZAdd("work:queue", 50, "sess-peek"); err != nil {
		t.Fatalf("zadd: %v", err)
	}

	out, err := runAdminCmd(t, "queue", "peek")
	if err != nil {
		t.Fatalf("admin queue peek: %v", err)
	}

	m := parseJSONObject(t, out)
	if id, _ := m["issueIdentifier"].(string); id != "REN-88" {
		t.Errorf("want issueIdentifier REN-88, got %q", id)
	}
}

// ──────────────────────────────────────────────────────────────────────────────
// queue requeue — --yes bypasses prompt
// ──────────────────────────────────────────────────────────────────────────────

func TestAdminQueueRequeue_NotFound(t *testing.T) {
	startMiniredis(t)

	_, err := runAdminCmd(t, "queue", "requeue", "--yes", "ghost-session")
	// Should return an error wrapping ErrItemNotFound
	if err == nil {
		t.Fatal("expected error for nonexistent session, got nil")
	}
}

func TestAdminQueueRequeue_Success(t *testing.T) {
	mr := startMiniredis(t)

	sessJSON := `{"status":"running","workerId":"w1","issueIdentifier":"REN-10"}`
	if err := mr.Set("agent:session:sess-rq", sessJSON); err != nil {
		t.Fatalf("mr.Set: %v", err)
	}

	out, err := runAdminCmd(t, "queue", "requeue", "--yes", "sess-rq")
	if err != nil {
		t.Fatalf("admin queue requeue: %v", err)
	}

	m := parseJSONObject(t, out)
	if requeued, _ := m["requeued"].(float64); requeued < 1 {
		t.Errorf("want requeued >= 1, got %v", m["requeued"])
	}
}

// ──────────────────────────────────────────────────────────────────────────────
// queue drop — --yes bypasses prompt
// ──────────────────────────────────────────────────────────────────────────────

func TestAdminQueueDrop_NotFound(t *testing.T) {
	startMiniredis(t)

	_, err := runAdminCmd(t, "queue", "drop", "--yes", "nobody")
	if err == nil {
		t.Fatal("expected error for nonexistent session, got nil")
	}
}

func TestAdminQueueDrop_Success(t *testing.T) {
	mr := startMiniredis(t)

	sessJSON := `{"status":"pending","issueIdentifier":"REN-20"}`
	if err := mr.Set("agent:session:sess-drop", sessJSON); err != nil {
		t.Fatalf("mr.Set: %v", err)
	}

	out, err := runAdminCmd(t, "queue", "drop", "--yes", "sess-drop")
	if err != nil {
		t.Fatalf("admin queue drop: %v", err)
	}

	m := parseJSONObject(t, out)
	if dropped, _ := m["dropped"].(float64); dropped < 1 {
		t.Errorf("want dropped >= 1, got %v", m["dropped"])
	}
}

// ──────────────────────────────────────────────────────────────────────────────
// merge-queue list
// ──────────────────────────────────────────────────────────────────────────────

func TestAdminMergeQueueList_Empty(t *testing.T) {
	startMiniredis(t)

	out, err := runAdminCmd(t, "merge-queue", "list", "--repo", "test/repo")
	if err != nil {
		t.Fatalf("admin merge-queue list: %v", err)
	}

	m := parseJSONObject(t, out)
	if repoID, _ := m["repoId"].(string); repoID != "test/repo" {
		t.Errorf("want repoId test/repo, got %q", repoID)
	}
	if depth, _ := m["depth"].(float64); depth != 0 {
		t.Errorf("want depth 0, got %v", m["depth"])
	}
}

func TestAdminMergeQueueList_WithEntries(t *testing.T) {
	mr := startMiniredis(t)
	repo := "org/proj"

	entryJSON := `{"repoId":"org/proj","prNumber":42,"sourceBranch":"feat/mq","priority":1,"enqueuedAt":1000000}`
	mr.HSet("merge:entry:"+repo, strconv.Itoa(42), entryJSON)
	if _, err := mr.ZAdd("merge:queue:"+repo, 42, strconv.Itoa(42)); err != nil {
		t.Fatalf("zadd merge:queue: %v", err)
	}

	out, err := runAdminCmd(t, "merge-queue", "list", "--repo", repo)
	if err != nil {
		t.Fatalf("admin merge-queue list: %v", err)
	}

	m := parseJSONObject(t, out)
	if depth, _ := m["depth"].(float64); depth != 1 {
		t.Errorf("want depth 1, got %v", m["depth"])
	}
	entries, _ := m["entries"].([]any)
	if len(entries) != 1 {
		t.Errorf("want 1 entry, got %d", len(entries))
	}
}

// ──────────────────────────────────────────────────────────────────────────────
// merge-queue dequeue
// ──────────────────────────────────────────────────────────────────────────────

func TestAdminMergeQueueDequeue_NotFound(t *testing.T) {
	startMiniredis(t)

	_, err := runAdminCmd(t, "merge-queue", "dequeue", "--yes", "--repo", "x/y", "99")
	if err == nil {
		t.Fatal("expected error for nonexistent entry, got nil")
	}
}

func TestAdminMergeQueueDequeue_Success(t *testing.T) {
	mr := startMiniredis(t)
	repo := "test/dq"

	mr.HSet("merge:entry:"+repo, "7", `{"prNumber":7,"sourceBranch":"feat/dq"}`)
	if _, err := mr.ZAdd("merge:queue:"+repo, 7, "7"); err != nil {
		t.Fatalf("zadd: %v", err)
	}

	out, err := runAdminCmd(t, "merge-queue", "dequeue", "--yes", "--repo", repo, "7")
	if err != nil {
		t.Fatalf("admin merge-queue dequeue: %v", err)
	}

	m := parseJSONObject(t, out)
	if dequeued, _ := m["dequeued"].(bool); !dequeued {
		t.Errorf("want dequeued=true, got %v", m["dequeued"])
	}
	if pr, _ := m["prNumber"].(float64); int(pr) != 7 {
		t.Errorf("want prNumber 7, got %v", m["prNumber"])
	}
}

// ──────────────────────────────────────────────────────────────────────────────
// merge-queue force-merge
// ──────────────────────────────────────────────────────────────────────────────

func TestAdminMergeQueueForceMerge_NotFound(t *testing.T) {
	startMiniredis(t)

	_, err := runAdminCmd(t, "merge-queue", "force-merge", "--yes", "--repo", "a/b", "100")
	if err == nil {
		t.Fatal("expected error for nonexistent entry, got nil")
	}
}

func TestAdminMergeQueueForceMerge_Success(t *testing.T) {
	mr := startMiniredis(t)
	repo := "test/fm"

	mr.HSet("merge:entry:"+repo, "3", `{"prNumber":3,"sourceBranch":"feat/fm","failureReason":"ci","enqueuedAt":999}`)
	if _, err := mr.ZAdd("merge:failed:"+repo, 3, "3"); err != nil {
		t.Fatalf("zadd merge:failed: %v", err)
	}

	out, err := runAdminCmd(t, "merge-queue", "force-merge", "--yes", "--repo", repo, "3")
	if err != nil {
		t.Fatalf("admin merge-queue force-merge: %v", err)
	}

	m := parseJSONObject(t, out)
	if retried, _ := m["retried"].(bool); !retried {
		t.Errorf("want retried=true, got %v", m["retried"])
	}
}

// ──────────────────────────────────────────────────────────────────────────────
// parsePRNumber
// ──────────────────────────────────────────────────────────────────────────────

func TestParsePRNumber(t *testing.T) {
	t.Parallel()

	cases := []struct {
		input   string
		wantN   int
		wantErr bool
	}{
		{"42", 42, false},
		{"1", 1, false},
		{"0", 0, true},
		{"-5", 0, true},
		{"abc", 0, true},
		{"", 0, true},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.input, func(t *testing.T) {
			t.Parallel()
			n, err := parsePRNumber(tc.input)
			if tc.wantErr && err == nil {
				t.Errorf("parsePRNumber(%q): want error, got nil", tc.input)
			}
			if !tc.wantErr && err != nil {
				t.Errorf("parsePRNumber(%q): unexpected error: %v", tc.input, err)
			}
			if !tc.wantErr && n != tc.wantN {
				t.Errorf("parsePRNumber(%q): want %d, got %d", tc.input, tc.wantN, n)
			}
		})
	}
}
