package daemon

import (
	"context"
	"fmt"
	"net"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// TestNew_HTTPPortZero_IsEphemeral asserts the Wave 12 / C3 contract:
// constructing a Daemon with Options{HTTPPort: 0} preserves the zero
// value. The library no longer auto-fills DefaultHTTPPort — callers
// that need 7734 (afcli/daemon_run.go) substitute it themselves before
// constructing Options. Tests pass HTTPPort: 0 to mean "ephemeral
// port: kernel picks a free port at listener bind time".
func TestNew_HTTPPortZero_IsEphemeral(t *testing.T) {
	t.Parallel()
	d := New(Options{
		ConfigPath: "/dev/null",
		HTTPHost:   "127.0.0.1",
		HTTPPort:   0,
	})
	if got := d.opts.HTTPPort; got != 0 {
		t.Fatalf("opts.HTTPPort after New = %d, want 0 (ephemeral); the library must NOT auto-fill DefaultHTTPPort", got)
	}
}

// TestNew_HTTPPortExplicit_Preserved double-checks the cobra-layer
// substitution path: when the caller supplies an explicit port (e.g.
// afcli/daemon_run.go after `if port == 0 { port = DefaultHTTPPort }`),
// New preserves the value verbatim.
func TestNew_HTTPPortExplicit_Preserved(t *testing.T) {
	t.Parallel()
	d := New(Options{
		ConfigPath: "/dev/null",
		HTTPHost:   "127.0.0.1",
		HTTPPort:   DefaultHTTPPort,
	})
	if got := d.opts.HTTPPort; got != DefaultHTTPPort {
		t.Fatalf("opts.HTTPPort = %d, want %d (cobra layer substitutes the well-known port)", got, DefaultHTTPPort)
	}
}

// TestEphemeralPortNoCollision soaks the C3 fix: N parallel subtests
// each construct a Daemon + Server with HTTPPort: 0 and call Start.
// All N must bind successfully (no shared 7734 collision) and
// Server.Addr() must report a non-zero ephemeral port string.
//
// Run under -race with -count=5 to catch lingering flakiness; this
// test exists specifically because Wave 11's parallel daemon tests
// hit the port-7734 bind race when daemon.New auto-filled the
// default port.
func TestEphemeralPortNoCollision(t *testing.T) {
	t.Parallel()

	const N = 8

	var (
		mu        sync.Mutex
		seenPorts = make(map[int]string)
	)

	for i := 0; i < N; i++ {
		i := i
		t.Run(fmt.Sprintf("parallel-%d", i), func(t *testing.T) {
			t.Parallel()

			// Build a Server directly — avoids Daemon.Start's
			// registration RPC + config-load path. The C3 fix is
			// about Server.Start binding; a Daemon shell is enough
			// to thread Options{HTTPPort: 0} through NewServer.
			d := New(Options{
				ConfigPath: "/dev/null",
				HTTPHost:   "127.0.0.1",
				HTTPPort:   0,
			})

			srv := NewServer(d)
			errCh, err := srv.Start()
			if err != nil {
				t.Fatalf("server Start: %v", err)
			}
			t.Cleanup(func() {
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				_ = srv.Shutdown(ctx)
				// Drain errCh so the serve goroutine exits cleanly.
				select {
				case <-errCh:
				case <-time.After(2 * time.Second):
				}
			})

			addr := srv.Addr()
			if addr == "" {
				t.Fatalf("Server.Addr() = empty after Start; expected ephemeral host:port")
			}
			// Server.Addr() must NOT report the literal "127.0.0.1:0"
			// after Start — the listener captures the kernel-picked
			// port via listener.Addr() and rewrites s.addr.
			if strings.HasSuffix(addr, ":0") {
				t.Fatalf("Server.Addr() = %q after Start; ephemeral port was not captured", addr)
			}

			_, portStr, err := net.SplitHostPort(addr)
			if err != nil {
				t.Fatalf("net.SplitHostPort(%q): %v", addr, err)
			}
			port, err := strconv.Atoi(portStr)
			if err != nil {
				t.Fatalf("port not numeric in %q: %v", addr, err)
			}
			if port == 0 {
				t.Fatalf("ephemeral port resolved to 0 in %q", addr)
			}

			mu.Lock()
			defer mu.Unlock()
			if prev, ok := seenPorts[port]; ok {
				t.Fatalf("port %d already bound by subtest %s — kernel handed out the same port to two listeners (this should be impossible while listeners are still live)", port, prev)
			}
			seenPorts[port] = fmt.Sprintf("parallel-%d", i)
		})
	}
}
