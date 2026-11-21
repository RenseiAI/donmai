package codex

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/RenseiAI/donmai/agent"
)

// A completed turn whose final MCP config release fails in a contained way
// (Provider poisoned, owner removal clean) must still deliver its result,
// with the cleanup failure as a warning. An uncontained failure keeps the
// terminal mcp_cleanup_failed error in place of the result.
func TestHandle_TerminalResultSurvivesContainedMCPCleanupFailure(t *testing.T) {
	cleanupErr := codexMCPApplicationError("clear isolated MCP config: isolated config boundary rejected: isolated Codex home mode is 0750, want 0700")
	cases := []struct {
		name        string
		release     func() error
		params      string
		wantResult  bool
		wantSuccess bool
		wantWarning bool
		wantError   bool
	}{
		{
			name:        "clean release",
			release:     func() error { return nil },
			params:      `{"turn":{"id":"t","status":"completed"}}`,
			wantResult:  true,
			wantSuccess: true,
		},
		{
			name:        "contained failure after completed turn",
			release:     func() error { return &mcpCleanupError{err: cleanupErr, contained: true} },
			params:      `{"turn":{"id":"t","status":"completed"}}`,
			wantResult:  true,
			wantSuccess: true,
			wantWarning: true,
		},
		{
			name:        "contained failure after failed turn keeps the turn's own result",
			release:     func() error { return &mcpCleanupError{err: cleanupErr, contained: true} },
			params:      `{"turn":{"id":"t","status":"failed","error":{"message":"boom"}}}`,
			wantResult:  true,
			wantWarning: true,
		},
		{
			name:      "uncontained failure stays fatal",
			release:   func() error { return &mcpCleanupError{err: cleanupErr} },
			params:    `{"turn":{"id":"t","status":"completed"}}`,
			wantError: true,
		},
		{
			name:      "untyped failure stays fatal",
			release:   func() error { return cleanupErr },
			params:    `{"turn":{"id":"t","status":"completed"}}`,
			wantError: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := notificationHandle()
			h.mcpRelease = tc.release
			var result *agent.ResultEvent
			var warning *agent.SystemEvent
			var cleanup *agent.ErrorEvent
			warningBeforeResult := false
			for _, event := range dispatch(t, h, "turn/completed", tc.params) {
				switch value := event.(type) {
				case agent.ResultEvent:
					result = &value
				case agent.SystemEvent:
					if value.Subtype == "codex_warning" {
						warning = &value
						warningBeforeResult = result == nil
					}
				case agent.ErrorEvent:
					if value.Code == "mcp_cleanup_failed" {
						cleanup = &value
					}
				}
			}
			if (result != nil) != tc.wantResult {
				t.Fatalf("result = %+v, want present=%v", result, tc.wantResult)
			}
			if result != nil && result.Success != tc.wantSuccess {
				t.Fatalf("result success = %v, want %v", result.Success, tc.wantSuccess)
			}
			if (warning != nil) != tc.wantWarning {
				t.Fatalf("cleanup warning = %+v, want present=%v", warning, tc.wantWarning)
			}
			if warning != nil {
				if !warningBeforeResult {
					t.Fatal("cleanup warning must precede the terminal result so it is not dropped after Events closes")
				}
				if !strings.Contains(warning.Message, "home mode is 0750") {
					t.Fatalf("cleanup warning = %q, want the bounded cause", warning.Message)
				}
			}
			if (cleanup != nil) != tc.wantError {
				t.Fatalf("cleanup error = %+v, want present=%v", cleanup, tc.wantError)
			}
		})
	}
}

// The real release path: a private home widened after acquire (for example
// by a process that inherited CODEX_HOME) is refused by the boundary before
// any RPC. The release poisons the Provider, removes the owned home, and so
// reports a contained failure whose detail names the cause. A failed owner
// removal (parent identity changed) is not contained.
func TestProvider_FinalReleaseBoundaryRejectionContainment(t *testing.T) {
	cases := []struct {
		name          string
		tamper        func(t *testing.T, p *Provider)
		wantContained bool
		wantDetail    string
	}{
		{
			name: "widened home is contained",
			tamper: func(t *testing.T, p *Provider) {
				if err := os.Chmod(p.config.home, 0o750); err != nil { //nolint:gosec // widen the owned home to trip validate()
					t.Fatal(err)
				}
			},
			wantContained: true,
			wantDetail:    "isolated Codex home mode is 0750, want 0700",
		},
		{
			name: "failed owner removal is not contained",
			tamper: func(t *testing.T, p *Provider) {
				other, err := os.Stat(t.TempDir())
				if err != nil {
					t.Fatal(err)
				}
				p.config.parentInfo = other
			},
			wantDetail: "isolated Codex parent identity changed",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fs, stdinW, stdoutR := newFakeServer()
			go fs.run(t, "thread-release-boundary")
			p, err := New(Options{
				skipProcess: true, stdinOverride: stdinW, stdoutOverride: stdoutR,
				configTempDir: t.TempDir(),
			})
			if err != nil {
				fs.close()
				t.Fatalf("New: %v", err)
			}
			startTestProvider(t, p)
			client := p.client
			t.Cleanup(func() {
				client.Stop(errors.New("test complete"))
				fs.close()
			})
			release, err := p.acquireMCPConfig(t.Context(), map[string]any{}, t.TempDir())
			if err != nil {
				t.Fatalf("acquire: %v", err)
			}
			ownedHome := p.config.home
			tc.tamper(t, p)

			err = release()
			if err == nil {
				t.Fatal("release succeeded over a rejected boundary")
			}
			if got := mcpCleanupContained(err); got != tc.wantContained {
				t.Fatalf("contained = %v, want %v (err %v)", got, tc.wantContained, err)
			}
			if !strings.Contains(err.Error(), tc.wantDetail) {
				t.Fatalf("release error = %v, want detail %q", err, tc.wantDetail)
			}
			var adaptation *agent.ToolAdaptationError
			if !errors.As(err, &adaptation) || adaptation.Code != agent.ToolDenialApplicationFailed {
				t.Fatalf("release error = %v, want typed MCP application failure", err)
			}
			if tc.wantContained {
				if _, statErr := os.Stat(ownedHome); !os.IsNotExist(statErr) {
					t.Fatalf("contained release left the owned home: stat err=%v", statErr)
				}
			}
			if _, err := p.acquireMCPConfig(context.Background(), map[string]any{}, t.TempDir()); err == nil {
				t.Fatal("provider accepted a new lease after a failed final release")
			}
		})
	}
}
