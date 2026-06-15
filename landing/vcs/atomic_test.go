package vcs

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func availableAtomic(r commandRunner) *AtomicProvider {
	p := NewAtomicProvider()
	p.runner = r
	p.now = fixedNow()
	return p
}

// ── capability profile ──────────────────────────────────────────────────────

func TestAtomicCapabilityProfile(t *testing.T) {
	tests := []struct {
		name string
		got  any
		want any
	}{
		{"MergeModel", AtomicCapabilities.MergeModel, "patch-theory"},
		{"ConflictGranularity", AtomicCapabilities.ConflictGranularity, "token"},
		{"PatchModel", AtomicCapabilities.PatchModel, "patch-theoretic"},
		{"HasPullRequests", AtomicCapabilities.HasPullRequests, false},
		{"HasReviewWorkflow", AtomicCapabilities.HasReviewWorkflow, false},
		{"HasMergeQueue", AtomicCapabilities.HasMergeQueue, false},
		{"SupportsBranches", AtomicCapabilities.SupportsBranches, true},
		{"SupportsRebase", AtomicCapabilities.SupportsRebase, false},
		{"ProvenanceNative", AtomicCapabilities.ProvenanceNative, true},
		{"SupportsAttest", AtomicCapabilities.SupportsAttest, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.got != tt.want {
				t.Errorf("AtomicCapabilities.%s = %v, want %v", tt.name, tt.got, tt.want)
			}
		})
	}
	if NewAtomicProvider().Name() != "atomic" {
		t.Errorf("Name() != atomic")
	}
}

// ── commutative: optional verbs are unsupported ─────────────────────────────

func TestAtomicUnsupportedVerbs(t *testing.T) {
	ctx := context.Background()
	p := availableAtomic(&fakeRunner{})
	ws := testWorkspace()
	ref := ProposalRef{ID: "x", State: "open"}

	t.Run("OpenProposal", func(t *testing.T) {
		_, err := p.OpenProposal(ctx, ws, ProposalOpts{BaseRef: "main"})
		assertUnsupported(t, err, "HasPullRequests")
	})
	t.Run("MergeProposal", func(t *testing.T) {
		_, err := p.MergeProposal(ctx, ref, "auto")
		assertUnsupported(t, err, "HasPullRequests")
	})
	t.Run("EnqueueForMerge", func(t *testing.T) {
		_, err := p.EnqueueForMerge(ctx, ref, QueueOpts{})
		assertUnsupported(t, err, "HasMergeQueue")
	})
}

func assertUnsupported(t *testing.T, err error, wantCap string) {
	t.Helper()
	var ue *UnsupportedOperationError
	if !errors.As(err, &ue) {
		t.Fatalf("err = %v, want *UnsupportedOperationError", err)
	}
	if ue.Capability != wantCap {
		t.Errorf("Capability = %q, want %q", ue.Capability, wantCap)
	}
}

// ── required verbs ──────────────────────────────────────────────────────────

func TestAtomicClone(t *testing.T) {
	r := newSeqRunner(
		seqReply{},                        // atomic clone
		seqReply{stdout: "patchset-aa\n"}, // atomic log (resolveHead)
	)
	p := availableAtomic(r)
	ws, err := p.Clone(context.Background(), "atomic://repo", "/tmp/dst", CloneOpts{Ref: "view-1"})
	if err != nil {
		t.Fatalf("Clone: %v", err)
	}
	if ws.HeadRef != "patchset-aa" {
		t.Errorf("HeadRef = %q, want patchset-aa", ws.HeadRef)
	}
	if ws.ProviderID != "atomic" {
		t.Errorf("ProviderID = %q", ws.ProviderID)
	}
	if !strings.Contains(r.commandLines()[0], "--branch view-1") {
		t.Errorf("clone missing branch flag: %q", r.commandLines()[0])
	}
}

func TestAtomicRecordChange(t *testing.T) {
	r := newSeqRunner(
		seqReply{},                        // atomic add
		seqReply{},                        // atomic record
		seqReply{stdout: "patchset-bb\n"}, // resolveHead
	)
	p := availableAtomic(r)
	ref, err := p.RecordChange(context.Background(), testWorkspace(), ChangeRequest{
		Message: "record this",
		Paths:   []string{"x.txt"},
	})
	if err != nil {
		t.Fatalf("RecordChange: %v", err)
	}
	if ref.Ref != "patchset-bb" {
		t.Errorf("Ref = %q, want patchset-bb", ref.Ref)
	}
	if ref.Summary != "record this" {
		t.Errorf("Summary = %q", ref.Summary)
	}
}

func TestAtomicRecordChangeSignedAuthor(t *testing.T) {
	r := newSeqRunner(seqReply{}, seqReply{stdout: "ps\n"}) // no paths → no add; record + resolveHead
	p := availableAtomic(r)
	att := testAttestation()
	att.SignedBy = "did:key:zAtomic"
	_, err := p.RecordChange(context.Background(), testWorkspace(), ChangeRequest{
		Message:     "m",
		Attestation: &att,
	})
	if err != nil {
		t.Fatalf("RecordChange: %v", err)
	}
	joined := strings.Join(r.commandLines(), "\n")
	if !strings.Contains(joined, "--author did:key:zAtomic") {
		t.Errorf("signing identity not passed natively:\n%s", joined)
	}
}

func TestAtomicPush(t *testing.T) {
	t.Run("success", func(t *testing.T) {
		r := newSeqRunner(seqReply{}, seqReply{stdout: "ps-head\n"})
		p := availableAtomic(r)
		res, err := p.Push(context.Background(), testWorkspace(), PushTarget{Remote: "origin", Ref: "main"})
		if err != nil {
			t.Fatalf("Push: %v", err)
		}
		if res.Kind != PushPushed || res.Ref.Ref != "ps-head" {
			t.Errorf("res = %+v, want pushed ps-head", res)
		}
	})

	t.Run("auth rejection", func(t *testing.T) {
		r := &fakeRunner{reply: errOnMatch("push", "permission denied", "")}
		p := availableAtomic(r)
		res, _ := p.Push(context.Background(), testWorkspace(), PushTarget{Remote: "origin", Ref: "main"})
		if res.Kind != PushRejected || res.Reason != "auth" {
			t.Errorf("res = %+v, want rejected auth", res)
		}
	})
}

func TestAtomicPull(t *testing.T) {
	tests := []struct {
		name     string
		stdout   string
		pullErr  string
		wantKind MergeResultKind
		wantLen  int
	}{
		{"clean", "Already up to date", "", MergeClean, 0},
		{"auto-resolved with files", "Auto-resolved 2 patches\nAuto-resolved src/foo.ts (patch-theory)", "", MergeAutoResolved, 1},
		{"auto-resolved count only", "Auto-resolved 3 patches", "", MergeAutoResolved, 1},
		{"conflict in stdout", "Conflict in src/bar.ts: structural", "", MergeConflict, 1},
		{"conflict via error", "", "unresolvable conflict in src/baz.ts", MergeConflict, 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var r *fakeRunner
			if tt.pullErr != "" {
				r = &fakeRunner{reply: errOnMatch("pull", tt.pullErr, "")}
			} else {
				r = &fakeRunner{reply: func(string, []string) (string, error) { return tt.stdout, nil }}
			}
			p := availableAtomic(r)
			res, err := p.Pull(context.Background(), testWorkspace(), PullSource{Remote: "origin", Ref: "main"})
			if err != nil {
				t.Fatalf("Pull: %v", err)
			}
			if res.Kind != tt.wantKind {
				t.Fatalf("Kind = %q, want %q", res.Kind, tt.wantKind)
			}
			switch tt.wantKind {
			case MergeAutoResolved:
				if len(res.Resolutions) < tt.wantLen {
					t.Errorf("Resolutions len = %d, want >= %d", len(res.Resolutions), tt.wantLen)
				}
			case MergeConflict:
				if len(res.Conflicts) < tt.wantLen {
					t.Errorf("Conflicts len = %d, want >= %d", len(res.Conflicts), tt.wantLen)
				}
			}
		})
	}

	t.Run("unexpected error is returned", func(t *testing.T) {
		r := &fakeRunner{reply: errOnMatch("pull", "network down", "")}
		p := availableAtomic(r)
		_, err := p.Pull(context.Background(), testWorkspace(), PullSource{Remote: "origin", Ref: "main"})
		if err == nil || !strings.Contains(err.Error(), "network down") {
			t.Errorf("err = %v, want network down wrapped", err)
		}
	})
}

func TestAtomicAttest(t *testing.T) {
	r := newSeqRunner(
		seqReply{},                        // atomic record --signed
		seqReply{stdout: "attested-ps\n"}, // resolveHead
	)
	p := availableAtomic(r)
	ref, err := p.Attest(context.Background(), testWorkspace(), testAttestation())
	if err != nil {
		t.Fatalf("Attest: %v", err)
	}
	if ref.StorageKind != "native" {
		t.Errorf("StorageKind = %q, want native", ref.StorageKind)
	}
	if ref.ID != "attested-ps" {
		t.Errorf("ID = %q, want attested-ps", ref.ID)
	}
	joined := strings.Join(r.commandLines(), "\n")
	if !strings.Contains(joined, "--signed") || !strings.Contains(joined, "--allow-empty") {
		t.Errorf("attest missing native signing flags:\n%s", joined)
	}
}

// ── parsing helpers ─────────────────────────────────────────────────────────

func TestParseAtomicPullOutput(t *testing.T) {
	tests := []struct {
		name     string
		in       string
		wantKind MergeResultKind
	}{
		{"clean nothing to pull", "Nothing to pull", MergeClean},
		{"clean already up to date", "Already up to date", MergeClean},
		{"auto-resolved", "Auto-resolved 1 patch in src/a.ts", MergeAutoResolved},
		{"conflict", "Conflict in src/a.ts", MergeConflict},
		{"unresolvable", "unresolvable structural divergence", MergeConflict},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := parseAtomicPullOutput(tt.in).Kind; got != tt.wantKind {
				t.Errorf("kind = %q, want %q", got, tt.wantKind)
			}
		})
	}
}

func TestParseAutoResolutions(t *testing.T) {
	t.Run("per-file entries", func(t *testing.T) {
		out := "Auto-resolved 2 patches\nAuto-resolved src/foo.ts (patch-theory)\nAuto-resolved src/bar.ts (three-way)"
		res := parseAutoResolutions(out)
		if len(res) != 2 {
			t.Fatalf("len = %d, want 2", len(res))
		}
		if res[0].FilePath != "src/foo.ts" || res[0].Strategy != "patch-theory" {
			t.Errorf("res[0] = %+v", res[0])
		}
		if res[1].Strategy != "three-way" {
			t.Errorf("res[1].Strategy = %q", res[1].Strategy)
		}
	})

	t.Run("count marker without per-file synthesizes summary", func(t *testing.T) {
		res := parseAutoResolutions("Auto-resolved 5 patches")
		if len(res) != 1 {
			t.Fatalf("len = %d, want 1 synthesized", len(res))
		}
		if !strings.Contains(res[0].FilePath, "multiple files") {
			t.Errorf("FilePath = %q", res[0].FilePath)
		}
	})

	t.Run("no marker yields none", func(t *testing.T) {
		if res := parseAutoResolutions("nothing here"); len(res) != 0 {
			t.Errorf("len = %d, want 0", len(res))
		}
	})
}

func TestParseAtomicConflicts(t *testing.T) {
	t.Run("with detail", func(t *testing.T) {
		c := parseAtomicConflicts("Conflict in src/x.ts: token overlap")
		if len(c) != 1 || c[0].FilePath != "src/x.ts" || c[0].Detail != "token overlap" {
			t.Errorf("conflicts = %+v", c)
		}
	})

	t.Run("fallback unknown", func(t *testing.T) {
		c := parseAtomicConflicts("something unexpected went wrong")
		if len(c) != 1 || !strings.Contains(c[0].FilePath, "unknown") {
			t.Errorf("conflicts = %+v, want unknown fallback", c)
		}
	})

	t.Run("detail truncated to 200", func(t *testing.T) {
		long := strings.Repeat("z", 500)
		c := parseAtomicConflicts(long)
		if len(c[0].Detail) != 200 {
			t.Errorf("detail len = %d, want 200", len(c[0].Detail))
		}
	})
}

func TestClassifyAtomicPushError(t *testing.T) {
	tests := map[string]string{
		"authentication required": "auth",
		"permission denied":       "auth",
		"non-fast-forward update": "non-fast-forward",
		"rejected by remote hook": "policy",
		"some other failure":      "policy",
	}
	for in, want := range tests {
		if got := classifyAtomicPushError(in); got != want {
			t.Errorf("classifyAtomicPushError(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestBuildAtomicAttestationMessage(t *testing.T) {
	att := testAttestation()
	att.WorkareaSnapshotRef = &WorkareaSnapshotRef{Ref: "snap-1"}
	att.SignedBy = "did:key:zA"
	att.ReviewerHints = []string{"alice", "bob"}
	msg := buildAtomicAttestationMessage(att)

	for _, want := range []string{
		"Donmai-Agent-Id: agent-001",
		"Donmai-Session-Id: sess-xyz",
		"Donmai-Model: anthropic/opus",
		"Donmai-Kit-Set: spring/java@1.0.0",
		"Donmai-Workarea-Snapshot: snap-1",
		"Donmai-Signed-By: did:key:zA",
		"Donmai-Reviewer-Hints: alice,bob",
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("message missing %q:\n%s", want, msg)
		}
	}

	// OSS hygiene: the legacy private brand must not appear in committed
	// attestation text. Brand literal assembled at runtime (see github_test.go).
	brand := "Ren" + "sei"
	if strings.Contains(msg, brand) || strings.Contains(msg, strings.ToLower(brand)) {
		t.Errorf("attestation message leaks private brand:\n%s", msg)
	}
}
