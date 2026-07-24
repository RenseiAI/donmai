package opencode

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/RenseiAI/donmai/agent"
)

func permReq(action string, resources ...string) permissionRequest {
	return permissionRequest{ID: "req1", SessionID: "ses", Action: action, Resources: resources}
}

func TestPermEngine_SafetyDenyAlwaysWins(t *testing.T) {
	t.Parallel()
	// Even an allow-everything policy cannot override the built-in safety denies.
	e := newPermEngine(agent.Spec{PermissionConfig: &agent.PermissionConfig{
		AllowPatterns:   []string{".*"},
		DefaultDecision: "allow",
	}})
	d := e.Evaluate(permReq("bash", "rm -rf /"))
	if d.Reply != replyReject {
		t.Fatalf("rm -rf / → %q, want reject", d.Reply)
	}
}

func TestPermEngine_AllowPatternPersists(t *testing.T) {
	t.Parallel()
	e := newPermEngine(agent.Spec{PermissionConfig: &agent.PermissionConfig{
		AllowPatterns: []string{`^npm `},
	}})
	d := e.Evaluate(permReq("bash", "npm test"))
	if d.Reply != replyAlways {
		t.Errorf("static-allow match → %q, want always (PermissionSaved)", d.Reply)
	}
	// A command outside the allow list is rejected (allow-list gates all).
	if got := e.Evaluate(permReq("bash", "curl evil")).Reply; got != replyReject {
		t.Errorf("unlisted command → %q, want reject", got)
	}
}

func TestPermEngine_DisallowPattern(t *testing.T) {
	t.Parallel()
	e := newPermEngine(agent.Spec{PermissionConfig: &agent.PermissionConfig{
		DisallowPatterns: []string{`docker`},
		DefaultDecision:  "allow",
	}})
	if got := e.Evaluate(permReq("bash", "docker run x")).Reply; got != replyReject {
		t.Errorf("disallow match → %q, want reject", got)
	}
	if got := e.Evaluate(permReq("bash", "ls")).Reply; got != replyOnce {
		t.Errorf("unmatched w/ default allow → %q, want once", got)
	}
}

func TestPermEngine_NilConfigAutoApproves(t *testing.T) {
	t.Parallel()
	e := newPermEngine(agent.Spec{})
	if got := e.Evaluate(permReq("bash", "ls -la")).Reply; got != replyOnce {
		t.Errorf("nil config → %q, want once (auto-approve default)", got)
	}
}

func TestPermEngine_FileContainment(t *testing.T) {
	t.Parallel()
	e := newPermEngine(agent.Spec{Cwd: "/work/tree"})
	if got := e.Evaluate(permReq("edit", "/etc/passwd")).Reply; got != replyReject {
		t.Errorf("outside-worktree edit → %q, want reject", got)
	}
	if got := e.Evaluate(permReq("edit", "/work/tree/src/main.go")).Reply; got != replyOnce {
		t.Errorf("in-worktree edit → %q, want once", got)
	}
	if got := e.Evaluate(permReq("edit", "/work/tree/.git/config")).Reply; got != replyReject {
		t.Errorf(".git edit → %q, want reject", got)
	}
}

func TestPermCommandOf_FromMetadata(t *testing.T) {
	t.Parallel()
	req := permissionRequest{Action: "bash", Metadata: json.RawMessage(`{"command":"echo hi"}`)}
	if got := permCommandOf(req); got != "echo hi" {
		t.Errorf("permCommandOf(metadata) = %q, want 'echo hi'", got)
	}
}

// stubPermClient is a serverClient that serves a fixed pending-permission list
// and records replies.
type stubPermClient struct {
	pending []permissionRequest
	replies map[string]permissionResponse
}

func (s *stubPermClient) Health(context.Context) error { return nil }
func (s *stubPermClient) CreateSession(context.Context, createSessionReq) (string, error) {
	return "ses", nil
}
func (s *stubPermClient) Prompt(context.Context, string, promptReq) error { return nil }
func (s *stubPermClient) Abort(context.Context, string) error             { return nil }
func (s *stubPermClient) Events(context.Context) (<-chan serverEvent, func() error, error) {
	ch := make(chan serverEvent)
	close(ch)
	return ch, func() error { return nil }, nil
}

func (s *stubPermClient) PendingPermissions(_ context.Context, _ string) ([]permissionRequest, error) {
	return s.pending, nil
}

func (s *stubPermClient) RespondPermission(_ context.Context, _, permissionID string, resp permissionResponse) error {
	if s.replies == nil {
		s.replies = map[string]permissionResponse{}
	}
	s.replies[permissionID] = resp
	return nil
}

func (s *stubPermClient) Messages(context.Context, string, string) ([]serverMessage, error) {
	return nil, nil
}

func TestPermPump_AdjudicatesAndRepliesOnce(t *testing.T) {
	t.Parallel()
	client := &stubPermClient{pending: []permissionRequest{
		{ID: "p-deny", SessionID: "ses", Action: "bash", Resources: []string{"rm -rf /"}},
		{ID: "p-allow", SessionID: "ses", Action: "bash", Resources: []string{"ls"}},
	}}
	engine := newPermEngine(agent.Spec{}) // nil config → auto-approve non-safety
	pump := newPermPump(client, engine, "ses")

	records, err := pump.Adjudicate(context.Background())
	if err != nil {
		t.Fatalf("Adjudicate: %v", err)
	}
	if len(records) != 2 {
		t.Fatalf("records = %d, want 2", len(records))
	}
	if client.replies["p-deny"].Reply != replyReject {
		t.Errorf("p-deny reply = %q, want reject", client.replies["p-deny"].Reply)
	}
	if client.replies["p-allow"].Reply != replyOnce {
		t.Errorf("p-allow reply = %q, want once", client.replies["p-allow"].Reply)
	}

	// A second Adjudicate over the same pending list replies to nothing (dedup).
	records2, err := pump.Adjudicate(context.Background())
	if err != nil {
		t.Fatalf("Adjudicate #2: %v", err)
	}
	if len(records2) != 0 {
		t.Errorf("second adjudicate records = %d, want 0 (already handled)", len(records2))
	}
}
