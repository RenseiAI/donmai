package daemon

import (
	"context"
	"strings"
	"testing"
	"time"
)

func TestResolveControlBind(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name         string
		rawHost      string
		port         int
		portExplicit bool
		wantHost     string
		wantPort     int
		wantErr      string
	}{
		{name: "default", wantHost: "127.0.0.1"},
		{name: "localhost", rawHost: "LOCALHOST", wantHost: "localhost"},
		{name: "ipv4 loopback range", rawHost: "127.12.34.56", wantHost: "127.12.34.56"},
		{name: "ipv6 loopback", rawHost: "::1", wantHost: "::1"},
		{name: "bracketed ipv6 loopback", rawHost: "[::1]", wantHost: "::1"},
		{name: "mapped ipv4 loopback", rawHost: "::ffff:127.0.0.1", wantHost: "127.0.0.1"},
		{name: "host embedded port", rawHost: "localhost:8123", wantHost: "localhost", wantPort: 8123},
		{name: "ipv6 embedded port", rawHost: "[::1]:8123", wantHost: "::1", wantPort: 8123},
		{name: "matching explicit port", rawHost: "127.0.0.1:8123", port: 8123, portExplicit: true, wantHost: "127.0.0.1", wantPort: 8123},
		{name: "wildcard ipv4", rawHost: "0.0.0.0", wantErr: "refusing non-loopback"},
		{name: "wildcard ipv6", rawHost: "::", wantErr: "refusing non-loopback"},
		{name: "lan address", rawHost: "192.168.1.4", wantErr: "refusing non-loopback"},
		{name: "public address", rawHost: "203.0.113.7", wantErr: "refusing non-loopback"},
		{name: "arbitrary hostname", rawHost: "example.invalid", wantErr: "not localhost or a loopback IP literal"},
		{name: "malformed address", rawHost: "[::1", wantErr: "invalid bracketed control bind address"},
		{name: "malformed embedded port", rawHost: "127.0.0.1:port", wantErr: "invalid control bind port"},
		{name: "invalid separate port", rawHost: "127.0.0.1", port: 65536, wantErr: "outside 0-65535"},
		{name: "conflicting ports", rawHost: "127.0.0.1:8123", port: 8124, portExplicit: true, wantErr: "conflicts with explicit port"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			host, port, err := ResolveControlBind(tt.rawHost, tt.port, tt.portExplicit)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("ResolveControlBind(%q, %d, %t) error = %v, want containing %q", tt.rawHost, tt.port, tt.portExplicit, err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("ResolveControlBind(%q, %d, %t): %v", tt.rawHost, tt.port, tt.portExplicit, err)
			}
			if host != tt.wantHost || port != tt.wantPort {
				t.Fatalf("ResolveControlBind(%q, %d, %t) = (%q, %d), want (%q, %d)", tt.rawHost, tt.port, tt.portExplicit, host, port, tt.wantHost, tt.wantPort)
			}
		})
	}
}

func TestServerStartRejectsNonLoopbackBeforeListening(t *testing.T) {
	t.Parallel()

	d := New(Options{HTTPHost: "0.0.0.0", HTTPPort: 0})
	srv := NewServer(d)
	if _, err := srv.Start(); err == nil || !strings.Contains(err.Error(), "refusing non-loopback") {
		t.Fatalf("Server.Start() error = %v, want non-loopback refusal", err)
	}
	if srv.started {
		t.Fatal("Server.Start marked the server started after rejecting its control bind")
	}
}

func TestServerStartHonorsEmbeddedLoopbackPort(t *testing.T) {
	t.Parallel()

	d := New(Options{HTTPHost: "127.0.0.1:0", HTTPPort: 0})
	srv := NewServer(d)
	errCh, err := srv.Start()
	if err != nil {
		t.Fatalf("Server.Start(): %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = srv.Shutdown(ctx)
		<-errCh
	})
	if !strings.HasPrefix(srv.Addr(), "127.0.0.1:") {
		t.Fatalf("Server.Addr() = %q, want normalized loopback address", srv.Addr())
	}
}
