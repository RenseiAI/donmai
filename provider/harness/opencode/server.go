package opencode

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/exec"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/RenseiAI/donmai/agent"
	runtimeenv "github.com/RenseiAI/donmai/runtime/env"
)

// ─── opencode serve child lifecycle (07 §6) ──────────────────────────────────
//
// Process model (decided in 07 §2): ONE `opencode serve` child per donmai
// session, on an ephemeral loopback port the provider owns, torn down with the
// session. Per-session config injection is trivially safe when the server IS
// per-session; it avoids shared-server session leakage across worktrees. The
// OPENCODE_ENDPOINT attach-to-external-server escape hatch bypasses the child
// entirely (operators / tests point at their own `opencode serve`).

// serveReadyTimeout bounds how long startServe waits for the child to report
// healthy before giving up and tearing it down.
const serveReadyTimeout = 20 * time.Second

// serveHealthInterval is the readiness poll cadence.
const serveHealthInterval = 100 * time.Millisecond

// serveConfig carries everything startServe needs to bring up a child.
type serveConfig struct {
	binary        string            // resolved opencode binary path
	cwd           string            // session worktree
	configPath    string            // owned OPENCODE_CONFIG target (may be empty)
	env           map[string]string // additional env (spec env + resolved key)
	endpointBound bool              // exact endpoint/model/auth controls must not inherit
	apiKey        string            // bearer for health/probe (usually empty locally)
	logger        *slog.Logger
}

// serveChild is a running `opencode serve` process the provider owns.
type serveChild struct {
	cmd      *exec.Cmd
	endpoint string // http://127.0.0.1:<port>
	logger   *slog.Logger

	stopOnce sync.Once
	done     chan struct{} // closed when the reaper's cmd.Wait returns
	waitErr  atomic.Pointer[error]
}

// freeLoopbackPort asks the OS for an unused loopback TCP port. There is a
// small window between close and the child's bind; a failed bind surfaces as a
// readiness-probe timeout with an actionable error rather than a silent hang.
func freeLoopbackPort() (int, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer func() { _ = l.Close() }()
	return l.Addr().(*net.TCPAddr).Port, nil
}

// startServe spawns an `opencode serve` child on a fresh loopback port, waits
// for it to report healthy, and returns the running child. On any failure the
// child is torn down before returning.
func startServe(ctx context.Context, cfg serveConfig) (*serveChild, error) {
	port, err := freeLoopbackPort()
	if err != nil {
		return nil, fmt.Errorf("%w: allocate serve port: %v", agent.ErrSpawnFailed, err)
	}
	endpoint := fmt.Sprintf("http://127.0.0.1:%d", port)

	argv := []string{"serve", "--hostname", "127.0.0.1", "--port", fmt.Sprintf("%d", port)}
	// nolint:gosec // G204: binary resolved from PATH at construction; argv is a
	// closed set of serve flags.
	cmd := exec.Command(cfg.binary, argv...)
	if cfg.cwd != "" {
		cmd.Dir = cfg.cwd
	}
	env := map[string]string{}
	for k, v := range cfg.env {
		env[k] = v
	}
	if cfg.configPath != "" {
		env[OCConfigEnvVar] = cfg.configPath
	}
	parentEnv := os.Environ()
	if cfg.endpointBound {
		parentEnv = filterEndpointControls(parentEnv)
	}
	cmd.Env = runtimeenv.ComposeChildEnv(parentEnv, env)
	configureProcessGroup(cmd)

	logger := cfg.logger
	if logger == nil {
		logger = slog.Default()
	}

	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("%w: start opencode serve: %v", agent.ErrSpawnFailed, err)
	}

	child := &serveChild{
		cmd:      cmd,
		endpoint: endpoint,
		logger:   logger.With("component", "opencode-serve", "port", port),
		done:     make(chan struct{}),
	}
	// Single reaper owns cmd.Wait(); everything else observes s.done.
	go child.reap()

	if err := child.waitHealthy(ctx, cfg.apiKey); err != nil {
		child.stop()
		return nil, err
	}
	return child, nil
}

// reap is the single owner of cmd.Wait(); it stashes the result and closes
// done so waitHealthy / stop / exited can observe termination without racing a
// second Wait call.
func (s *serveChild) reap() {
	err := s.cmd.Wait()
	if err != nil {
		s.waitErr.Store(&err)
	}
	close(s.done)
}

// waitHealthy polls GET /api/health until the child reports healthy, the ctx
// fires, serveReadyTimeout elapses, or the child exits early.
func (s *serveChild) waitHealthy(ctx context.Context, apiKey string) error {
	client := newClientV1(s.endpoint, apiKey, &http.Client{Timeout: 2 * time.Second})
	deadline := time.Now().Add(serveReadyTimeout)
	ticker := time.NewTicker(serveHealthInterval)
	defer ticker.Stop()

	for {
		probeCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
		err := client.Health(probeCtx)
		cancel()
		if err == nil {
			return nil
		}
		select {
		case <-s.done:
			return fmt.Errorf("%w: opencode serve exited before ready: %v", agent.ErrSpawnFailed, s.waitErrValue())
		case <-ctx.Done():
			return fmt.Errorf("%w: opencode serve readiness cancelled: %v", agent.ErrSpawnFailed, ctx.Err())
		case <-ticker.C:
			if time.Now().After(deadline) {
				return fmt.Errorf("%w: opencode serve at %s not healthy within %s", agent.ErrSpawnFailed, s.endpoint, serveReadyTimeout)
			}
		}
	}
}

func (s *serveChild) waitErrValue() error {
	if p := s.waitErr.Load(); p != nil {
		return *p
	}
	return nil
}

// stop tears down the child: SIGTERM the process group, 5s grace, SIGKILL.
// Idempotent; safe after the child has already exited.
func (s *serveChild) stop() {
	s.stopOnce.Do(func() {
		select {
		case <-s.done:
			return // already exited; reaper closed done
		default:
		}
		if s.cmd == nil || s.cmd.Process == nil {
			return
		}
		signalProcessGroup(s.cmd, syscall.SIGTERM)
		select {
		case <-s.done:
		case <-time.After(stopGracePeriod):
			signalProcessGroup(s.cmd, syscall.SIGKILL)
			<-s.done
		}
	})
}

// exited reports whether the child process has already terminated (used by the
// handle's event pump to distinguish a clean session end from a serve crash).
func (s *serveChild) exited() bool {
	select {
	case <-s.done:
		return true
	default:
		return false
	}
}
