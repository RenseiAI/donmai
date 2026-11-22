package afcli

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/RenseiAI/donmai/afclient"
)

type fakeObservationsClient struct {
	obs         afclient.HostObservations
	scannedWith []string
	scanned     bool
}

func (f *fakeObservationsClient) HostObservations() (*afclient.HostObservations, error) {
	out := f.obs
	return &out, nil
}

func (f *fakeObservationsClient) ScanHostObservations(_ context.Context, kinds []string) (*afclient.HostObservations, error) {
	f.scanned, f.scannedWith = true, kinds
	out := f.obs
	return &out, nil
}

func sampleHostObservations() afclient.HostObservations {
	codex := json.RawMessage(`{"harness":"codex","provider":"openai-chatgpt","plan":"pro","allowances":[{"id":"codex","unit":"percent","windows":[{"id":"primary","kind":"session","used":5},{"id":"secondary","kind":"weekly","used":40}]}],"billing":{"mode":"allowance"}}`)
	muse := json.RawMessage(`{"harness":"muse","provider":"muse","allowances":[],"billing":{"mode":"unknown"}}`)
	return afclient.HostObservations{
		Report: afclient.HostObservationReport{Version: 1, Full: true, Kinds: []string{"subscription-login"}, Items: []afclient.HostObservationItem{
			{Kind: "subscription-login", ItemKey: "k1", Slot: "default", Readiness: afclient.ObservationReadiness{State: "ready"}, Attributes: codex},
			{Kind: "subscription-login", ItemKey: "k2", Slot: "0123456789abcdef", Readiness: afclient.ObservationReadiness{State: "unknown"}, Attributes: muse},
		}},
		Marks: []afclient.HostObservationMark{
			{Kind: "subscription-login", ItemKey: "k1", Slot: "default", New: true, Enabled: true},
			{Kind: "subscription-login", ItemKey: "k2", Slot: "0123456789abcdef", Enabled: false},
		},
	}
}

func TestHostObservationsCmd(t *testing.T) {
	tests := []struct {
		name       string
		args       []string
		wantScan   bool
		wantIn     []string
		wantAbsent []string
	}{
		{
			name: "checklist", args: nil,
			wantIn: []string{
				"[x]  subscription-login  codex/openai-chatgpt  plan=pro  slot=default  ready  primary 5% · secondary 40%  NEW",
				"[ ]  subscription-login  muse/muse  plan=-  slot=0123456789abcdef  unknown  no windows",
			},
		},
		{name: "rescan", args: []string{"--rescan", "--kind", "subscription-login"}, wantScan: true, wantIn: []string{"NEW"}},
		{name: "json", args: []string{"--json"}, wantIn: []string{`"marks": [`, `"new": true`, `"itemKey": "k1"`}},
		{name: "kind filter", args: []string{"--kind", "git-identity"}, wantIn: []string{"no observed items"}, wantAbsent: []string{"codex"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fake := &fakeObservationsClient{obs: sampleHostObservations()}
			cmd := newHostObservationsCmd(func(afclient.DaemonConfig) hostObservationsClient { return fake })
			var out bytes.Buffer
			cmd.SetOut(&out)
			cmd.SetArgs(tt.args)
			if err := cmd.Execute(); err != nil {
				t.Fatalf("execute: %v", err)
			}
			if fake.scanned != tt.wantScan || (tt.wantScan && strings.Join(fake.scannedWith, ",") != "subscription-login") {
				t.Errorf("scanned = %v with %v, want %v", fake.scanned, fake.scannedWith, tt.wantScan)
			}
			for _, want := range tt.wantIn {
				if !strings.Contains(out.String(), want) {
					t.Errorf("output lacks %q:\n%s", want, out.String())
				}
			}
			for _, absent := range tt.wantAbsent {
				if strings.Contains(out.String(), absent) {
					t.Errorf("output carries %q:\n%s", absent, out.String())
				}
			}
		})
	}
}

func TestHostObservationsCmd_KindFilterNarrowsCoverage(t *testing.T) {
	tests := []struct {
		name      string
		kind      string
		wantFull  bool
		wantKinds string
		wantItems int
	}{
		// A filter that covers none of the reported kinds is partial: it
		// never claims an authoritative "no subscription logins".
		{name: "uncovered kind", kind: "git-identity", wantFull: false, wantKinds: "", wantItems: 0},
		{name: "covered kind", kind: "subscription-login", wantFull: true, wantKinds: "subscription-login", wantItems: 2},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fake := &fakeObservationsClient{obs: sampleHostObservations()}
			cmd := newHostObservationsCmd(func(afclient.DaemonConfig) hostObservationsClient { return fake })
			var out bytes.Buffer
			cmd.SetOut(&out)
			cmd.SetArgs([]string{"--json", "--kind", tt.kind})
			if err := cmd.Execute(); err != nil {
				t.Fatalf("execute: %v", err)
			}
			var got afclient.HostObservations
			if err := json.Unmarshal(out.Bytes(), &got); err != nil {
				t.Fatalf("decode: %v\n%s", err, out.String())
			}
			if got.Report.Full != tt.wantFull || strings.Join(got.Report.Kinds, ",") != tt.wantKinds || len(got.Report.Items) != tt.wantItems {
				t.Fatalf("full=%v kinds=%v items=%d, want full=%v kinds=[%s] items=%d", got.Report.Full, got.Report.Kinds, len(got.Report.Items), tt.wantFull, tt.wantKinds, tt.wantItems)
			}
		})
	}
}

func TestHostObservationsCmd_IPv6HostReachesAValidURL(t *testing.T) {
	for _, host := range []string{"::1", "[::1]"} {
		var cfg afclient.DaemonConfig
		cmd := newHostObservationsCmd(func(c afclient.DaemonConfig) hostObservationsClient {
			cfg = c
			return &fakeObservationsClient{obs: sampleHostObservations()}
		})
		cmd.SetOut(&bytes.Buffer{})
		cmd.SetArgs([]string{"--host", host, "--port", "60874"})
		if err := cmd.Execute(); err != nil {
			t.Fatalf("--host %s: %v", host, err)
		}
		if got := cfg.BaseURL(); got != "http://[::1]:60874" {
			t.Fatalf("--host %s: base URL %q, want http://[::1]:60874", host, got)
		}
	}
}
