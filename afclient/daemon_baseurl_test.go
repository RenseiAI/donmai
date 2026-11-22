package afclient

import (
	"net/url"
	"testing"
)

func TestDaemonConfigBaseURL(t *testing.T) {
	tests := []struct {
		host string
		port int
		want string
	}{
		{"", 0, "http://127.0.0.1:7734"},
		{"127.0.0.1", 8080, "http://127.0.0.1:8080"},
		{"localhost", 7734, "http://localhost:7734"},
		{"::1", 60874, "http://[::1]:60874"},
		{"[::1]", 60874, "http://[::1]:60874"},
		{"fe80::1", 7734, "http://[fe80::1]:7734"},
	}
	for _, tt := range tests {
		got := DaemonConfig{Host: tt.host, Port: tt.port}.BaseURL()
		if got != tt.want {
			t.Errorf("BaseURL(%q, %d) = %q, want %q", tt.host, tt.port, got, tt.want)
		}
		if _, err := url.Parse(got + "/api/daemon/observations"); err != nil {
			t.Errorf("BaseURL(%q, %d) = %q does not parse: %v", tt.host, tt.port, got, err)
		}
	}
}
