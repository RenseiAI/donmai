package codex

import (
	"context"
	"io"
	"slices"
	"testing"
	"time"
)

func scriptedAccess(t *testing.T, steps []quotaProbeModelStep, d, hs, rpc time.Duration) (*Provider, *quotaProbeFakeServer, func()) {
	t.Helper()
	sr, sw := io.Pipe()
	rr, rw := io.Pipe()
	fs := &quotaProbeFakeServer{stdin: sr, stdout: rw, initDelay: d, modelSteps: steps}
	serveQuotaProbeFake(t, fs, false)
	p, err := New(Options{skipProcess: true, stdinOverride: sw, stdoutOverride: rr, configTempDir: t.TempDir(), HandshakeTimeout: hs, RPCTimeout: rpc})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return p, fs, func() {
		c, x := context.WithTimeout(context.Background(), 5*time.Second)
		defer x()
		_ = p.Shutdown(c)
		_ = sr.Close()
		_ = rw.Close()
	}
}

func accRow(m string, c []string, a string) map[string]any {
	r := map[string]any{"id": "x-" + m, "model": m}
	ap := map[string]any{}
	if c != nil {
		l := make([]any, len(c))
		for i, v := range c {
			l[i] = v
		}
		ap["cyber"] = l
	}
	if a != "" {
		ap["daybreakAvailability"] = a
	}
	r["availableAccessPrograms"] = ap
	return r
}

func findGroup(gs []AccessProgramGroup, s AccessProgramState, tr string) *AccessProgramGroup {
	for i := range gs {
		if gs[i].State == s && gs[i].Tier == tr {
			return &gs[i]
		}
	}
	return nil
}

func TestMappingsSingleScan(t *testing.T) {
	t.Parallel()
	mk := accRow
	p1 := []any{mk("gpt-5.6-sol", []string{"standard"}, "enrollmentRequired"), mk("gpt-6-sol", []string{"daybreakBlue"}, "unavailableForModel"), map[string]any{"id": "x", "model": "gpt-6-astra"}, mk("gpt-5.6-terra", []string{"daybreakGreen"}, "available"), mk("private-alias", []string{"daybreakBlue"}, "available"), mk("gpt-6-luna", []string{"daybreakBlue"}, "available"), mk("gpt-5.6-luna", []string{"daybreakBlue"}, "available"), mk("gpt-5.6-luna", []string{"daybreakBlue"}, "available"), mk("gpt-daybreak-blue-latest", []string{"daybreakBlue", "daybreakRed"}, "available"), mk("gpt-6.2-sol", []string{"standard"}, "available"), mk("gpt-5.5", []string{"standard"}, "default")}
	p2 := []any{mk("gpt-6-luna", []string{"daybreakRed"}, "available"), mk("gpt-5.6-terra", []string{"daybreakBlue"}, "default"), mk("gpt-6.1-sol", []string{"daybreakBlue"}, "available")}
	s := []quotaProbeModelStep{{result: map[string]any{"data": p1, "nextCursor": "c"}}, {result: map[string]any{"data": p2}}}
	p, fs, d := scriptedAccess(t, s, 0, 5*time.Second, 5*time.Second)
	defer d()
	g, e := p.ProbeAccessPrograms(context.Background())
	if e != nil {
		t.Fatal(e)
	}
	if ne := findGroup(g, AccessProgramNotEnabled, ""); ne == nil || !slices.Equal(ne.Models, []string{"gpt-5.6-sol"}) {
		t.Errorf("notEnabled %v", g)
	}
	if un := findGroup(g, AccessProgramUnknown, ""); un == nil || !slices.Equal(un.Models, []string{"gpt-5.6-terra", "gpt-6-luna", "gpt-6-sol", "gpt-6.2-sol"}) {
		t.Errorf("unknown %v", g)
	}
	if b := findGroup(g, AccessProgramEnabled, "blue"); b == nil || !slices.Equal(b.Models, []string{"gpt-5.6-luna", "gpt-6.1-sol", "gpt-daybreak-blue-latest"}) {
		t.Errorf("blue %v", g)
	}
	if r := findGroup(g, AccessProgramEnabled, "red"); r == nil || !slices.Equal(r.Models, []string{"gpt-daybreak-blue-latest"}) {
		t.Errorf("red %v", g)
	}
	if len(g) != 4 {
		t.Errorf("groups %v want 4", g)
	}
	for _, x := range g {
		for _, m := range x.Models {
			if m == "private-alias" || m == "daybreakGreen" {
				t.Fatalf("escaped %q", m)
			}
		}
	}
	fs.modelMu.Lock()
	defer fs.modelMu.Unlock()
	if len(fs.modelReqs) != 2 || fs.modelReqs[1]["cursor"] != "c" || fs.modelReqs[0]["includeHidden"] != true {
		t.Fatalf("transcript %v", fs.modelReqs)
	}
}

func TestConflictLoopPartialIsolation(t *testing.T) {
	t.Parallel()
	mk := func(m string) []any { return []any{accRow(m, []string{"daybreakBlue"}, "available")} }
	t.Run("deadlines", func(t *testing.T) {
		t.Parallel()
		for _, tc := range []struct {
			n       string
			s       []quotaProbeModelStep
			d       time.Duration
			hs, rpc time.Duration
			ctx     time.Duration
		}{
			{"init", nil, 500 * time.Millisecond, 5 * time.Second, 5 * time.Second, 100 * time.Millisecond},
			{"pageCaller", []quotaProbeModelStep{{delay: 500 * time.Millisecond, result: map[string]any{"data": mk("gpt-5.6-sol")}}}, 0, 5 * time.Second, 5 * time.Second, 100 * time.Millisecond},
			{"pageRPC", []quotaProbeModelStep{{delay: 500 * time.Millisecond, result: map[string]any{"data": mk("gpt-5.6-sol")}}}, 0, 5 * time.Second, 80 * time.Millisecond, 10 * time.Second},
			{"loop", []quotaProbeModelStep{{result: map[string]any{"data": mk("gpt-5.6-sol"), "nextCursor": "l"}}, {result: map[string]any{"data": mk("gpt-6-sol"), "nextCursor": "l"}}}, 0, 5 * time.Second, 5 * time.Second, 0},
			{"rpcErr", []quotaProbeModelStep{{result: map[string]any{"data": mk("gpt-5.6-sol"), "nextCursor": "n"}}, {rpcErr: map[string]any{"code": -32603, "message": "x"}}}, 0, 5 * time.Second, 5 * time.Second, 0},
			{"malformed", []quotaProbeModelStep{{result: map[string]any{"data": mk("gpt-5.6-sol"), "nextCursor": "n"}}, {result: map[string]any{"oops": true}}}, 0, 5 * time.Second, 5 * time.Second, 0},
		} {
			t.Run(tc.n, func(t *testing.T) {
				t.Parallel()
				p, _, d := scriptedAccess(t, tc.s, tc.d, tc.hs, tc.rpc)
				defer d()
				ctx := context.Background()
				if tc.ctx > 0 {
					var c context.CancelFunc
					ctx, c = context.WithTimeout(ctx, tc.ctx)
					defer c()
				}
				if g, e := p.ProbeAccessPrograms(ctx); e == nil || len(g) != 0 {
					t.Errorf("%s %v %v", tc.n, g, e)
				}
			})
		}
	})
	t.Run("isolation", func(t *testing.T) {
		t.Parallel()
		p1, _, d1 := scriptedAccess(t, []quotaProbeModelStep{{result: map[string]any{"data": mk("gpt-5.6-sol")}}}, 0, 5*time.Second, 5*time.Second)
		defer d1()
		if _, e := p1.ProbeAccessPrograms(context.Background()); e != nil {
			t.Fatal(e)
		}
		p2, _, d2 := scriptedAccess(t, []quotaProbeModelStep{{result: map[string]any{"data": mk("gpt-6-sol")}}}, 0, 5*time.Second, 5*time.Second)
		defer d2()
		g2, e := p2.ProbeAccessPrograms(context.Background())
		if e != nil {
			t.Fatal(e)
		}
		if b := findGroup(g2, AccessProgramEnabled, "blue"); b == nil || !slices.Equal(b.Models, []string{"gpt-6-sol"}) {
			t.Errorf("isolation %v", g2)
		}
	})
}
