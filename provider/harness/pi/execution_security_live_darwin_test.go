//go:build darwin

package pi

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/RenseiAI/donmai/agent"
)

func TestExecutionSecurityConfinedPiReportsAndRefusesOutsideWrites(t *testing.T) {
	for _, prepared := range []bool{false, true} {
		for _, mode := range []string{"spawn", "resume", "interactive"} {
			name := mode
			if prepared {
				name += "/prepared"
			}
			t.Run(name, func(t *testing.T) {
				w := newLiveWorld(t)
				p := liveProvider(t, writeLiveHarness(t, w), true)
				ctx := liveCtx(t)
				spec := w.spec()
				spec.ExecutionSecurity = confinedSecurityStamp()
				if mode == "interactive" {
					spec.Interactive = &agent.InteractiveSpec{Cols: 80, Rows: 24}
				}
				if prepared {
					digest := strings.Repeat("a", 64)
					plan, err := agent.CompilePreparedHarness(spec, p.Manifest(), digest, nil, securityMaterializations(digest))
					if err != nil {
						t.Fatalf("compile confined requirement: %v", err)
					}
					spec.PreparedHarness = plan
				}
				var h agent.Handle
				var err error
				if mode == "resume" {
					h, err = p.Resume(ctx, "ses_live", spec)
				} else {
					h, err = p.Spawn(ctx, spec)
				}
				if err != nil {
					t.Fatalf("confined %s: %v", mode, err)
				}
				if mode == "interactive" {
					finishInteractive(t, h)
				} else {
					finishHeadless(t, h)
				}
				assertConfined(t, w)
				report, err := agent.RenderExecutionSecurity(spec, p.Manifest())
				if err != nil {
					t.Fatal(err)
				}
				confiner, err := p.confinerForSession(ctx, spec)
				if err != nil {
					t.Fatal(err)
				}
				attestation, ok := confiner.Attestation()
				if !ok {
					t.Fatal("confined requirement has no attestation")
				}
				for _, dimension := range []agent.ExecutionSecurityDimension{agent.ExecutionSecurityFileWrite, agent.ExecutionSecurityIsolation} {
					entry := report.Dimension(dimension)
					if entry.AchievedLevel != spec.ExecutionSecurity.Levels.Level(dimension) || !reflect.DeepEqual(entry.EnforcingLayers, []agent.EnforcingLayer{agent.LayerExecutorOSSandbox}) || entry.EvidenceDigest != attestation.SelfTestDigest || entry.EvidenceDigest == "" {
						t.Fatalf("%s report = %+v, want current OS confinement attestation", dimension, entry)
					}
				}
				if err := agent.ExecutionSecurityReportMeets(&report, spec.ExecutionSecurity.Levels); err != nil {
					t.Fatal(err)
				}
			})
		}
	}
}

func TestExecutionSecurityUnconfinedPiCannotReuseHostAttestation(t *testing.T) {
	w := newLiveWorld(t)
	bin := writeLiveHarness(t, w)
	confined := liveProvider(t, bin, true)
	if len(confined.Manifest().ExecutionSecurity.Levels) != 2 {
		t.Fatal("control: confined host did not attest its write and isolation levels")
	}
	for _, processless := range []bool{false, true} {
		p := liveProvider(t, bin, processless)
		p.opts.skipProcess = processless
		spec := w.spec()
		spec.ExecutionSecurity = confinedSecurityStamp()
		h, err := p.Spawn(liveCtx(t), spec)
		if h != nil {
			t.Cleanup(func() { _ = h.Stop(context.Background()) })
		}
		if agent.ExecutionSecurityErrorCode(err) != agent.ExecutionSecurityUnrenderable {
			t.Fatalf("unconfined/processless Spawn reused another host's attestation: %v", err)
		}
	}
	// Without a security requirement the same unconfined entry point runs,
	// and the outside writes succeed: the OS boundary made the difference.
	h, err := liveProvider(t, bin, false).Spawn(liveCtx(t), w.spec())
	if err != nil {
		t.Fatal(err)
	}
	finishHeadless(t, h)
	assertUnconfined(t, w)
}

func TestExecutionSecurityConfinedPiRejectsReceiptEvidenceDrift(t *testing.T) {
	w := newLiveWorld(t)
	p := liveProvider(t, writeLiveHarness(t, w), true)
	spec := w.spec()
	spec.ExecutionSecurity = confinedSecurityStamp()
	digest := strings.Repeat("a", 64)
	plan, err := agent.CompilePreparedHarness(spec, p.Manifest(), digest, nil, securityMaterializations(digest))
	if err != nil {
		t.Fatal(err)
	}
	plan.ExecutionSecurity.FileWrite.EvidenceDigest = "sha256:changed-evidence"
	spec.PreparedHarness = plan
	if _, err := p.Spawn(liveCtx(t), spec); !errors.Is(err, agent.ErrSpawnFailed) || agent.ExecutionSecurityErrorCode(err) != agent.ExecutionSecurityReceiptUnmet {
		t.Fatalf("Spawn with changed receipt evidence = %v, want receipt unmet", err)
	}
}
