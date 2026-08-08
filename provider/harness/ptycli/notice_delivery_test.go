package ptycli

import (
	"context"
	"errors"
	"testing"

	"github.com/RenseiAI/donmai/agent"
)

// ptyNoticeManifest is the manifest of a harness that DECLARES pty-notice —
// i.e. one for which writing a self-submitting line into the terminal is the
// correct primitive because no agent sits behind it. Every pre-existing spawn
// test in this package passes it so their behaviour is unchanged; the
// declaration only matters for TryWriteNotice.
func ptyNoticeManifest() agent.HarnessManifest {
	return agent.HarnessManifest{
		Name: agent.HarnessShell,
		Caps: agent.HarnessCaps{
			SupportsInteractivePTY: true,
			NoticeDelivery:         agent.NoticeDeliveryPTYNotice,
		},
	}
}

// TestSpawn_NoticePermissionComesFromTheDeclaredChannel is the driver-level
// half of the notice-delivery gate.
//
// The defect it exists for: the PTY notice was written into ANY interactive
// session that would take the bytes. For a shell that is a command; for claude
// or codex it is a keystroke into a live agent UI, where the submit byte
// selects whatever option that UI is currently drawing — and the terminal
// cannot see the difference, which is exactly why the permission has to be
// declared rather than detected.
//
// So the driver reads the harness's own manifest and the session refuses
// structurally. A refusal here wraps agent.ErrUnsupported (not the transient
// "(false, nil), try later"), because nothing about it can change for the life
// of the session — a caller that retries it spins, and a caller that treats it
// as delivered lies.
func TestSpawn_NoticePermissionComesFromTheDeclaredChannel(t *testing.T) {
	requireShell(t)

	tests := []struct {
		name     string
		declared agent.NoticeDelivery
		wantWrit bool
	}{
		{name: "pty-notice is the one channel this surface implements", declared: agent.NoticeDeliveryPTYNotice, wantWrit: true},
		{name: "claude-code declares hook", declared: agent.NoticeDeliveryHook},
		{name: "codex declares mcp-rpc", declared: agent.NoticeDeliveryMCPRPC},
		{name: "opencode declares http-session", declared: agent.NoticeDeliveryHTTPSession},
		{name: "pi declares rpc-steer", declared: agent.NoticeDeliveryRPCSteer},
		{name: "amp declares resume-inject", declared: agent.NoticeDeliveryResumeInject},
		{name: "an explicit none", declared: agent.NoticeDeliveryNone},
		{name: "an undeclared manifest refuses too", declared: ""},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			manifest := agent.HarnessManifest{
				Caps: agent.HarnessCaps{
					SupportsInteractivePTY: true,
					NoticeDelivery:         tc.declared,
				},
			}
			h, err := Spawn(context.Background(), "sh", []string{"-c", "sleep 30"}, agent.Spec{
				Cwd: t.TempDir(), Interactive: &agent.InteractiveSpec{},
			}, manifest)
			if err != nil {
				t.Fatalf("Spawn: %v", err)
			}
			t.Cleanup(func() { _ = h.Stop(context.Background()) })

			notifier, ok := h.InteractiveSession().(agent.InteractiveNotifier)
			if !ok {
				t.Fatal("the PTY session does not implement agent.InteractiveNotifier")
			}

			written, err := notifier.TryWriteNotice([]byte("peer message\r"))
			if written != tc.wantWrit {
				t.Fatalf("TryWriteNotice written=%v, want %v (declared %q)", written, tc.wantWrit, tc.declared)
			}
			if tc.wantWrit {
				if err != nil {
					t.Fatalf("TryWriteNotice on a pty-notice harness: %v", err)
				}
				return
			}
			if !errors.Is(err, agent.ErrUnsupported) {
				t.Fatalf("refusal error = %v; want one wrapping agent.ErrUnsupported so the caller can tell "+
					"a structural refusal from a retryable one", err)
			}
		})
	}
}
