package runner

import (
	"bytes"
	"testing"

	"github.com/RenseiAI/donmai/agent"
)

// TestTranslateSpec_PermissionProfile pins the QueuedWork.PermissionProfile →
// agent.Spec.SandboxLevel mapping translateSpec's resolveSandboxLevel
// implements: absent/empty/workspace-write preserve the pre-field hardcoded
// default byte-for-byte, autonomous requests the full-access posture, and an
// unrecognized value is fail-safe (warns, falls back to workspace-write)
// rather than fail-closed — see runner/types.go's QueuedWork.PermissionProfile
// doc comment for the full contract.
func TestTranslateSpec_PermissionProfile(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name        string
		profile     PermissionProfile
		wantLevel   agent.SandboxLevel
		wantWarning bool
	}{
		{name: "absent-preserves-current-behavior", profile: "", wantLevel: agent.SandboxWorkspaceWrite},
		{name: "explicit-workspace-write", profile: PermissionProfileWorkspaceWrite, wantLevel: agent.SandboxWorkspaceWrite},
		{name: "autonomous-requests-full-access", profile: PermissionProfileAutonomous, wantLevel: agent.SandboxFullAccess},
		{name: "unknown-value-warns-and-falls-back", profile: PermissionProfile("bogus"), wantLevel: agent.SandboxWorkspaceWrite, wantWarning: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			qw := QueuedWork{PermissionProfile: tt.profile}
			qw.SessionID = "sess_perm"

			var buf bytes.Buffer
			in := SpecInputs{Cwd: "/tmp/wt", Prompt: "do", Logger: captureLogger(&buf)}
			spec := translateSpec(qw, agent.Capabilities{}, in)

			if spec.SandboxLevel != tt.wantLevel {
				t.Fatalf("SandboxLevel = %q, want %q", spec.SandboxLevel, tt.wantLevel)
			}
			if !spec.SandboxEnabled {
				t.Fatalf("SandboxEnabled = false, want true (unchanged by PermissionProfile)")
			}

			records := capturedRecords(t, &buf)
			if tt.wantWarning {
				if len(records) != 1 {
					t.Fatalf("want exactly one warning record, got %d: %s", len(records), buf.String())
				}
				if got := records[0]["level"]; got != "WARN" {
					t.Fatalf("level = %v, want WARN", got)
				}
				if got := records[0]["permissionProfile"]; got != string(tt.profile) {
					t.Fatalf("permissionProfile = %v, want %v", got, tt.profile)
				}
				if got := records[0]["sessionId"]; got != "sess_perm" {
					t.Fatalf("sessionId = %v, want sess_perm", got)
				}
			} else if len(records) != 0 {
				t.Fatalf("want silence, got %d record(s): %s", len(records), buf.String())
			}
		})
	}
}
