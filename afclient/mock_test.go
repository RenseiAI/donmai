package afclient

import (
	"errors"
	"testing"
)

func TestMockClientGetStats(t *testing.T) {
	m := NewMockClient()
	stats, err := m.GetStats()
	if err != nil {
		t.Fatalf("GetStats() error: %v", err)
	}
	if stats.WorkersOnline != 3 {
		t.Errorf("WorkersOnline = %d, want 3", stats.WorkersOnline)
	}
	if stats.AgentsWorking != 5 {
		t.Errorf("AgentsWorking = %d, want 5", stats.AgentsWorking)
	}
	if stats.QueueDepth != 2 {
		t.Errorf("QueueDepth = %d, want 2", stats.QueueDepth)
	}
	if stats.CompletedToday != 2 {
		t.Errorf("CompletedToday = %d, want 2", stats.CompletedToday)
	}
}

func TestMockClientGetSessions(t *testing.T) {
	m := NewMockClient()
	resp, err := m.GetSessions()
	if err != nil {
		t.Fatalf("GetSessions() error: %v", err)
	}
	if resp.Count != 12 {
		t.Errorf("Count = %d, want 12", resp.Count)
	}
	if len(resp.Sessions) != 12 {
		t.Errorf("len(Sessions) = %d, want 12", len(resp.Sessions))
	}

	// Verify status distribution
	counts := make(map[SessionStatus]int)
	for _, s := range resp.Sessions {
		counts[s.Status]++
	}
	if counts[StatusWorking] != 5 {
		t.Errorf("working sessions = %d, want 5", counts[StatusWorking])
	}
	if counts[StatusQueued] != 2 {
		t.Errorf("queued sessions = %d, want 2", counts[StatusQueued])
	}
}

func TestMockClientGetSessionsFiltered(t *testing.T) {
	m := NewMockClient()
	// Mock ignores project scope; both empty and non-empty should return the full list.
	cases := []string{"", "my-project", "team-a"}
	for _, project := range cases {
		resp, err := m.GetSessionsFiltered(project)
		if err != nil {
			t.Fatalf("GetSessionsFiltered(%q) error: %v", project, err)
		}
		if resp.Count != 12 {
			t.Errorf("project %q: Count = %d, want 12", project, resp.Count)
		}
	}
}

func TestMockClientGetSessionDetail(t *testing.T) {
	m := NewMockClient()
	detail, err := m.GetSessionDetail("mock-001")
	if err != nil {
		t.Fatalf("GetSessionDetail() error: %v", err)
	}
	if detail.Session.Identifier != "SUP-1180" {
		t.Errorf("Identifier = %q, want %q", detail.Session.Identifier, "SUP-1180")
	}
	if detail.Session.Status != StatusWorking {
		t.Errorf("Status = %q, want %q", detail.Session.Status, StatusWorking)
	}
	if detail.Session.Timeline.Created == "" {
		t.Error("Timeline.Created is empty")
	}
}

func TestMockClientGetSessionDetailNotFound(t *testing.T) {
	m := NewMockClient()
	_, err := m.GetSessionDetail("nonexistent")
	if err == nil {
		t.Error("expected error for nonexistent session")
	}
}

func TestMockClientStopSession(t *testing.T) {
	m := NewMockClient()
	resp, err := m.StopSession("mock-001")
	if err != nil {
		t.Fatalf("StopSession: %v", err)
	}
	if !resp.Stopped {
		t.Error("Stopped should be true")
	}
	if resp.SessionID != "mock-001" {
		t.Errorf("SessionID = %q, want %q", resp.SessionID, "mock-001")
	}
	if resp.PreviousStatus != StatusWorking {
		t.Errorf("PreviousStatus = %q, want %q", resp.PreviousStatus, StatusWorking)
	}
	if resp.NewStatus != StatusStopped {
		t.Errorf("NewStatus = %q, want %q", resp.NewStatus, StatusStopped)
	}
}

func TestMockClientStopSessionNotFound(t *testing.T) {
	m := NewMockClient()
	_, err := m.StopSession("nope")
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("err = %v, want ErrNotFound", err)
	}
}

func TestMockClientChatSession(t *testing.T) {
	m := NewMockClient()
	resp, err := m.ChatSession("mock-001", ChatSessionRequest{Prompt: "hello"})
	if err != nil {
		t.Fatalf("ChatSession: %v", err)
	}
	if !resp.Delivered {
		t.Error("Delivered should be true")
	}
	if resp.PromptID == "" {
		t.Error("PromptID should be set")
	}
	if resp.SessionID != "mock-001" {
		t.Errorf("SessionID = %q, want %q", resp.SessionID, "mock-001")
	}
	if resp.SessionStatus == "" {
		t.Error("SessionStatus should be set")
	}
}

func TestMockClientChatSessionNotFound(t *testing.T) {
	m := NewMockClient()
	_, err := m.ChatSession("nope", ChatSessionRequest{Prompt: "hello"})
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("err = %v, want ErrNotFound", err)
	}
}

func TestMockClientReconnectSession(t *testing.T) {
	m := NewMockClient()
	resp, err := m.ReconnectSession("mock-001", ReconnectSessionRequest{})
	if err != nil {
		t.Fatalf("ReconnectSession: %v", err)
	}
	if !resp.Reconnected {
		t.Error("Reconnected should be true")
	}
	if resp.SessionID != "mock-001" {
		t.Errorf("SessionID = %q, want %q", resp.SessionID, "mock-001")
	}
	if resp.MissedEvents != 0 {
		t.Errorf("MissedEvents = %d, want 0", resp.MissedEvents)
	}
}

func TestMockClientReconnectSessionNotFound(t *testing.T) {
	m := NewMockClient()
	_, err := m.ReconnectSession("nope", ReconnectSessionRequest{})
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("err = %v, want ErrNotFound", err)
	}
}

func TestMockClientMCPEndpoints(t *testing.T) {
	m := NewMockClient()
	if _, err := m.SubmitTask(SubmitTaskRequest{IssueID: "i-1"}); err != nil {
		t.Errorf("SubmitTask: %v", err)
	}
	if _, err := m.StopAgent(StopAgentRequest{TaskID: "t-1"}); err != nil {
		t.Errorf("StopAgent: %v", err)
	}
	if _, err := m.ForwardPrompt(ForwardPromptRequest{TaskID: "t-1"}); err != nil {
		t.Errorf("ForwardPrompt: %v", err)
	}
	if _, err := m.GetCostReport(); err != nil {
		t.Errorf("GetCostReport: %v", err)
	}
	if _, err := m.ListFleet(); err != nil {
		t.Errorf("ListFleet: %v", err)
	}
}

func TestMockClientGetActivities(t *testing.T) {
	m := NewMockClient()
	resp, err := m.GetActivities("mock-001", nil)
	if err != nil {
		t.Fatalf("GetActivities: %v", err)
	}
	if len(resp.Activities) == 0 {
		t.Error("expected activities")
	}

	if resp.Cursor == nil {
		t.Fatal("expected cursor")
	}
	// Passing a cursor should filter to later events (or none).
	second, err := m.GetActivities("mock-001", resp.Cursor)
	if err != nil {
		t.Fatalf("GetActivities with cursor: %v", err)
	}
	if len(second.Activities) != 0 {
		t.Errorf("expected no activities after final cursor, got %d", len(second.Activities))
	}

	if _, err := m.GetActivities("unknown", nil); err == nil {
		t.Error("expected error for unknown session")
	}
}
