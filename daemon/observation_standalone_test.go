package daemon

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
)

// TestObservationScanStandaloneListener drives the real control listener
// over HTTP: it proves the registered route, the control-token gate and
// the handler agree when the daemon serves on an ephemeral port with no
// quota probes and no credentials. An empty full report is expected on a
// host with no logins.
func TestObservationScanStandaloneListener(t *testing.T) {
	_, srv, cleanup := mustStartDaemonWith(t, func(o *Options) {
		o.SkipRegistration = true
		o.RequireControlToken = true
		o.ControlToken = testControlToken
	})
	defer cleanup()
	addr := srv.Addr()

	post := func(body string, token string) (int, []byte) {
		t.Helper()
		req, err := http.NewRequest(http.MethodPost, "http://"+addr+"/api/daemon/observations/scan", strings.NewReader(body)) //nolint:gosec
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Content-Type", "application/json")
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("POST scan: %v", err)
		}
		defer func() { _ = res.Body.Close() }()
		raw, _ := io.ReadAll(res.Body)
		return res.StatusCode, raw
	}
	get := func(path string) (int, []byte) {
		t.Helper()
		res, err := http.Get("http://" + addr + path) //nolint:gosec
		if err != nil {
			t.Fatalf("GET %s: %v", path, err)
		}
		defer func() { _ = res.Body.Close() }()
		raw, _ := io.ReadAll(res.Body)
		return res.StatusCode, raw
	}

	// The gate refuses an unauthenticated scan before any handler runs.
	if code, _ := post(`{"kinds":[]}`, ""); code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated scan = %d, want 401", code)
	}

	// First authenticated scan: 200 with a full subscription-login report.
	code, raw := post(`{"kinds":[]}`, testControlToken)
	if code != http.StatusOK {
		t.Fatalf("first scan = %d (%s), want 200", code, raw)
	}
	var first struct {
		Report struct {
			Full  bool     `json:"full"`
			Kinds []string `json:"kinds"`
			Items []any    `json:"items"`
		} `json:"report"`
		Marks []any `json:"marks"`
	}
	if err := json.Unmarshal(raw, &first); err != nil {
		t.Fatalf("decode first scan: %v (%s)", err, raw)
	}
	if !first.Report.Full || len(first.Report.Kinds) != 1 || first.Report.Kinds[0] != "subscription-login" {
		t.Fatalf("first scan report = %+v, want a full subscription-login report", first.Report)
	}
	if first.Marks == nil {
		t.Fatal("first scan marks = nil, want an (empty) array")
	}

	// Immediate second scan: the one-a-minute gate refuses with 429.
	code, raw = post(`{"kinds":[]}`, testControlToken)
	if code != http.StatusTooManyRequests {
		t.Fatalf("second scan = %d (%s), want 429", code, raw)
	}
	var limited struct {
		Reason            string `json:"reason"`
		RetryAfterSeconds int    `json:"retryAfterSeconds"`
	}
	if err := json.Unmarshal(raw, &limited); err != nil {
		t.Fatalf("decode rate limit: %v (%s)", err, raw)
	}
	if limited.Reason != observationScanRateLimited || limited.RetryAfterSeconds <= 0 {
		t.Fatalf("rate limit = %+v (%s), want reason %q and a positive retry", limited, raw, observationScanRateLimited)
	}

	// Trailing data is refused with 400 before any scan slot is taken.
	if code, raw := post(`{"kinds":[]}{"kinds":["x"]}`, testControlToken); code != http.StatusBadRequest {
		t.Fatalf("trailing body = %d (%s), want 400", code, raw)
	}

	// The cached read stays open and returns the same report shape.
	if code, raw := get("/api/daemon/observations"); code != http.StatusOK {
		t.Fatalf("GET observations = %d (%s), want 200", code, raw)
	} else {
		var read struct {
			Report struct {
				Full  bool     `json:"full"`
				Kinds []string `json:"kinds"`
				Items []any    `json:"items"`
			} `json:"report"`
			Marks []any `json:"marks"`
		}
		if err := json.Unmarshal(raw, &read); err != nil {
			t.Fatalf("decode read: %v (%s)", err, raw)
		}
		if !read.Report.Full || strings.Join(read.Report.Kinds, ",") != "subscription-login" {
			t.Fatalf("read report = %+v, want the same full subscription-login shape as the scan", read.Report)
		}
		if read.Marks == nil {
			t.Fatal("read marks = nil, want an (empty) array like the scan")
		}
	}
}

// TestObservationScanStandaloneListenerIPv6 proves the same scan answers on
// the IPv6 loopback bind the CLI reaches with a raw `--host ::1`.
func TestObservationScanStandaloneListenerIPv6(t *testing.T) {
	_, srv, cleanup := mustStartDaemonWith(t, func(o *Options) {
		o.SkipRegistration = true
		o.HTTPHost = "::1"
		o.RequireControlToken = true
		o.ControlToken = testControlToken
	})
	defer cleanup()
	addr := srv.Addr()
	if !strings.HasPrefix(addr, "[::1]:") {
		t.Fatalf("addr = %q, want an IPv6 loopback bind", addr)
	}
	var buf bytes.Buffer
	_ = json.NewEncoder(&buf).Encode(map[string][]string{"kinds": {}})
	req, err := http.NewRequest(http.MethodPost, "http://"+addr+"/api/daemon/observations/scan", &buf) //nolint:gosec
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+testControlToken)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST scan on %s: %v", addr, err)
	}
	defer func() { _ = res.Body.Close() }()
	if res.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(res.Body)
		t.Fatalf("IPv6 scan = %d (%s), want 200", res.StatusCode, raw)
	}
}
