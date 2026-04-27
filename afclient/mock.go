package afclient

import (
	"fmt"
	"time"
)

func ptr[T any](v T) *T { return &v }

// Compile-time check that MockClient satisfies DataSource.
var _ DataSource = (*MockClient)(nil)

// MockClient returns realistic mock data matching the public API shapes.
type MockClient struct {
	sessions []SessionResponse
}

// NewMockClient creates a mock data source with 12 sample sessions.
func NewMockClient() *MockClient {
	now := time.Now()
	return &MockClient{
		sessions: []SessionResponse{
			{
				ID: "mock-001", Identifier: "SUP-1180", Status: StatusWorking,
				WorkType: "development", StartedAt: now.Add(-47 * time.Minute).Format(time.RFC3339),
				Duration: 2820, CostUsd: ptr(3.42), Provider: ptr("anthropic"),
			},
			{
				ID: "mock-002", Identifier: "SUP-1195", Status: StatusWorking,
				WorkType: "research", StartedAt: now.Add(-72 * time.Minute).Format(time.RFC3339),
				Duration: 4320, CostUsd: ptr(5.18), Provider: ptr("anthropic"),
			},
			{
				ID: "mock-003", Identifier: "SUP-1201", Status: StatusWorking,
				WorkType: "qa", StartedAt: now.Add(-22 * time.Minute).Format(time.RFC3339),
				Duration: 1320, CostUsd: ptr(1.87), Provider: ptr("openai"),
			},
			{
				ID: "mock-004", Identifier: "SUP-1199", Status: StatusWorking,
				WorkType: "feature", StartedAt: now.Add(-35 * time.Minute).Format(time.RFC3339),
				Duration: 2100, CostUsd: ptr(2.91), Provider: ptr("anthropic"),
			},
			{
				ID: "mock-005", Identifier: "SUP-1188", Status: StatusWorking,
				WorkType: "bugfix", StartedAt: now.Add(-63 * time.Minute).Format(time.RFC3339),
				Duration: 3780, CostUsd: ptr(4.20), Provider: ptr("openai"),
			},
			{
				ID: "mock-006", Identifier: "SUP-1205", Status: StatusQueued,
				WorkType: "acceptance", StartedAt: now.Add(-5 * time.Minute).Format(time.RFC3339),
				Duration: 300, CostUsd: nil, Provider: nil,
			},
			{
				ID: "mock-007", Identifier: "SUP-1208", Status: StatusQueued,
				WorkType: "coordination", StartedAt: now.Add(-2 * time.Minute).Format(time.RFC3339),
				Duration: 120, CostUsd: nil, Provider: nil,
			},
			{
				ID: "mock-008", Identifier: "SUP-1150", Status: StatusCompleted,
				WorkType: "development", StartedAt: now.Add(-4 * time.Hour).Format(time.RFC3339),
				Duration: 13500, CostUsd: ptr(8.50), Provider: ptr("anthropic"),
			},
			{
				ID: "mock-009", Identifier: "SUP-1162", Status: StatusCompleted,
				WorkType: "refactor", StartedAt: now.Add(-3 * time.Hour).Format(time.RFC3339),
				Duration: 7800, CostUsd: ptr(6.33), Provider: ptr("anthropic"),
			},
			{
				ID: "mock-010", Identifier: "SUP-1175", Status: StatusFailed,
				WorkType: "qa", StartedAt: now.Add(-2 * time.Hour).Format(time.RFC3339),
				Duration: 2700, CostUsd: ptr(2.10), Provider: ptr("openai"),
			},
			{
				ID: "mock-011", Identifier: "SUP-1190", Status: StatusStopped,
				WorkType: "docs", StartedAt: now.Add(-90 * time.Minute).Format(time.RFC3339),
				Duration: 900, CostUsd: ptr(0.85), Provider: ptr("anthropic"),
			},
			{
				ID: "mock-012", Identifier: "SUP-1202", Status: StatusParked,
				WorkType: "refinement", StartedAt: now.Add(-8 * time.Minute).Format(time.RFC3339),
				Duration: 480, CostUsd: nil, Provider: nil,
			},
		},
	}
}

// GetStats returns mock fleet statistics.
func (m *MockClient) GetStats() (*StatsResponse, error) {
	working := 0
	queued := 0
	completed := 0
	var totalCost float64
	for _, s := range m.sessions {
		switch s.Status {
		case StatusWorking:
			working++
		case StatusQueued:
			queued++
		case StatusCompleted:
			completed++
		}
		if s.CostUsd != nil {
			totalCost += *s.CostUsd
		}
	}

	return &StatsResponse{
		WorkersOnline:     3,
		AgentsWorking:     working,
		QueueDepth:        queued,
		CompletedToday:    completed,
		AvailableCapacity: 8 - working,
		TotalCostToday:    float64(int(totalCost*100)) / 100,
		TotalCostAllTime:  142.87,
		SessionCountToday: len(m.sessions),
		Timestamp:         time.Now().Format(time.RFC3339),
	}, nil
}

// GetSessions returns the mock session list (fleet-wide).
func (m *MockClient) GetSessions() (*SessionsListResponse, error) {
	return m.GetSessionsFiltered("")
}

// GetSessionsFiltered returns the mock session list. The project argument is
// accepted for interface compatibility but has no effect on mock data — all
// sessions are returned regardless of scope.
func (m *MockClient) GetSessionsFiltered(_ string) (*SessionsListResponse, error) {
	return &SessionsListResponse{
		Sessions:  m.sessions,
		Count:     len(m.sessions),
		Timestamp: time.Now().Format(time.RFC3339),
	}, nil
}

// GetSessionDetail returns mock detail for a single session.
func (m *MockClient) GetSessionDetail(id string) (*SessionDetailResponse, error) {
	for _, s := range m.sessions {
		if s.ID == id {
			now := time.Now()
			startedAt := s.StartedAt
			timeline := SessionTimeline{
				Created: startedAt,
			}
			queuedTime := now.Add(-time.Duration(s.Duration+2) * time.Second).Format(time.RFC3339)
			timeline.Queued = &queuedTime
			startedTime := now.Add(-time.Duration(s.Duration) * time.Second).Format(time.RFC3339)
			timeline.Started = &startedTime

			if s.Status == StatusCompleted || s.Status == StatusFailed || s.Status == StatusStopped {
				completedTime := now.Add(-30 * time.Second).Format(time.RFC3339)
				timeline.Completed = &completedTime
			}

			return &SessionDetailResponse{
				Session: SessionDetail{
					ID:           s.ID,
					Identifier:   s.Identifier,
					Status:       s.Status,
					WorkType:     s.WorkType,
					StartedAt:    s.StartedAt,
					Duration:     s.Duration,
					Timeline:     timeline,
					Provider:     s.Provider,
					CostUsd:      s.CostUsd,
					Branch:       ptr(s.Identifier + "-DEV"),
					IssueTitle:   ptr("Add user authentication to login flow"),
					InputTokens:  ptr(45200),
					OutputTokens: ptr(12800),
				},
				Timestamp: time.Now().Format(time.RFC3339),
			}, nil
		}
	}
	return nil, fmt.Errorf("session not found: %s", id)
}

// StopSession returns a mock stop response for a known session, or ErrNotFound.
func (m *MockClient) StopSession(id string) (*StopSessionResponse, error) {
	for i, s := range m.sessions {
		if s.ID == id {
			prev := s.Status
			m.sessions[i].Status = StatusStopped
			return &StopSessionResponse{
				Stopped:        true,
				SessionID:      id,
				PreviousStatus: prev,
				NewStatus:      StatusStopped,
			}, nil
		}
	}
	return nil, fmt.Errorf("session %s: %w", id, ErrNotFound)
}

// ChatSession returns a mock chat delivery response for a known session.
func (m *MockClient) ChatSession(id string, _ ChatSessionRequest) (*ChatSessionResponse, error) {
	for _, s := range m.sessions {
		if s.ID == id {
			return &ChatSessionResponse{
				Delivered:     true,
				PromptID:      "mock-prompt-" + id,
				SessionID:     id,
				SessionStatus: s.Status,
			}, nil
		}
	}
	return nil, fmt.Errorf("session %s: %w", id, ErrNotFound)
}

// ReconnectSession returns a mock reconnect response for a known session.
func (m *MockClient) ReconnectSession(id string, _ ReconnectSessionRequest) (*ReconnectSessionResponse, error) {
	for _, s := range m.sessions {
		if s.ID == id {
			return &ReconnectSessionResponse{
				Reconnected:   true,
				SessionID:     id,
				SessionStatus: s.Status,
				MissedEvents:  0,
			}, nil
		}
	}
	return nil, fmt.Errorf("session %s: %w", id, ErrNotFound)
}

// GetActivities returns mock activity events for a session.
func (m *MockClient) GetActivities(sessionID string, afterCursor *string) (*ActivityListResponse, error) {
	// Find the session to get status
	var status SessionStatus
	for _, s := range m.sessions {
		if s.ID == sessionID {
			status = s.Status
			break
		}
	}
	if status == "" {
		return nil, fmt.Errorf("session not found: %s", sessionID)
	}

	now := time.Now()
	allActivities := []ActivityEvent{
		{ID: "1", Type: ActivityThought, Content: "Analyzing issue requirements and codebase structure", Timestamp: now.Add(-10 * time.Minute).Format(time.RFC3339)},
		{ID: "2", Type: ActivityAction, Content: "Reading src/auth/login.ts", ToolName: ptr("Read"), Timestamp: now.Add(-9 * time.Minute).Format(time.RFC3339)},
		{ID: "3", Type: ActivityThought, Content: "Found existing auth patterns, need to extend middleware", Timestamp: now.Add(-8 * time.Minute).Format(time.RFC3339)},
		{ID: "4", Type: ActivityAction, Content: "Reading src/auth/middleware.ts", ToolName: ptr("Read"), Timestamp: now.Add(-7 * time.Minute).Format(time.RFC3339)},
		{ID: "5", Type: ActivityProgress, Content: "Research phase complete", Timestamp: now.Add(-6 * time.Minute).Format(time.RFC3339)},
		{ID: "6", Type: ActivityAction, Content: "Writing src/auth/jwt-validator.ts", ToolName: ptr("Write"), Timestamp: now.Add(-5 * time.Minute).Format(time.RFC3339)},
		{ID: "7", Type: ActivityThought, Content: "JWT validation middleware created, adding route guards", Timestamp: now.Add(-4 * time.Minute).Format(time.RFC3339)},
		{ID: "8", Type: ActivityAction, Content: "Editing src/routes/protected.ts", ToolName: ptr("Edit"), Timestamp: now.Add(-3 * time.Minute).Format(time.RFC3339)},
		{ID: "9", Type: ActivityResponse, Content: "Added JWT validation to 3 protected routes", Timestamp: now.Add(-2 * time.Minute).Format(time.RFC3339)},
		{ID: "10", Type: ActivityAction, Content: "Running npm test -- --grep auth", ToolName: ptr("Bash"), Timestamp: now.Add(-90 * time.Second).Format(time.RFC3339)},
		{ID: "11", Type: ActivityProgress, Content: "Tests passing: 12/12", Timestamp: now.Add(-60 * time.Second).Format(time.RFC3339)},
		{ID: "12", Type: ActivityAction, Content: "Creating pull request", ToolName: ptr("Bash"), Timestamp: now.Add(-30 * time.Second).Format(time.RFC3339)},
		{ID: "13", Type: ActivityResponse, Content: "PR #47 created: Add JWT authentication middleware", Timestamp: now.Add(-15 * time.Second).Format(time.RFC3339)},
	}

	// Filter by cursor
	activities := allActivities
	if afterCursor != nil {
		cursorNum := 0
		_, _ = fmt.Sscanf(*afterCursor, "%d", &cursorNum)
		filtered := make([]ActivityEvent, 0)
		for _, a := range allActivities {
			aNum := 0
			_, _ = fmt.Sscanf(a.ID, "%d", &aNum)
			if aNum > cursorNum {
				filtered = append(filtered, a)
			}
		}
		activities = filtered
	}

	var cursor *string
	if len(activities) > 0 {
		cursor = &activities[len(activities)-1].ID
	}

	return &ActivityListResponse{
		Activities:    activities,
		Cursor:        cursor,
		SessionStatus: status,
	}, nil
}

// SubmitTask returns a mock task submission response.
func (m *MockClient) SubmitTask(req SubmitTaskRequest) (*SubmitTaskResponse, error) {
	return &SubmitTaskResponse{Submitted: true, TaskID: "mock-task", IssueID: req.IssueID, Status: "pending", Priority: 3, WorkType: "development"}, nil
}

// StopAgent returns a mock stop-agent response.
func (m *MockClient) StopAgent(req StopAgentRequest) (*StopAgentResponse, error) {
	return &StopAgentResponse{Stopped: true, TaskID: req.TaskID, PreviousStatus: "running", NewStatus: "stopped"}, nil
}

// ForwardPrompt returns a mock prompt-forwarding response.
func (m *MockClient) ForwardPrompt(req ForwardPromptRequest) (*ForwardPromptResponse, error) {
	return &ForwardPromptResponse{Forwarded: true, PromptID: "mock-prm-1", TaskID: req.TaskID, IssueID: req.TaskID, SessionStatus: "running"}, nil
}

// GetCostReport returns a mock cost report.
func (m *MockClient) GetCostReport() (*CostReportResponse, error) {
	return &CostReportResponse{TotalSessions: 5, SessionsWithCostData: 3, TotalCostUsd: 12.50, TotalInputTokens: 50000, TotalOutputTokens: 25000}, nil
}

// ListFleet returns a mock fleet listing.
func (m *MockClient) ListFleet() (*ListFleetResponse, error) {
	sessions, _ := m.GetSessions()
	return &ListFleetResponse{Total: len(sessions.Sessions), Returned: len(sessions.Sessions), Sessions: sessions.Sessions}, nil
}

// ── Architecture-aware mock methods (REN-1333) ────────────────────────────────

// GetStatsV2 returns mock fleet statistics extended with per-machine and
// per-provider breakdowns. Returns realistic-but-empty slices so callers can
// safely introspect length and field structure without nil-pointer panics.
func (m *MockClient) GetStatsV2() (*StatsResponseV2, error) {
	base, err := m.GetStats()
	if err != nil {
		return nil, err
	}
	now := time.Now()
	return &StatsResponseV2{
		StatsResponse: *base,
		Machines: []MachineStats{
			{
				ID:             "mac-studio-marks-office",
				Region:         "home-network",
				Status:         DaemonReady,
				Version:        "0.8.60",
				ActiveSessions: 3,
				Capacity: MachineCapacity{
					MaxConcurrentSessions: 8,
					MaxVCpuPerSession:     4,
					MaxMemoryMbPerSession: 8192,
					ReservedVCpu:          4,
					ReservedMemoryMb:      16384,
				},
				UptimeSeconds: 7860,
				LastSeenAt:    now.Add(-15 * time.Second).Format(time.RFC3339),
			},
			{
				ID:             "macbook-travel",
				Region:         "home-network",
				Status:         DaemonPaused,
				Version:        "0.8.60",
				ActiveSessions: 0,
				Capacity: MachineCapacity{
					MaxConcurrentSessions: 4,
					MaxVCpuPerSession:     2,
					MaxMemoryMbPerSession: 4096,
					ReservedVCpu:          2,
					ReservedMemoryMb:      8192,
				},
				UptimeSeconds: 3600,
				LastSeenAt:    now.Add(-2 * time.Minute).Format(time.RFC3339),
			},
		},
		Providers: []ProviderCost{
			{Provider: "anthropic", CostUsd: 24.31, Sessions: 7},
			{Provider: "openai", CostUsd: 8.27, Sessions: 3},
		},
	}, nil
}

// GetMachineStats returns mock per-machine capacity and status snapshots.
func (m *MockClient) GetMachineStats() ([]MachineStats, error) {
	v2, err := m.GetStatsV2()
	if err != nil {
		return nil, err
	}
	return v2.Machines, nil
}

// GetWorkareaPoolStats returns a mock workarea pool snapshot. The machineID
// argument is accepted for interface compatibility but has no effect on mock
// data — the same pool is returned regardless.
func (m *MockClient) GetWorkareaPoolStats(_ MachineID) (*WorkareaPoolStats, error) {
	now := time.Now()
	members := []WorkareaPoolMember{
		{
			ID:                 "pool-001",
			Repository:         "github.com/renseiai/agentfactory",
			Ref:                "main",
			ToolchainKey:       "node-20",
			Status:             PoolMemberReady,
			CleanStateChecksum: "sha256:a3f1b2c4d5e6f7a8b9c0d1e2f3a4b5c6d7e8f9a0b1c2d3e4f5a6b7c8d9e0f1a2",
			CreatedAt:          now.Add(-6 * time.Hour).Format(time.RFC3339),
			LastAcquiredAt:     now.Add(-45 * time.Minute).Format(time.RFC3339),
			DiskUsageMb:        1240,
		},
		{
			ID:                 "pool-002",
			Repository:         "github.com/renseiai/agentfactory",
			Ref:                "main",
			ToolchainKey:       "node-20",
			Status:             PoolMemberAcquired,
			CleanStateChecksum: "sha256:a3f1b2c4d5e6f7a8b9c0d1e2f3a4b5c6d7e8f9a0b1c2d3e4f5a6b7c8d9e0f1a2",
			AcquiredBy:         "mock-001",
			CreatedAt:          now.Add(-5 * time.Hour).Format(time.RFC3339),
			LastAcquiredAt:     now.Add(-47 * time.Minute).Format(time.RFC3339),
			DiskUsageMb:        1238,
		},
		{
			ID:           "pool-003",
			Repository:   "github.com/renseiai/platform",
			Ref:          "main",
			ToolchainKey: "node-20+java-17",
			Status:       PoolMemberWarming,
			CreatedAt:    now.Add(-3 * time.Minute).Format(time.RFC3339),
			DiskUsageMb:  320,
		},
		{
			ID:                 "pool-004",
			Repository:         "github.com/renseiai/platform",
			Ref:                "main",
			ToolchainKey:       "node-20+java-17",
			Status:             PoolMemberInvalid,
			CleanStateChecksum: "sha256:b4c5d6e7f8a9b0c1d2e3f4a5b6c7d8e9f0a1b2c3d4e5f6a7b8c9d0e1f2a3b4c5",
			CreatedAt:          now.Add(-8 * time.Hour).Format(time.RFC3339),
			LastAcquiredAt:     now.Add(-2 * time.Hour).Format(time.RFC3339),
			DiskUsageMb:        2100,
		},
	}

	totalDisk := int64(0)
	readyCount, acquiredCount, warmingCount, invalidCount := 0, 0, 0, 0
	for _, mem := range members {
		totalDisk += mem.DiskUsageMb
		switch mem.Status {
		case PoolMemberReady:
			readyCount++
		case PoolMemberAcquired:
			acquiredCount++
		case PoolMemberWarming:
			warmingCount++
		case PoolMemberInvalid:
			invalidCount++
		}
	}
	return &WorkareaPoolStats{
		Members:          members,
		TotalMembers:     len(members),
		ReadyMembers:     readyCount,
		AcquiredMembers:  acquiredCount,
		WarmingMembers:   warmingCount,
		InvalidMembers:   invalidCount,
		TotalDiskUsageMb: totalDisk,
		Timestamp:        now.Format(time.RFC3339),
	}, nil
}

// GetSandboxProviderStats returns mock runtime snapshots for registered SandboxProviders.
func (m *MockClient) GetSandboxProviderStats() ([]SandboxProviderStats, error) {
	now := time.Now()
	return []SandboxProviderStats{
		{
			ID:                  "local",
			DisplayName:         "Local Daemon (mac-studio-marks-office)",
			TransportModel:      TransportEither,
			BillingModel:        BillingFixed,
			ProvisionedActive:   3,
			ProvisionedPaused:   0,
			MaxConcurrent:       8,
			Regions:             []string{"home-network"},
			SupportsPauseResume: false,
			SupportsFsSnapshot:  false,
			IsA2ARemote:         false,
			Healthy:             true,
			CapturedAt:          now.Format(time.RFC3339),
		},
		{
			ID:                  "e2b",
			DisplayName:         "E2B Cloud Sandbox",
			TransportModel:      TransportDialIn,
			BillingModel:        BillingWallClock,
			ProvisionedActive:   2,
			ProvisionedPaused:   4,
			MaxConcurrent:       -1,
			Regions:             []string{"us-east-1", "eu-west-1"},
			SupportsPauseResume: true,
			SupportsFsSnapshot:  true,
			IsA2ARemote:         false,
			Healthy:             true,
			CapturedAt:          now.Format(time.RFC3339),
		},
	}, nil
}

// GetKitDetections returns mock kit detection results for a session.
func (m *MockClient) GetKitDetections(_ string) ([]KitDetection, error) {
	return []KitDetection{
		{
			KitID:      "ts/nextjs",
			KitVersion: "1.2.0",
			Applies:    true,
			Confidence: 0.97,
			Reason:     "Detected next.config.ts and package.json with next dependency",
			ToolchainDemand: map[string]string{
				"node": ">=20",
			},
			DetectPhase: "declarative",
		},
		{
			KitID:       "spring/java",
			KitVersion:  "1.0.0",
			Applies:     false,
			Confidence:  0.0,
			Reason:      "No pom.xml or build.gradle found",
			DetectPhase: "declarative",
		},
	}, nil
}

// GetKitContributions returns mock kit contribution summaries for a session.
func (m *MockClient) GetKitContributions(_ string) ([]KitContribution, error) {
	return []KitContribution{
		{
			KitID:      "ts/nextjs",
			KitVersion: "1.2.0",
			Commands: map[string]string{
				"build":    "pnpm build",
				"test":     "pnpm test",
				"validate": "pnpm typecheck",
			},
			PromptFragmentCount: 2,
			ToolPermissionCount: 3,
			MCPServerNames:      []string{"nextjs-context"},
			SkillRefs:           []string{"skills/nextjs-debugging/SKILL.md"},
			WorkareaCleanDirs:   []string{".next", ".turbo", "node_modules/.cache"},
			AppliedAt:           time.Now().Add(-50 * time.Minute).Format(time.RFC3339),
		},
	}, nil
}

// GetAuditChain returns mock Layer 6 audit chain entries for a session.
func (m *MockClient) GetAuditChain(sessionID string) ([]AuditChainEntry, error) {
	now := time.Now()
	fingerprint := "abc1234…d4f2"
	attest := &Attestation{
		KeyAlgorithm:   AttestationEd25519,
		KeyFingerprint: fingerprint,
		Signature:      "base64encodedmocksignature==",
		SignedAt:       now.Add(-50 * time.Minute).Format(time.RFC3339),
		Verified:       true,
	}
	return []AuditChainEntry{
		{
			Sequence:     1,
			EventKind:    "session-accepted",
			SessionID:    sessionID,
			ActorID:      "orchestrator",
			Payload:      map[string]any{"project": "renseiai/agentfactory", "workType": "development"},
			PreviousHash: "0000000000000000000000000000000000000000000000000000000000000000",
			EntryHash:    "a1b2c3d4e5f6a7b8c9d0e1f2a3b4c5d6e7f8a9b0c1d2e3f4a5b6c7d8e9f0a1b2",
			Attestation:  attest,
			OccurredAt:   now.Add(-50 * time.Minute).Format(time.RFC3339),
		},
		{
			Sequence:     2,
			EventKind:    "workarea-acquired",
			SessionID:    sessionID,
			ActorID:      "daemon:mac-studio-marks-office",
			Payload:      map[string]any{"workareaId": "pool-002", "acquirePath": "pool-warm", "durationMs": 4200},
			PreviousHash: "a1b2c3d4e5f6a7b8c9d0e1f2a3b4c5d6e7f8a9b0c1d2e3f4a5b6c7d8e9f0a1b2",
			EntryHash:    "b2c3d4e5f6a7b8c9d0e1f2a3b4c5d6e7f8a9b0c1d2e3f4a5b6c7d8e9f0a1b2c3",
			OccurredAt:   now.Add(-49 * time.Minute).Format(time.RFC3339),
		},
		{
			Sequence:     3,
			EventKind:    "session-completed",
			SessionID:    sessionID,
			ActorID:      "worker",
			Payload:      map[string]any{"result": "delivered", "wallClockS": 2820, "activeCpuS": 1440},
			PreviousHash: "b2c3d4e5f6a7b8c9d0e1f2a3b4c5d6e7f8a9b0c1d2e3f4a5b6c7d8e9f0a1b2c3",
			EntryHash:    "c3d4e5f6a7b8c9d0e1f2a3b4c5d6e7f8a9b0c1d2e3f4a5b6c7d8e9f0a1b2c3d4",
			Attestation:  attest,
			OccurredAt:   now.Add(-3 * time.Minute).Format(time.RFC3339),
		},
	}, nil
}
