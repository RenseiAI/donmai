package dashboard

import (
	"testing"

	"github.com/RenseiAI/agentfactory-tui/afclient"
)

// TestFormatProviderDisplay tests the provider display formatter.
func TestFormatProviderDisplay(t *testing.T) {
	tests := []struct {
		name     string
		input    *string
		expected string
	}{
		{
			name:     "nil provider",
			input:    nil,
			expected: "(default)",
		},
		{
			name:     "empty string provider",
			input:    pointerString(""),
			expected: "(default)",
		},
		{
			name:     "valid provider",
			input:    pointerString("e2b"),
			expected: "e2b",
		},
		{
			name:     "provider with ID",
			input:    pointerString("docker-prod-1"),
			expected: "docker-prod-1",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := formatProviderDisplay(tt.input)
			if got != tt.expected {
				t.Errorf("formatProviderDisplay(%v) = %q, want %q", tt.input, got, tt.expected)
			}
		})
	}
}

// TestFilterSessionsByProvider tests the provider-aware session filtering.
func TestFilterSessionsByProvider(t *testing.T) {
	tests := []struct {
		name        string
		sessions    []afclient.SessionResponse
		filterText  string
		expectedLen int
		expectedIDs []string
	}{
		{
			name:        "empty filter returns all",
			sessions:    testSessions(),
			filterText:  "",
			expectedLen: 3,
			expectedIDs: []string{"s1", "s2", "s3"},
		},
		{
			name:        "filter by provider ID",
			sessions:    testSessions(),
			filterText:  "e2b",
			expectedLen: 2,
			expectedIDs: []string{"s1", "s2"},
		},
		{
			name:        "filter by identifier",
			sessions:    testSessions(),
			filterText:  "test-1",
			expectedLen: 1,
			expectedIDs: []string{"s1"},
		},
		{
			name:        "filter by status",
			sessions:    testSessions(),
			filterText:  "queued",
			expectedLen: 1,
			expectedIDs: []string{"s1"},
		},
		{
			name:        "case-insensitive provider filter",
			sessions:    testSessions(),
			filterText:  "E2B",
			expectedLen: 2,
			expectedIDs: []string{"s1", "s2"},
		},
		{
			name:        "no matches returns empty",
			sessions:    testSessions(),
			filterText:  "nonexistent",
			expectedLen: 0,
			expectedIDs: []string{},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := filterSessions(tt.sessions, tt.filterText)
			if len(got) != tt.expectedLen {
				t.Errorf("filterSessions() returned %d results, want %d", len(got), tt.expectedLen)
			}
			for i, session := range got {
				if i < len(tt.expectedIDs) && session.ID != tt.expectedIDs[i] {
					t.Errorf("filterSessions() result[%d].ID = %q, want %q", i, session.ID, tt.expectedIDs[i])
				}
			}
		})
	}
}

// Helper function to create test data
func testSessions() []afclient.SessionResponse {
	return []afclient.SessionResponse{
		{
			ID:         "s1",
			Identifier: "test-1",
			Status:     afclient.StatusQueued,
			WorkType:   "agent",
			Duration:   60,
			Provider:   pointerString("e2b"),
		},
		{
			ID:         "s2",
			Identifier: "test-2",
			Status:     afclient.StatusWorking,
			WorkType:   "task",
			Duration:   120,
			Provider:   pointerString("e2b"),
		},
		{
			ID:         "s3",
			Identifier: "test-3",
			Status:     afclient.StatusCompleted,
			WorkType:   "agent",
			Duration:   300,
			Provider:   nil,
		},
	}
}

// Helper function to create a string pointer
func pointerString(s string) *string {
	return &s
}
