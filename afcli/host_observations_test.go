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
