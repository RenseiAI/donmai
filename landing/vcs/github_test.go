package vcs

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

// fixedNow returns a deterministic clock for timestamp assertions.
func fixedNow() func() time.Time {
	t := time.Date(2026, 4, 27, 0, 0, 0, 0, time.UTC)
	return func() time.Time { return t }
}

func testWorkspace() Workspace {
	return Workspace{
		ID:         "github:github.com/org/repo",
		ProviderID: "github",
		Path:       "/tmp/test-workspace",
		HeadRef:    "abc123",
	}
}

func testAttestation() SessionAttestation {
	return SessionAttestation{
		AgentID:   "agent-001",
		Model:     ModelRef{Provider: "anthropic", Model: "opus"},
		SessionID: "sess-xyz",
		KitIDs:    []KitProviderID{{ID: "spring/java", Version: "1.0.0"}},
		StartedAt: "2026-04-27T00:00:00Z",
	}
}

// availableGitHub returns a provider opted-in (available) with a fake runner and
// fixed clock.
func availableGitHub(r commandRunner) *GitHubProvider {
	p := NewGitHubProvider(GitHubOpts{Available: true})
	p.runner = r
	p.now = fixedNow()
	return p
}

// ── capability profile ──────────────────────────────────────────────────────

func TestGitHubCapabilityProfile(t *testing.T) {
	tests := []struct {
		name string
		got  any
		want any
	}{
		{"MergeModel", GitHubCapabilities.MergeModel, "three-way-text"},
		{"ConflictGranularity", GitHubCapabilities.ConflictGranularity, "line"},
		{"PatchModel", GitHubCapabilities.PatchModel, "commit-graph"},
		{"HasPullRequests", GitHubCapabilities.HasPullRequests, true},
		{"HasReviewWorkflow", GitHubCapabilities.HasReviewWorkflow, true},
		{"HasMergeQueue", GitHubCapabilities.HasMergeQueue, true},
		{"SupportsBranches", GitHubCapabilities.SupportsBranches, true},
		{"SupportsRebase", GitHubCapabilities.SupportsRebase, true},
		{"ProvenanceNative", GitHubCapabilities.ProvenanceNative, false},
		{"SupportsAttest", GitHubCapabilities.SupportsAttest, true},
		{"SupportsBinary", GitHubCapabilities.SupportsBinary, true},
		{"SupportsStructuredContent", GitHubCapabilities.SupportsStructuredContent, false},
		{"SupportsLargeFiles", GitHubCapabilities.SupportsLargeFiles, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.got != tt.want {
				t.Errorf("GitHubCapabilities.%s = %v, want %v", tt.name, tt.got, tt.want)
			}
		})
	}
}

// ── constructor ─────────────────────────────────────────────────────────────

func TestNewGitHubProvider(t *testing.T) {
	t.Run("defaults to hasMergeQueue=true", func(t *testing.T) {
		p := NewGitHubProvider(GitHubOpts{})
		if !p.Capabilities().HasMergeQueue {
			t.Errorf("HasMergeQueue = false, want true")
		}
	})

	t.Run("respects HasMergeQueueOverride=false", func(t *testing.T) {
		off := false
		p := NewGitHubProvider(GitHubOpts{HasMergeQueueOverride: &off})
		if p.Capabilities().HasMergeQueue {
			t.Errorf("HasMergeQueue = true, want false")
		}
	})

	t.Run("respects HasMergeQueueOverride=true", func(t *testing.T) {
		on := true
		p := NewGitHubProvider(GitHubOpts{HasMergeQueueOverride: &on})
		if !p.Capabilities().HasMergeQueue {
			t.Errorf("HasMergeQueue = false, want true")
		}
	})

	t.Run("name is github", func(t *testing.T) {
		if got := NewGitHubProvider(GitHubOpts{}).Name(); got != "github" {
			t.Errorf("Name() = %q, want github", got)
		}
	})
}

// ── default-off capability gate (donmai #151 pattern) ───────────────────────

func TestGitHubGateDefaultOff(t *testing.T) {
	// A provider built WITHOUT Available must refuse every verb with
	// ErrGitHubUnavailable and never touch the runner.
	r := &fakeRunner{}
	p := NewGitHubProvider(GitHubOpts{}) // Available defaults to false
	p.runner = r
	ctx := context.Background()
	ws := testWorkspace()
	ref := ProposalRef{ID: "42", State: "open"}

	type call struct {
		name string
		fn   func() error
	}
	calls := []call{
		{"Clone", func() error { _, e := p.Clone(ctx, "u", "d", CloneOpts{}); return e }},
		{"RecordChange", func() error { _, e := p.RecordChange(ctx, ws, ChangeRequest{Message: "m"}); return e }},
		{"Push", func() error { _, e := p.Push(ctx, ws, PushTarget{Remote: "origin", Ref: "main"}); return e }},
		{"Pull", func() error { _, e := p.Pull(ctx, ws, PullSource{Remote: "origin", Ref: "main"}); return e }},
		{"OpenProposal", func() error { _, e := p.OpenProposal(ctx, ws, ProposalOpts{BaseRef: "main"}); return e }},
		{"MergeProposal", func() error { _, e := p.MergeProposal(ctx, ref, "auto"); return e }},
		{"EnqueueForMerge", func() error { _, e := p.EnqueueForMerge(ctx, ref, QueueOpts{}); return e }},
		{"Attest", func() error { _, e := p.Attest(ctx, ws, testAttestation()); return e }},
	}

	for _, c := range calls {
		t.Run(c.name, func(t *testing.T) {
			err := c.fn()
			if !errors.Is(err, ErrGitHubUnavailable) {
				t.Fatalf("%s err = %v, want ErrGitHubUnavailable", c.name, err)
			}
		})
	}

	if len(r.commandLines()) != 0 {
		t.Errorf("runner was invoked %d times while gated; want 0: %v", len(r.commandLines()), r.commandLines())
	}
}

func TestGitHubGateAvailableRuns(t *testing.T) {
	// When Available=true the gate passes and the runner is used.
	r := newSeqRunner(seqReply{stdout: "abc123\n"}) // git rev-parse HEAD for clone
	p := availableGitHub(r)
	_, err := p.Clone(context.Background(), "git@github.com:org/repo.git", "/tmp/dst", CloneOpts{})
	if err != nil {
		t.Fatalf("Clone with Available=true: %v", err)
	}
	if len(r.commandLines()) == 0 {
		t.Error("runner not invoked despite Available=true")
	}
}

// ── trailer builders ────────────────────────────────────────────────────────

func TestBuildAttestationTrailers(t *testing.T) {
	tests := []struct {
		name     string
		att      SessionAttestation
		contains []string
		absent   []string
	}{
		{
			name:     "base trailers",
			att:      testAttestation(),
			contains: []string{"Co-Authored-By: agent-001 <agent@donmai.dev>", "X-Donmai-Session-Id: sess-xyz", "X-Donmai-Model: anthropic/opus", "X-Donmai-Kit-Set: spring/java@1.0.0"},
		},
		{
			name: "multiple kits",
			att: func() SessionAttestation {
				a := testAttestation()
				a.KitIDs = []KitProviderID{{ID: "spring/java", Version: "1.0.0"}, {ID: "docker-compose", Version: "2.1.0"}}
				return a
			}(),
			contains: []string{"X-Donmai-Kit-Set: spring/java@1.0.0,docker-compose@2.1.0"},
		},
		{
			name: "empty kits omits kit-set",
			att: func() SessionAttestation {
				a := testAttestation()
				a.KitIDs = nil
				return a
			}(),
			absent: []string{"X-Donmai-Kit-Set"},
		},
		{
			name: "workarea snapshot present",
			att: func() SessionAttestation {
				a := testAttestation()
				a.WorkareaSnapshotRef = &WorkareaSnapshotRef{Ref: "snap-abc", ProviderID: "wa-1"}
				return a
			}(),
			contains: []string{"X-Donmai-Workarea-Snapshot: snap-abc"},
		},
		{
			name:   "workarea snapshot absent",
			att:    testAttestation(),
			absent: []string{"X-Donmai-Workarea-Snapshot"},
		},
		{
			name: "signed-by present",
			att: func() SessionAttestation {
				a := testAttestation()
				a.SignedBy = "did:key:z6Mk"
				return a
			}(),
			contains: []string{"X-Donmai-Signed-By: did:key:z6Mk"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			out := BuildAttestationTrailers(tt.att)
			for _, c := range tt.contains {
				if !strings.Contains(out, c) {
					t.Errorf("trailers missing %q in:\n%s", c, out)
				}
			}
			for _, a := range tt.absent {
				if strings.Contains(out, a) {
					t.Errorf("trailers unexpectedly contain %q in:\n%s", a, out)
				}
			}
		})
	}
}

func TestBuildAttestationTrailersHasNoPrivateBrand(t *testing.T) {
	// OSS hygiene: the legacy private brand word must not leak into committed
	// trailer text. The brand literal is assembled at runtime so the source of
	// this OSS-published file never carries it.
	brand := "Ren" + "sei"
	a := testAttestation()
	a.SignedBy = "did:key:z6Mk"
	a.WorkareaSnapshotRef = &WorkareaSnapshotRef{Ref: "snap"}
	out := BuildAttestationTrailers(a)
	if strings.Contains(out, brand) || strings.Contains(out, strings.ToLower(brand)) {
		t.Errorf("trailer text leaks private brand:\n%s", out)
	}
}

func TestBuildCommitMessageWithTrailers(t *testing.T) {
	t.Run("appends after blank line", func(t *testing.T) {
		msg := BuildCommitMessageWithTrailers("fix: my commit", testAttestation())
		if !strings.Contains(msg, "\n\nCo-Authored-By:") {
			t.Errorf("expected blank-line separator before trailers:\n%s", msg)
		}
	})
	t.Run("preserves original subject", func(t *testing.T) {
		msg := BuildCommitMessageWithTrailers("chore: update deps", testAttestation())
		if !strings.HasPrefix(msg, "chore: update deps") {
			t.Errorf("subject not preserved:\n%s", msg)
		}
	})
	t.Run("single newline suffix uses single separator", func(t *testing.T) {
		msg := BuildCommitMessageWithTrailers("subject\n", testAttestation())
		if strings.Contains(msg, "subject\n\n\n") {
			t.Errorf("double-padded separator:\n%q", msg)
		}
	})
}

// ── push classification ─────────────────────────────────────────────────────

func TestGitHubPush(t *testing.T) {
	tests := []struct {
		name       string
		pushErr    string
		wantKind   PushResultKind
		wantReason string
		wantRef    string
	}{
		{"success", "", PushPushed, "", "def456"},
		{"non-fast-forward", "! [rejected] main -> main (non-fast-forward)", PushRejected, "non-fast-forward", ""},
		{"auth", "Permission denied (publickey)", PushRejected, "auth", ""},
		{"policy", "remote: Push rejected by repository policy", PushRejected, "policy", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var r *fakeRunner
			if tt.pushErr == "" {
				r = newSeqRunner(seqReply{stdout: "def456\n"}, seqReply{})
			} else {
				r = &fakeRunner{reply: errOnMatch("push", tt.pushErr, "def456\n")}
			}
			p := availableGitHub(r)
			res, err := p.Push(context.Background(), testWorkspace(), PushTarget{Remote: "origin", Ref: "main"})
			if err != nil {
				t.Fatalf("Push returned error: %v", err)
			}
			if res.Kind != tt.wantKind {
				t.Errorf("Kind = %q, want %q", res.Kind, tt.wantKind)
			}
			if tt.wantReason != "" && res.Reason != tt.wantReason {
				t.Errorf("Reason = %q, want %q", res.Reason, tt.wantReason)
			}
			if tt.wantRef != "" && res.Ref.Ref != tt.wantRef {
				t.Errorf("Ref.Ref = %q, want %q", res.Ref.Ref, tt.wantRef)
			}
		})
	}
}

func TestClassifyPushError(t *testing.T) {
	tests := []struct {
		in   string
		want string
	}{
		{"non-fast-forward", "non-fast-forward"},
		{"! [rejected] main", "non-fast-forward"},
		{"fatal: Authentication failed", "auth"},
		{"Permission denied (publickey)", "auth"},
		{"remote: protected branch policy", "policy"},
	}
	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			if got := classifyPushError(tt.in); got != tt.want {
				t.Errorf("classifyPushError(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

// ── pull variants ───────────────────────────────────────────────────────────

func TestGitHubPull(t *testing.T) {
	t.Run("clean", func(t *testing.T) {
		p := availableGitHub(&fakeRunner{})
		res, err := p.Pull(context.Background(), testWorkspace(), PullSource{Remote: "origin", Ref: "main"})
		if err != nil {
			t.Fatalf("Pull: %v", err)
		}
		if res.Kind != MergeClean {
			t.Errorf("Kind = %q, want clean", res.Kind)
		}
	})

	t.Run("conflict on CONFLICT marker", func(t *testing.T) {
		r := &fakeRunner{reply: func(name string, args []string) (string, error) {
			line := name + " " + strings.Join(args, " ")
			if strings.Contains(line, "pull") {
				return "", errors.New("CONFLICT (content): Merge conflict in src/index.ts")
			}
			if strings.Contains(line, "diff") {
				return "src/index.ts\n", nil
			}
			return "", nil
		}}
		p := availableGitHub(r)
		res, err := p.Pull(context.Background(), testWorkspace(), PullSource{Remote: "origin", Ref: "main"})
		if err != nil {
			t.Fatalf("Pull: %v", err)
		}
		if res.Kind != MergeConflict {
			t.Fatalf("Kind = %q, want conflict", res.Kind)
		}
		if len(res.Conflicts) == 0 || res.Conflicts[0].FilePath != "src/index.ts" {
			t.Errorf("Conflicts = %+v, want src/index.ts", res.Conflicts)
		}
	})

	t.Run("conflict on Automatic merge failed", func(t *testing.T) {
		r := &fakeRunner{reply: func(name string, args []string) (string, error) {
			line := name + " " + strings.Join(args, " ")
			if strings.Contains(line, "pull") {
				return "", errors.New("Automatic merge failed; fix conflicts")
			}
			return "src/foo.ts\n", nil
		}}
		p := availableGitHub(r)
		res, _ := p.Pull(context.Background(), testWorkspace(), PullSource{Remote: "origin", Ref: "main"})
		if res.Kind != MergeConflict {
			t.Errorf("Kind = %q, want conflict", res.Kind)
		}
	})

	t.Run("unexpected error is returned", func(t *testing.T) {
		r := &fakeRunner{reply: errOnMatch("pull", "network error: connection refused", "")}
		p := availableGitHub(r)
		_, err := p.Pull(context.Background(), testWorkspace(), PullSource{Remote: "origin", Ref: "main"})
		if err == nil || !strings.Contains(err.Error(), "network error") {
			t.Errorf("err = %v, want network error wrapped", err)
		}
	})
}

// ── record change ───────────────────────────────────────────────────────────

func TestGitHubRecordChange(t *testing.T) {
	t.Run("stages, commits, returns ref+summary", func(t *testing.T) {
		r := newSeqRunner(
			seqReply{},                      // git add
			seqReply{},                      // git commit
			seqReply{stdout: "newsha123\n"}, // rev-parse
			seqReply{stdout: "feat: thing"}, // log -1
		)
		p := availableGitHub(r)
		ref, err := p.RecordChange(context.Background(), testWorkspace(), ChangeRequest{
			Message: "feat: thing",
			Paths:   []string{"a.go", "b.go"},
		})
		if err != nil {
			t.Fatalf("RecordChange: %v", err)
		}
		if ref.Ref != "newsha123" {
			t.Errorf("Ref = %q, want newsha123", ref.Ref)
		}
		if ref.Summary != "feat: thing" {
			t.Errorf("Summary = %q", ref.Summary)
		}
		if ref.RecordedAt == "" {
			t.Error("RecordedAt empty")
		}
		lines := r.commandLines()
		if !strings.Contains(lines[0], "git add -- a.go b.go") {
			t.Errorf("first call = %q, want git add", lines[0])
		}
	})

	t.Run("attestation trailers reach commit args", func(t *testing.T) {
		r := newSeqRunner(seqReply{}, seqReply{stdout: "sha\n"}, seqReply{stdout: "subject"})
		p := availableGitHub(r)
		_, err := p.RecordChange(context.Background(), testWorkspace(), ChangeRequest{
			Message:     "feat: x",
			Attestation: ptrAtt(testAttestation()),
		})
		if err != nil {
			t.Fatalf("RecordChange: %v", err)
		}
		joined := strings.Join(r.commandLines(), "\n")
		if !strings.Contains(joined, "X-Donmai-Session-Id: sess-xyz") {
			t.Errorf("commit args missing trailer:\n%s", joined)
		}
	})
}

func ptrAtt(a SessionAttestation) *SessionAttestation { return &a }

// ── attest ──────────────────────────────────────────────────────────────────

func TestGitHubAttest(t *testing.T) {
	r := newSeqRunner(
		seqReply{},                     // git commit --allow-empty
		seqReply{stdout: "deadbeef\n"}, // rev-parse
	)
	p := availableGitHub(r)
	ref, err := p.Attest(context.Background(), testWorkspace(), testAttestation())
	if err != nil {
		t.Fatalf("Attest: %v", err)
	}
	if ref.StorageKind != "commit-trailer" {
		t.Errorf("StorageKind = %q, want commit-trailer", ref.StorageKind)
	}
	if ref.ID != "deadbeef" {
		t.Errorf("ID = %q, want deadbeef", ref.ID)
	}
	if ref.AttestedAt == "" {
		t.Error("AttestedAt empty")
	}
	joined := strings.Join(r.commandLines(), "\n")
	if !strings.Contains(joined, "--allow-empty") {
		t.Errorf("attest did not use --allow-empty:\n%s", joined)
	}
	if !strings.Contains(joined, "X-Donmai-Session-Id: sess-xyz") {
		t.Errorf("attest commit missing session trailer:\n%s", joined)
	}
}

// ── proposal verbs ──────────────────────────────────────────────────────────

func TestGitHubOpenProposal(t *testing.T) {
	t.Run("parses pr url", func(t *testing.T) {
		r := newSeqRunner(seqReply{stdout: "https://github.com/org/repo/pull/123\n"})
		p := availableGitHub(r)
		ref, err := p.OpenProposal(context.Background(), testWorkspace(), ProposalOpts{
			Title: "t", Body: "b", BaseRef: "main",
			Reviewers: []string{"alice", "bob"}, Labels: []string{"feat"},
		})
		if err != nil {
			t.Fatalf("OpenProposal: %v", err)
		}
		if ref.ID != "123" {
			t.Errorf("ID = %q, want 123", ref.ID)
		}
		if ref.State != "open" {
			t.Errorf("State = %q, want open", ref.State)
		}
		joined := strings.Join(r.commandLines(), "\n")
		if !strings.Contains(joined, "--reviewer alice,bob") {
			t.Errorf("reviewers not joined:\n%s", joined)
		}
	})

	t.Run("unparseable url errors", func(t *testing.T) {
		r := newSeqRunner(seqReply{stdout: "not a url"})
		p := availableGitHub(r)
		_, err := p.OpenProposal(context.Background(), testWorkspace(), ProposalOpts{BaseRef: "main"})
		if err == nil {
			t.Fatal("expected error on unparseable URL")
		}
	})

	t.Run("gated when HasPullRequests=false", func(t *testing.T) {
		p := availableGitHub(&fakeRunner{})
		p.caps.HasPullRequests = false
		_, err := p.OpenProposal(context.Background(), testWorkspace(), ProposalOpts{BaseRef: "main"})
		var ue *UnsupportedOperationError
		if !errors.As(err, &ue) {
			t.Fatalf("err = %v, want *UnsupportedOperationError", err)
		}
	})
}

func TestGitHubMergeProposal(t *testing.T) {
	tests := []struct {
		name     string
		strategy string
		wantFlag string
	}{
		{"merge", "auto", "--merge"},
		{"rebase", "rebase", "--rebase"},
		{"squash", "squash", "--squash"},
		{"unknown maps to merge", "weird", "--merge"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := &fakeRunner{}
			p := availableGitHub(r)
			res, err := p.MergeProposal(context.Background(), ProposalRef{ID: "42", State: "open"}, tt.strategy)
			if err != nil {
				t.Fatalf("MergeProposal: %v", err)
			}
			if res.Kind != MergeClean {
				t.Errorf("Kind = %q, want clean", res.Kind)
			}
			joined := strings.Join(r.commandLines(), "\n")
			if !strings.Contains(joined, tt.wantFlag) {
				t.Errorf("merge flag = %s missing:\n%s", tt.wantFlag, joined)
			}
		})
	}

	t.Run("conflict surfaced", func(t *testing.T) {
		r := &fakeRunner{reply: errOnMatch("merge", "merge conflict between base and head", "")}
		p := availableGitHub(r)
		res, err := p.MergeProposal(context.Background(), ProposalRef{ID: "42", State: "open"}, "auto")
		if err != nil {
			t.Fatalf("MergeProposal: %v", err)
		}
		if res.Kind != MergeConflict {
			t.Errorf("Kind = %q, want conflict", res.Kind)
		}
	})

	t.Run("gated when HasPullRequests=false", func(t *testing.T) {
		p := availableGitHub(&fakeRunner{})
		p.caps.HasPullRequests = false
		_, err := p.MergeProposal(context.Background(), ProposalRef{ID: "1", State: "open"}, "auto")
		var ue *UnsupportedOperationError
		if !errors.As(err, &ue) {
			t.Fatalf("err = %v, want *UnsupportedOperationError", err)
		}
	})
}

func TestGitHubEnqueueForMerge(t *testing.T) {
	t.Run("happy path parses graphql", func(t *testing.T) {
		nodeResp := `{"data":{"repository":{"pullRequest":{"id":"PR_node_1"}}}}`
		enqueueResp := `{"data":{"enqueuePullRequest":{"mergeQueueEntry":{"state":"QUEUED","position":3,"enqueuedAt":"2026-04-27T01:02:03Z"}}}}`
		r := newSeqRunner(seqReply{stdout: nodeResp}, seqReply{stdout: enqueueResp})
		p := availableGitHub(r)
		ticket, err := p.EnqueueForMerge(context.Background(), ProposalRef{ID: "42", State: "open"}, QueueOpts{Priority: 1})
		if err != nil {
			t.Fatalf("EnqueueForMerge: %v", err)
		}
		if ticket.ID != "42:queue" {
			t.Errorf("ID = %q, want 42:queue", ticket.ID)
		}
		if ticket.Position != 3 {
			t.Errorf("Position = %d, want 3", ticket.Position)
		}
		if ticket.EnqueuedAt != "2026-04-27T01:02:03Z" {
			t.Errorf("EnqueuedAt = %q", ticket.EnqueuedAt)
		}
	})

	t.Run("empty enqueuedAt falls back to now", func(t *testing.T) {
		nodeResp := `{"data":{"repository":{"pullRequest":{"id":"PR_1"}}}}`
		enqueueResp := `{"data":{"enqueuePullRequest":{"mergeQueueEntry":{"state":"QUEUED","position":1,"enqueuedAt":""}}}}`
		r := newSeqRunner(seqReply{stdout: nodeResp}, seqReply{stdout: enqueueResp})
		p := availableGitHub(r)
		ticket, err := p.EnqueueForMerge(context.Background(), ProposalRef{ID: "9", State: "open"}, QueueOpts{})
		if err != nil {
			t.Fatalf("EnqueueForMerge: %v", err)
		}
		if ticket.EnqueuedAt == "" {
			t.Error("EnqueuedAt should fall back to now")
		}
	})

	t.Run("graphql errors surface", func(t *testing.T) {
		nodeResp := `{"data":{"repository":{"pullRequest":{"id":"PR_1"}}}}`
		errResp := `{"errors":[{"message":"merge queue not enabled"}]}`
		r := newSeqRunner(seqReply{stdout: nodeResp}, seqReply{stdout: errResp})
		p := availableGitHub(r)
		_, err := p.EnqueueForMerge(context.Background(), ProposalRef{ID: "9", State: "open"}, QueueOpts{})
		if err == nil || !strings.Contains(err.Error(), "merge queue not enabled") {
			t.Errorf("err = %v, want graphql error surfaced", err)
		}
	})

	t.Run("gated when HasMergeQueue=false", func(t *testing.T) {
		off := false
		p := NewGitHubProvider(GitHubOpts{Available: true, HasMergeQueueOverride: &off})
		p.runner = &fakeRunner{}
		_, err := p.EnqueueForMerge(context.Background(), ProposalRef{ID: "1", State: "open"}, QueueOpts{})
		var ue *UnsupportedOperationError
		if !errors.As(err, &ue) {
			t.Fatalf("err = %v, want *UnsupportedOperationError", err)
		}
	})
}

// ── pure helpers ────────────────────────────────────────────────────────────

func TestParsePRNumberFromURL(t *testing.T) {
	tests := []struct {
		in      string
		want    string
		wantErr bool
	}{
		{"https://github.com/org/repo/pull/42", "42", false},
		{"https://github.com/org/repo/pull/9999\n", "9999", false},
		{"https://github.com/org/repo/issues/42", "", true},
		{"garbage", "", true},
	}
	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			got, err := parsePRNumberFromURL(tt.in)
			if (err != nil) != tt.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tt.wantErr)
			}
			if got != tt.want {
				t.Errorf("got %q, want %q", got, tt.want)
			}
		})
	}
}

func TestGithubMergeFlag(t *testing.T) {
	tests := map[string]string{
		"auto": "--merge", "three-way-text": "--merge", "": "--merge",
		"rebase": "--rebase", "squash": "--squash", "unknown": "--merge",
	}
	for in, want := range tests {
		if got := githubMergeFlag(in); got != want {
			t.Errorf("githubMergeFlag(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestSplitMessageSegments(t *testing.T) {
	got := splitMessageSegments("subject\n\ntrailer1\ntrailer2")
	if len(got) != 2 {
		t.Fatalf("len = %d, want 2: %q", len(got), got)
	}
	if got[0] != "subject" || got[1] != "trailer1\ntrailer2" {
		t.Errorf("segments = %q", got)
	}
}
