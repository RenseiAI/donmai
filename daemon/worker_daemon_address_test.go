package daemon

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// lastEnvValue returns the value the exec'd child would see for key.
//
// composeEnv appends the daemon's own entries after os.Environ(), and the last
// occurrence is the one that wins at exec — so a test that took the first
// match could pass while the child still saw the operator's stale value.
func lastEnvValue(env []string, key string) (string, bool) {
	prefix := key + "="
	value := ""
	found := false
	for _, kv := range env {
		if strings.HasPrefix(kv, prefix) {
			value = strings.TrimPrefix(kv, prefix)
			found = true
		}
	}
	return value, found
}

// pinProcessDaemonURL makes the PROCESS environment these rows observe
// deterministic, because composeEnv prepends os.Environ() and lastEnvValue —
// correctly, for exec's last-wins semantics — would otherwise report whatever
// the developer's own shell exports.
//
// That is not a hypothetical shell: exporting DONMAI_DAEMON_URL at the daemon's
// exec boundary is the operator workaround this change exists to retire, so the
// people most likely to run this suite are the ones still carrying it. Left
// unpinned, the two "must stay unset" rows go red on their machines and green
// on CI.
//
// t.Setenv(key, "") is NOT enough: an empty assignment still lands in
// os.Environ(), so the helper would report the key as set. The variable has to
// be genuinely removed, hence os.Unsetenv plus an explicit restore.
func pinProcessDaemonURL(t *testing.T, value string) {
	t.Helper()
	previous, had := os.LookupEnv(EnvDaemonControlURL)
	t.Cleanup(func() {
		if had {
			if err := os.Setenv(EnvDaemonControlURL, previous); err != nil {
				t.Errorf("restoring %s: %v", EnvDaemonControlURL, err)
			}
			return
		}
		if err := os.Unsetenv(EnvDaemonControlURL); err != nil {
			t.Errorf("clearing %s: %v", EnvDaemonControlURL, err)
		}
	})
	if value == "" {
		if err := os.Unsetenv(EnvDaemonControlURL); err != nil {
			t.Fatalf("clearing %s: %v", EnvDaemonControlURL, err)
		}
		return
	}
	if err := os.Setenv(EnvDaemonControlURL, value); err != nil {
		t.Fatalf("setting %s: %v", EnvDaemonControlURL, err)
	}
}

// TestSpawner_DaemonControlURLReachesWorkerEnv is the core of the fix: a
// daemon that is NOT on the well-known port must still be findable by the
// workers it spawns.
//
// Each row spawns through both ownership paths, because a session's owner is
// decided before any env is composed and a fix that only covered one of them
// would leave the other dialling a port nothing serves.
func TestSpawner_DaemonControlURLReachesWorkerEnv(t *testing.T) {
	tests := []struct {
		name string
		// resolver stands in for the daemon's own ControlURL accessor.
		resolver func() string
		// baseEnv is what the operator's environment / .env.local contributed.
		baseEnv map[string]string
		// specEnv is what the platform stamped on the work item.
		specEnv map[string]string
		// processEnv is what the DAEMON'S OWN process environment carries —
		// the operator's export, inherited through composeEnv's os.Environ().
		// Empty means the variable is removed for the row.
		processEnv string
		want       string
		wantSet    bool
	}{
		{
			name:     "named instance on a non-default port",
			resolver: func() string { return "http://127.0.0.1:18382" },
			want:     "http://127.0.0.1:18382",
			wantSet:  true,
		},
		{
			name:     "default instance still states its address",
			resolver: func() string { return "http://127.0.0.1:7734" },
			want:     "http://127.0.0.1:7734",
			wantSet:  true,
		},
		{
			name:     "loopback IPv6 instance",
			resolver: func() string { return "http://[::1]:18382" },
			want:     "http://[::1]:18382",
			wantSet:  true,
		},
		{
			name:     "the daemon's address beats an inherited operator value",
			resolver: func() string { return "http://127.0.0.1:18382" },
			baseEnv:  map[string]string{EnvDaemonControlURL: "http://127.0.0.1:7734"},
			want:     "http://127.0.0.1:18382",
			wantSet:  true,
		},
		{
			// The shape that actually bites in the field: the operator (or the
			// downstream binary's own named-instance workaround) exports the
			// variable on the daemon process, and every worker inherits it.
			name:       "the daemon's address beats an inherited process-environment value",
			resolver:   func() string { return "http://127.0.0.1:18382" },
			processEnv: "http://127.0.0.1:7734",
			want:       "http://127.0.0.1:18382",
			wantSet:    true,
		},
		{
			name:     "the daemon's address beats a stamped session value",
			resolver: func() string { return "http://127.0.0.1:18382" },
			specEnv:  map[string]string{EnvDaemonControlURL: "http://127.0.0.1:9999"},
			want:     "http://127.0.0.1:18382",
			wantSet:  true,
		},
		{
			name:     "an unknown address is left unstated rather than guessed",
			resolver: func() string { return "  " },
			wantSet:  false,
		},
		{
			name:     "no resolver leaves the variable to the worker's own resolution",
			resolver: nil,
			wantSet:  false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			for _, path := range []string{"direct", "shim"} {
				t.Run(path, func(t *testing.T) {
					pinProcessDaemonURL(t, tt.processEnv)
					var (
						mu     sync.Mutex
						gotEnv []string
					)
					opts := SpawnerOptions{
						Projects:              []ProjectConfig{{ID: "p", Repository: "github.com/a/b"}},
						MaxConcurrentSessions: 1,
						BaseEnv:               tt.baseEnv,
						DaemonControlURL:      tt.resolver,
						WorkerCommand:         []string{"/bin/sh", "-c", "exit 0"},
						OnPreSpawn: func(_ SessionSpec, env []string) ([]string, error) {
							mu.Lock()
							gotEnv = append([]string(nil), env...)
							mu.Unlock()
							return nil, nil
						},
					}
					launched := make(chan struct{}, 1)
					if path == "shim" {
						opts.ShimOwns = func(SessionSpec) bool { return true }
						opts.ShimSpawn = func(spec SessionSpec, _ ProjectConfig, env []string) (*SessionHandle, error) {
							mu.Lock()
							gotEnv = append([]string(nil), env...)
							mu.Unlock()
							select {
							case launched <- struct{}{}:
							default:
							}
							return &SessionHandle{SessionID: spec.SessionID, State: SessionRunning}, nil
						}
					}
					s := NewWorkerSpawner(opts)
					ended := sessionEnds(s)
					if _, err := s.AcceptWork(SessionSpec{
						SessionID:  "sess-addr",
						Repository: "github.com/a/b",
						Ref:        "main",
						Env:        tt.specEnv,
					}); err != nil {
						t.Fatalf("AcceptWork: %v", err)
					}
					if path == "shim" {
						select {
						case <-launched:
						case <-time.After(spawnerWaitTimeout):
							t.Fatal("timed out waiting for the shim launcher")
						}
					} else {
						waitSessionEnd(t, ended)
					}

					mu.Lock()
					env := append([]string(nil), gotEnv...)
					mu.Unlock()
					got, ok := lastEnvValue(env, EnvDaemonControlURL)
					if ok != tt.wantSet {
						t.Fatalf("%s set = %v, want %v (got %q)", EnvDaemonControlURL, ok, tt.wantSet, got)
					}
					if tt.wantSet && got != tt.want {
						t.Errorf("%s = %q, want %q", EnvDaemonControlURL, got, tt.want)
					}
				})
			}
		})
	}
}

// TestDaemonControlURL covers the accessor the spawner consults: what a
// daemon answers when asked where it lives.
func TestDaemonControlURL(t *testing.T) {
	tests := []struct {
		name      string
		host      string
		port      int
		published string
		want      string
	}{
		{
			name: "configured named-instance port",
			host: "127.0.0.1",
			port: 18382,
			want: "http://127.0.0.1:18382",
		},
		{
			name: "empty host falls back to the loopback default",
			port: 18382,
			want: "http://127.0.0.1:18382",
		},
		{
			name: "unbound ephemeral port has no answer to give",
			host: "127.0.0.1",
			port: 0,
			want: "",
		},
		{
			// ResolveControlBind accepts a host that carries its own port.
			// Joining host and port again would produce "[localhost:7735]:7735".
			name: "a host carrying its own port is resolved, not joined again",
			host: "localhost:7735",
			port: 7735,
			want: "http://localhost:7735",
		},
		{
			name: "a host-embedded port answers even with no explicit port set",
			host: "localhost:7735",
			port: 0,
			want: "http://localhost:7735",
		},
		{
			// The worse of the two mis-renderings: an empty answer omits the
			// variable, the worker falls back to the built-in default, and the
			// fix becomes a no-op for exactly the instance it was written for.
			name: "a bracketed IPv6 literal host keeps its address",
			host: "[::1]",
			port: 7735,
			want: "http://[::1]:7735",
		},
		{
			name: "a bare IPv6 literal host keeps its address",
			host: "::1",
			port: 7735,
			want: "http://[::1]:7735",
		},
		{
			// The listener would refuse this bind outright (the control API is
			// unauthenticated), so the accessor must not name it either.
			name: "a non-loopback configured host has no control address to give",
			host: "0.0.0.0",
			port: 7735,
			want: "",
		},
		{
			name: "a configured host:port that contradicts the explicit port is no address",
			host: "localhost:7735",
			port: 7734,
			want: "",
		},
		{
			name:      "a bound listener outranks the configured port",
			host:      "127.0.0.1",
			port:      7734,
			published: "127.0.0.1:18382",
			want:      "http://127.0.0.1:18382",
		},
		{
			name:      "an ephemeral bind is knowable only once published",
			host:      "127.0.0.1",
			port:      0,
			published: "127.0.0.1:52341",
			want:      "http://127.0.0.1:52341",
		},
		{
			name:      "an unspecified bind host is narrowed to loopback",
			host:      "127.0.0.1",
			port:      0,
			published: "0.0.0.0:52341",
			want:      "http://127.0.0.1:52341",
		},
		{
			name:      "an unspecified IPv6 bind host is narrowed to loopback",
			host:      "127.0.0.1",
			port:      0,
			published: "[::]:52341",
			want:      "http://[::1]:52341",
		},
		{
			name:      "an IPv6 literal keeps its brackets",
			host:      "127.0.0.1",
			port:      0,
			published: "[::1]:52341",
			want:      "http://[::1]:52341",
		},
		{
			name:      "an unparseable address is ignored, not published",
			host:      "127.0.0.1",
			port:      18382,
			published: "not-an-address",
			want:      "http://127.0.0.1:18382",
		},
		{
			name:      "a port-zero address is not an address yet",
			host:      "127.0.0.1",
			port:      18382,
			published: "127.0.0.1:0",
			want:      "http://127.0.0.1:18382",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d := New(Options{SkipRegistration: true, HTTPHost: tt.host, HTTPPort: tt.port})
			if tt.published != "" {
				d.PublishControlAddr(tt.published)
			}
			if got := d.ControlURL(); got != tt.want {
				t.Errorf("ControlURL() = %q, want %q", got, tt.want)
			}
		})
	}
}

// TestNamedInstanceDaemonSpawnsWorkerAtItsOwnAddress is the end-to-end
// statement of the bug: a daemon whose control listener is NOT on the
// well-known port spawns a worker, and that worker is told where its parent
// actually is.
//
// The daemon binds an ephemeral port, which is the named-instance shape
// exactly — a port the worker's built-in default cannot possibly be.
func TestNamedInstanceDaemonSpawnsWorkerAtItsOwnAddress(t *testing.T) {
	tmp := t.TempDir()
	configPath := filepath.Join(tmp, "daemon.yaml")
	cfg := Config{
		APIVersion: "donmai.dev/v1", Kind: "LocalDaemon",
		Machine:      MachineConfig{ID: "named-instance-machine"},
		Orchestrator: OrchestratorConfig{URL: "https://example.test"},
		Projects:     []ProjectConfig{{ID: "p", Repository: "github.com/a/b"}},
	}
	cfg.Capacity.MaxConcurrentSessions = 1
	if err := WriteConfig(configPath, &cfg); err != nil {
		t.Fatal(err)
	}

	var (
		mu     sync.Mutex
		gotEnv []string
	)
	d := New(Options{
		ConfigPath:       configPath,
		JWTPath:          filepath.Join(tmp, "daemon.jwt"),
		SkipWizard:       true,
		SkipRegistration: true,
		// Zero => ephemeral: whatever the kernel hands out is, by
		// construction, not the well-known default port.
		HTTPPort: 0,
		SpawnerOptions: SpawnerOptions{
			WorkerCommand: []string{"/bin/sh", "-c", "exit 0"},
			OnPreSpawn: func(_ SessionSpec, env []string) ([]string, error) {
				mu.Lock()
				gotEnv = append([]string(nil), env...)
				mu.Unlock()
				return nil, nil
			},
		},
	})

	srv := NewServer(d)
	errCh, err := srv.StartBeforeDaemon()
	if err != nil {
		t.Fatalf("StartBeforeDaemon: %v", err)
	}
	t.Cleanup(func() {
		shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutCtx)
		<-errCh
	})
	if err := d.Start(context.Background()); err != nil {
		t.Fatalf("daemon start: %v", err)
	}
	srv.DaemonStarted()
	t.Cleanup(func() {
		stopCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = d.Stop(stopCtx)
	})

	wantURL := "http://" + srv.Addr()
	if got := d.ControlURL(); got != wantURL {
		t.Fatalf("ControlURL() = %q, want the bound listener %q", got, wantURL)
	}
	if strings.HasSuffix(srv.Addr(), ":"+strconv.Itoa(DefaultHTTPPort)) {
		t.Fatalf("ephemeral bind landed on the default port %d; this test proves nothing", DefaultHTTPPort)
	}

	ended := sessionEnds(d.spawner)
	if _, err := d.AcceptWork(SessionSpec{
		SessionID:  "sess-named-instance",
		Repository: "github.com/a/b",
		Ref:        "main",
	}); err != nil {
		t.Fatalf("AcceptWork: %v", err)
	}
	waitSessionEnd(t, ended)

	mu.Lock()
	env := append([]string(nil), gotEnv...)
	mu.Unlock()
	got, ok := lastEnvValue(env, EnvDaemonControlURL)
	if !ok {
		t.Fatalf("worker env carries no %s; it would dial the built-in default and die mute", EnvDaemonControlURL)
	}
	if got != wantURL {
		t.Errorf("%s = %q, want the daemon's own listener %q", EnvDaemonControlURL, got, wantURL)
	}

	// And the address really is this daemon's: a worker dialling it reaches
	// the session-detail route rather than a connection refused.
	resp, err := http.Get(got + "/api/daemon/sessions/sess-named-instance") //nolint:noctx // loopback test daemon
	if err != nil {
		t.Fatalf("dialling the advertised control URL: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode >= 500 {
		t.Errorf("advertised control URL answered %d", resp.StatusCode)
	}
}
