package afcli

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/RenseiAI/agentfactory-tui/afclient"
)

// fakeWorkareaClient is a hand-rolled mock satisfying
// workareaDaemonClient. Per AGENTS.md: no testify, no httptest
// reach-throughs.
type fakeWorkareaClient struct {
	listResp    *afclient.ListWorkareasResponse
	listErr     error
	getResp     *afclient.WorkareaEnvelope
	getErr      error
	gotID       string
	restoreResp *afclient.WorkareaRestoreResult
	restoreErr  error
	gotArchive  string
	gotRestore  afclient.WorkareaRestoreRequest
	diffResp    *afclient.WorkareaDiffResult
	diffErr     error
	gotIdA      string
	gotIdB      string
	listCalls   int
	getCalls    int
	restoreCalls int
	diffCalls   int
}

func (f *fakeWorkareaClient) ListWorkareas() (*afclient.ListWorkareasResponse, error) {
	f.listCalls++
	return f.listResp, f.listErr
}

func (f *fakeWorkareaClient) GetWorkarea(id string) (*afclient.WorkareaEnvelope, error) {
	f.getCalls++
	f.gotID = id
	return f.getResp, f.getErr
}

func (f *fakeWorkareaClient) RestoreWorkarea(archiveID string, req afclient.WorkareaRestoreRequest) (*afclient.WorkareaRestoreResult, error) {
	f.restoreCalls++
	f.gotArchive = archiveID
	f.gotRestore = req
	return f.restoreResp, f.restoreErr
}

func (f *fakeWorkareaClient) DiffWorkareas(idA, idB string) (*afclient.WorkareaDiffResult, error) {
	f.diffCalls++
	f.gotIdA = idA
	f.gotIdB = idB
	return f.diffResp, f.diffErr
}

func newWorkareaRootForTest(client *fakeWorkareaClient) *cobra.Command {
	root := &cobra.Command{Use: "afcli-test"}
	root.AddCommand(newWorkareaCmdWithFactory(func(_ afclient.DaemonConfig) workareaDaemonClient {
		return client
	}))
	return root
}

func sampleWorkareaSummary() afclient.WorkareaSummary {
	return afclient.WorkareaSummary{
		ID:         "wa-001",
		Kind:       afclient.WorkareaKindArchived,
		ProviderID: "local-pool",
		Status:     afclient.WorkareaStatusArchived,
		SessionID:  "sess-x",
	}
}

// ── list ──────────────────────────────────────────────────────────────────

func TestWorkareaCmd_List_RendersTable(t *testing.T) {
	client := &fakeWorkareaClient{
		listResp: &afclient.ListWorkareasResponse{
			Active:   []afclient.WorkareaSummary{},
			Archived: []afclient.WorkareaSummary{sampleWorkareaSummary()},
		},
	}
	root := newWorkareaRootForTest(client)
	out, err := runRootCmd(t, root, "workarea", "list")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if client.listCalls != 1 {
		t.Errorf("ListWorkareas calls = %d, want 1", client.listCalls)
	}
	if !strings.Contains(out, "wa-001") {
		t.Errorf("output missing id:\n%s", out)
	}
}

func TestWorkareaCmd_List_JSON(t *testing.T) {
	want := afclient.ListWorkareasResponse{
		Archived: []afclient.WorkareaSummary{sampleWorkareaSummary()},
	}
	client := &fakeWorkareaClient{listResp: &want}
	root := newWorkareaRootForTest(client)
	out, err := runRootCmd(t, root, "workarea", "list", "--json")
	if err != nil {
		t.Fatalf("list --json: %v", err)
	}
	var got afclient.ListWorkareasResponse
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("decode: %v\n%s", err, out)
	}
	if len(got.Archived) != 1 || got.Archived[0].ID != "wa-001" {
		t.Errorf("decoded list mismatch: %+v", got)
	}
}

func TestWorkareaCmd_List_PropagatesError(t *testing.T) {
	client := &fakeWorkareaClient{listErr: errors.New("boom")}
	root := newWorkareaRootForTest(client)
	if _, err := runRootCmd(t, root, "workarea", "list"); err == nil {
		t.Error("expected error")
	}
}

// ── show ──────────────────────────────────────────────────────────────────

func TestWorkareaCmd_Show_RendersDetail(t *testing.T) {
	client := &fakeWorkareaClient{getResp: &afclient.WorkareaEnvelope{
		Workarea: afclient.Workarea{ID: "wa-detail", Kind: afclient.WorkareaKindArchived,
			Status: afclient.WorkareaStatusArchived, Repository: "github.com/acme/x"},
	}}
	root := newWorkareaRootForTest(client)
	out, err := runRootCmd(t, root, "workarea", "show", "wa-detail")
	if err != nil {
		t.Fatalf("show: %v", err)
	}
	if client.gotID != "wa-detail" {
		t.Errorf("id = %q", client.gotID)
	}
	if !strings.Contains(out, "wa-detail") || !strings.Contains(out, "github.com/acme/x") {
		t.Errorf("output missing expected fields:\n%s", out)
	}
}

func TestWorkareaCmd_Show_JSON(t *testing.T) {
	client := &fakeWorkareaClient{getResp: &afclient.WorkareaEnvelope{
		Workarea: afclient.Workarea{ID: "j-1", Kind: afclient.WorkareaKindArchived},
	}}
	root := newWorkareaRootForTest(client)
	out, err := runRootCmd(t, root, "workarea", "show", "j-1", "--json")
	if err != nil {
		t.Fatalf("show --json: %v", err)
	}
	var got afclient.Workarea
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("decode: %v\n%s", err, out)
	}
	if got.ID != "j-1" {
		t.Errorf("id = %q", got.ID)
	}
}

func TestWorkareaCmd_Show_NotFoundFriendlyMessage(t *testing.T) {
	client := &fakeWorkareaClient{getErr: fmt.Errorf("get: %w", afclient.ErrNotFound)}
	root := newWorkareaRootForTest(client)
	_, err := runRootCmd(t, root, "workarea", "show", "ghost")
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "workarea not found: ghost") {
		t.Errorf("err = %v", err)
	}
}

func TestWorkareaCmd_Show_RequiresExactlyOneArg(t *testing.T) {
	client := &fakeWorkareaClient{}
	root := newWorkareaRootForTest(client)
	if _, err := runRootCmd(t, root, "workarea", "show"); err == nil {
		t.Error("expected error with no args")
	}
	if _, err := runRootCmd(t, root, "workarea", "show", "a", "b"); err == nil {
		t.Error("expected error with two args")
	}
}

// ── restore ───────────────────────────────────────────────────────────────

func TestWorkareaCmd_Restore_HappyPath(t *testing.T) {
	client := &fakeWorkareaClient{restoreResp: &afclient.WorkareaRestoreResult{
		Workarea: afclient.Workarea{ID: "wa-new", Kind: afclient.WorkareaKindActive,
			Status: afclient.WorkareaStatusReady},
	}}
	root := newWorkareaRootForTest(client)
	out, err := runRootCmd(t, root, "workarea", "restore", "wa-archive",
		"--reason", "audit", "--into-session-id", "sess-into")
	if err != nil {
		t.Fatalf("restore: %v", err)
	}
	if client.gotArchive != "wa-archive" {
		t.Errorf("archive id = %q", client.gotArchive)
	}
	if client.gotRestore.Reason != "audit" {
		t.Errorf("reason = %q", client.gotRestore.Reason)
	}
	if client.gotRestore.IntoSessionID != "sess-into" {
		t.Errorf("intoSessionId = %q", client.gotRestore.IntoSessionID)
	}
	if !strings.Contains(out, "Workarea restored") {
		t.Errorf("output missing restore confirmation:\n%s", out)
	}
}

func TestWorkareaCmd_Restore_JSON(t *testing.T) {
	client := &fakeWorkareaClient{restoreResp: &afclient.WorkareaRestoreResult{
		Workarea: afclient.Workarea{ID: "wa-jr", Kind: afclient.WorkareaKindActive},
	}}
	root := newWorkareaRootForTest(client)
	out, err := runRootCmd(t, root, "workarea", "restore", "wa-x", "--json")
	if err != nil {
		t.Fatalf("restore --json: %v", err)
	}
	var got afclient.WorkareaRestoreResult
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("decode: %v\n%s", err, out)
	}
	if got.Workarea.ID != "wa-jr" {
		t.Errorf("id = %q", got.Workarea.ID)
	}
}

func TestWorkareaCmd_Restore_ErrorMessages(t *testing.T) {
	cases := []struct {
		name      string
		serverErr error
		wantSubstr string
	}{
		{"not-found", fmt.Errorf("x: %w", afclient.ErrNotFound), "archive not found"},
		{"conflict", fmt.Errorf("x: %w", afclient.ErrConflict), "session id already in use"},
		{"unavailable", fmt.Errorf("x: %w", afclient.ErrUnavailable), "pool saturated"},
		{"bad-request", fmt.Errorf("x: %w", afclient.ErrBadRequest), "archive corrupted"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			client := &fakeWorkareaClient{restoreErr: tc.serverErr}
			root := newWorkareaRootForTest(client)
			_, err := runRootCmd(t, root, "workarea", "restore", "wa-x",
				"--into-session-id", "sess-x")
			if err == nil {
				t.Fatal("expected error")
			}
			if !strings.Contains(err.Error(), tc.wantSubstr) {
				t.Errorf("err = %v, want substring %q", err, tc.wantSubstr)
			}
		})
	}
}

func TestWorkareaCmd_Restore_RequiresExactlyOneArg(t *testing.T) {
	client := &fakeWorkareaClient{}
	root := newWorkareaRootForTest(client)
	if _, err := runRootCmd(t, root, "workarea", "restore"); err == nil {
		t.Error("expected error with no args")
	}
}

// ── diff ──────────────────────────────────────────────────────────────────

func TestWorkareaCmd_Diff_HappyPath(t *testing.T) {
	client := &fakeWorkareaClient{diffResp: &afclient.WorkareaDiffResult{
		Summary: afclient.WorkareaDiffSummary{
			WorkareaA: "a", WorkareaB: "b",
			Added: 1, Total: 1,
		},
		Entries: []afclient.WorkareaDiffEntry{{
			Path: "new.txt", Status: afclient.WorkareaDiffStatusAdded,
		}},
	}}
	root := newWorkareaRootForTest(client)
	out, err := runRootCmd(t, root, "workarea", "diff", "a", "b")
	if err != nil {
		t.Fatalf("diff: %v", err)
	}
	if client.gotIdA != "a" || client.gotIdB != "b" {
		t.Errorf("ids = %q vs %q", client.gotIdA, client.gotIdB)
	}
	if !strings.Contains(out, "new.txt") {
		t.Errorf("output missing entry:\n%s", out)
	}
}

func TestWorkareaCmd_Diff_JSON(t *testing.T) {
	client := &fakeWorkareaClient{diffResp: &afclient.WorkareaDiffResult{
		Summary: afclient.WorkareaDiffSummary{Total: 0},
		Entries: []afclient.WorkareaDiffEntry{},
	}}
	root := newWorkareaRootForTest(client)
	out, err := runRootCmd(t, root, "workarea", "diff", "a", "b", "--json")
	if err != nil {
		t.Fatalf("diff --json: %v", err)
	}
	var got afclient.WorkareaDiffResult
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("decode: %v\n%s", err, out)
	}
	if got.Summary.Total != 0 {
		t.Errorf("decoded total mismatch")
	}
}

func TestWorkareaCmd_Diff_NotFoundFriendlyMessage(t *testing.T) {
	client := &fakeWorkareaClient{diffErr: fmt.Errorf("x: %w", afclient.ErrNotFound)}
	root := newWorkareaRootForTest(client)
	_, err := runRootCmd(t, root, "workarea", "diff", "a", "b")
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "one or both archives not found") {
		t.Errorf("err = %v", err)
	}
}

func TestWorkareaCmd_Diff_RequiresExactlyTwoArgs(t *testing.T) {
	client := &fakeWorkareaClient{}
	root := newWorkareaRootForTest(client)
	if _, err := runRootCmd(t, root, "workarea", "diff"); err == nil {
		t.Error("expected error with no args")
	}
	if _, err := runRootCmd(t, root, "workarea", "diff", "a"); err == nil {
		t.Error("expected error with one arg")
	}
}

// ── integration: RegisterCommands wires the workarea subtree ──────────────

func TestWorkareaCmd_RegisteredViaRegisterCommands(t *testing.T) {
	root := &cobra.Command{Use: "test-root"}
	RegisterCommands(root, Config{
		ClientFactory: func() afclient.DataSource { return afclient.NewMockClient() },
	})
	for _, c := range root.Commands() {
		if c.Use == "workarea" {
			return
		}
	}
	t.Errorf("RegisterCommands did not register `workarea`; got: %v", commandNames(root))
}

// ── env override: RENSEI_DAEMON_URL ───────────────────────────────────────

func TestResolveWorkareaDaemonConfig(t *testing.T) {
	t.Setenv(workareaEnvDaemonURL, "")
	cfg := resolveWorkareaDaemonConfig()
	if cfg.Host != "127.0.0.1" || cfg.Port != 7734 {
		t.Errorf("default cfg = %+v, want 127.0.0.1:7734", cfg)
	}
	t.Setenv(workareaEnvDaemonURL, "http://10.0.0.5:9999")
	cfg = resolveWorkareaDaemonConfig()
	if cfg.Host != "10.0.0.5" || cfg.Port != 9999 {
		t.Errorf("override cfg = %+v", cfg)
	}
	t.Setenv(workareaEnvDaemonURL, "junk")
	cfg = resolveWorkareaDaemonConfig()
	if cfg.Host != "127.0.0.1" {
		t.Errorf("malformed override should fall back: %+v", cfg)
	}
}

// TestDefaultWorkareaClientFactory pins that the production factory
// returns a non-nil client.
func TestDefaultWorkareaClientFactory(t *testing.T) {
	c := defaultWorkareaClientFactory(afclient.DefaultDaemonConfig())
	if c == nil {
		t.Fatal("default factory returned nil")
	}
}
