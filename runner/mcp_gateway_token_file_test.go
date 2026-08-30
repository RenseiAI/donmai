package runner

import (
	"context"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/RenseiAI/donmai/agent"
	"github.com/RenseiAI/donmai/provider/harness/stub"
)

type mcpBearerEnvCaptureProvider struct {
	agent.Provider

	mu        sync.Mutex
	path      string
	bearer    string
	mode      os.FileMode
	readError error
}

func (p *mcpBearerEnvCaptureProvider) Spawn(ctx context.Context, spec agent.Spec) (agent.Handle, error) {
	path := spec.Env[mcpGatewayTokenFileEnv]
	raw, readErr := os.ReadFile(path)
	var mode os.FileMode
	if info, statErr := os.Stat(path); statErr == nil {
		mode = info.Mode().Perm()
	} else if readErr == nil {
		readErr = statErr
	}
	p.mu.Lock()
	p.path, p.bearer, p.mode, p.readError = path, string(raw), mode, readErr
	p.mu.Unlock()
	return p.Provider.Spawn(ctx, spec)
}

func (p *mcpBearerEnvCaptureProvider) snapshot() (string, string, os.FileMode, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.path, p.bearer, p.mode, p.readError
}

func TestPrepareSessionMCPBearerEnv_MaterializesSessionTokenForChild(t *testing.T) {
	t.Parallel()

	env := map[string]string{}
	qw := QueuedWork{McpAuthToken: "  session-bearer-exact\n"}
	cleanup, err := prepareSessionMCPBearerEnv(qw, env, "")
	if err != nil {
		t.Fatalf("prepareSessionMCPBearerEnv: %v", err)
	}
	t.Cleanup(cleanup)

	path := env[mcpGatewayTokenFileEnv]
	if path == "" {
		t.Fatalf("env missing %s", mcpGatewayTokenFileEnv)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read bootstrap bearer file: %v", err)
	}
	if got := string(raw); got != "session-bearer-exact" {
		t.Fatalf("bootstrap bearer = %q, want exact trimmed session bearer", got)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat bootstrap bearer file: %v", err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Fatalf("bootstrap bearer mode = %o, want 600", got)
	}

	cleanup()
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("bootstrap bearer still exists after cleanup: %v", err)
	}
}

func TestPrepareSessionMCPBearerEnv_ExistingRefreshFileWins(t *testing.T) {
	t.Parallel()

	env := map[string]string{"UNCHANGED": "yes"}
	cleanup, err := prepareSessionMCPBearerEnv(
		QueuedWork{McpAuthToken: "session-bearer-must-not-be-projected"},
		env,
		" /daemon/session/mcp-token ",
	)
	if err != nil {
		t.Fatalf("prepareSessionMCPBearerEnv: %v", err)
	}
	cleanup()
	if _, ok := env[mcpGatewayTokenFileEnv]; ok {
		t.Fatalf("runner overrode the daemon-owned %s path: %v", mcpGatewayTokenFileEnv, env)
	}
	if env["UNCHANGED"] != "yes" {
		t.Fatalf("unrelated env changed: %v", env)
	}
}

func TestPrepareSessionMCPBearerEnv_DoesNotProjectWorkerBearer(t *testing.T) {
	t.Parallel()

	env := map[string]string{}
	cleanup, err := prepareSessionMCPBearerEnv(
		QueuedWork{AuthToken: "worker-runtime-bearer", McpAuthToken: " \t"},
		env,
		"",
	)
	if err != nil {
		t.Fatalf("prepareSessionMCPBearerEnv: %v", err)
	}
	cleanup()
	if _, ok := env[mcpGatewayTokenFileEnv]; ok {
		t.Fatalf("worker bearer was projected onto the session bearer rail: %v", env)
	}
	for _, value := range env {
		if strings.Contains(value, "worker-runtime-bearer") {
			t.Fatalf("worker bearer leaked into child env: %v", env)
		}
	}
}

func TestRunLoop_PublishesSessionMCPBearerFileToRealSpawn(t *testing.T) {
	t.Setenv(mcpGatewayTokenFileEnv, "")
	h := newRunnerHarness(t)
	inner, err := stub.New()
	if err != nil {
		t.Fatalf("stub.New: %v", err)
	}
	capture := &mcpBearerEnvCaptureProvider{Provider: inner}
	if err := h.runner.registry.Register(capture); err != nil {
		t.Fatalf("register capture provider: %v", err)
	}

	qw := h.queuedWork("MCP-BEARER-ENV-SPAWN")
	qw.McpAuthToken = "session-bearer-real-spawn"
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	res, err := h.runner.Run(ctx, qw)
	if err != nil {
		t.Fatalf("Run: %v (failureMode=%q error=%q)", err, res.FailureMode, res.Error)
	}
	if res.Status != "completed" {
		t.Fatalf("Run status = %q, want completed", res.Status)
	}

	path, bearer, mode, readErr := capture.snapshot()
	if readErr != nil {
		t.Fatalf("provider could not read session bearer file at Spawn: %v", readErr)
	}
	if path == "" || bearer != "session-bearer-real-spawn" || mode != 0o600 {
		t.Fatalf("spawn bearer rail = path %q bearer %q mode %o, want non-empty/exact/600", path, bearer, mode)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("runner-owned bearer file still exists after Run teardown: %v", err)
	}
}
