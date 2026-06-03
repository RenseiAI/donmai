package daemon

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestIsNewerVersion(t *testing.T) {
	cases := []struct {
		cand, cur string
		want      bool
	}{
		{"0.2.0", "0.1.0", true},
		{"v0.2.0", "0.1.0", true},
		{"0.1.0", "0.1.0", false},
		{"0.1.0", "0.2.0", false},
		{"1.0.0", "0.99.99", true},
		{"0.0.10", "0.0.9", true},
	}
	for _, c := range cases {
		if got := IsNewerVersion(c.cand, c.cur); got != c.want {
			t.Errorf("IsNewerVersion(%q, %q) = %v, want %v", c.cand, c.cur, got, c.want)
		}
	}
}

func TestUpdater_CheckForUpdate_UpToDate(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(VersionManifest{Version: "0.1.0", SHA256: "abc"})
	}))
	t.Cleanup(srv.Close)
	u := NewUpdater(UpdaterOptions{
		CurrentVersion: "0.1.0",
		Config:         AutoUpdateConfig{Channel: ChannelStable},
		CDNBase:        srv.URL,
	})
	m, err := u.CheckForUpdate(context.Background())
	if err != nil {
		t.Fatalf("CheckForUpdate: %v", err)
	}
	if m != nil {
		t.Errorf("expected nil manifest when up-to-date, got %+v", m)
	}
}

func TestUpdater_CheckForUpdate_NewerAvailable(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(VersionManifest{Version: "0.5.0", SHA256: "abc"})
	}))
	t.Cleanup(srv.Close)
	u := NewUpdater(UpdaterOptions{
		CurrentVersion: "0.1.0",
		Config:         AutoUpdateConfig{Channel: ChannelStable},
		CDNBase:        srv.URL,
	})
	m, err := u.CheckForUpdate(context.Background())
	if err != nil {
		t.Fatalf("CheckForUpdate: %v", err)
	}
	if m == nil || m.Version != "0.5.0" {
		t.Fatalf("expected manifest 0.5.0, got %+v", m)
	}
}

func TestUpdater_RunUpdate_RejectsByDefaultVerifier(t *testing.T) {
	// Manifest reports a newer version with a CORRECT sha256 of the served
	// binary, so the new checksum-integrity gate passes and the request still
	// reaches (and is rejected by) the default verifier — the swap MUST be
	// aborted at the signature stage. (sha256("fakebinary") below.)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/latest.json"):
			_ = json.NewEncoder(w).Encode(VersionManifest{Version: "9.9.9", SHA256: "035d560c0302d4caedadfc31e1b9d76dfad27622b64282576c34244a4fed6458"})
		case strings.HasSuffix(r.URL.Path, ".sig"):
			_, _ = w.Write([]byte("fakesig"))
		default:
			_, _ = w.Write([]byte("fakebinary"))
		}
	}))
	t.Cleanup(srv.Close)
	u := NewUpdater(UpdaterOptions{
		CurrentVersion: "0.1.0",
		Config:         AutoUpdateConfig{Channel: ChannelStable},
		CDNBase:        srv.URL,
		SkipExit:       true,
	})
	res, err := u.RunUpdate(context.Background())
	if err != nil {
		t.Fatalf("RunUpdate: %v", err)
	}
	if res.Updated {
		t.Errorf("expected Updated=false (default verifier rejects), got %+v", res)
	}
	if !strings.Contains(res.Reason, "sig-rejected") {
		t.Errorf("expected sig-rejected reason, got %q", res.Reason)
	}
}

func TestUpdater_RunUpdate_NoUpdate(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/latest.json") {
			_ = json.NewEncoder(w).Encode(VersionManifest{Version: "0.1.0"})
			return
		}
		http.Error(w, "unexpected path", http.StatusNotFound)
	}))
	t.Cleanup(srv.Close)
	u := NewUpdater(UpdaterOptions{
		CurrentVersion: "0.1.0",
		Config:         AutoUpdateConfig{Channel: ChannelStable},
		CDNBase:        srv.URL,
		SkipExit:       true,
	})
	res, err := u.RunUpdate(context.Background())
	if err != nil {
		t.Fatalf("RunUpdate: %v", err)
	}
	if res.Updated {
		t.Errorf("expected no update, got %+v", res)
	}
	if res.Reason != "already-up-to-date" {
		t.Errorf("Reason = %q, want already-up-to-date", res.Reason)
	}
}
