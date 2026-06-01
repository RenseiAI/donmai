package codesurvival

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

// stubGit is a table-driven gitRunner: it matches on a key derived from the
// args and returns the configured output/error. Unmatched calls return "" / nil
// (a benign default) unless errOnUnmatched is set.
type stubGit struct {
	// responses keys on a space-joined subset of args (see key()).
	responses map[string]stubResp
	// calls records every invocation for assertions.
	calls []string
}

type stubResp struct {
	out string
	err error
}

func (g *stubGit) run(_ context.Context, _ string, args ...string) (string, error) {
	k := key(args)
	g.calls = append(g.calls, k)
	if r, ok := g.responses[k]; ok {
		return r.out, r.err
	}
	// Default benign responses for calls the test doesn't pin.
	switch {
	case len(args) > 0 && args[0] == "--version":
		return "git version 2.45.0", nil
	case len(args) > 0 && args[0] == "clone":
		return "", nil
	case len(args) > 0 && args[0] == "remote":
		return "", nil
	case len(args) >= 2 && args[0] == "rev-parse":
		return "1111111111111111111111111111111111111111", nil
	case len(args) >= 2 && args[0] == "cat-file":
		return "", nil
	}
	return "", nil
}

// key collapses an arg list to a stable lookup string. For blame we key on the
// ref + file so at-merge vs at-head are distinguishable.
func key(args []string) string {
	return strings.Join(args, " ")
}

func newStubGit() *stubGit { return &stubGit{responses: map[string]stubResp{}} }

// signedlessJWT builds a 3-segment token whose payload carries org_id=org. The
// signature segment is a placeholder — the worker re-verifies the CLAIM only
// (it lacks the platform secret), so the signature value is irrelevant here.
func signedlessJWT(org string) string {
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"HS256","typ":"JWT"}`))
	payloadJSON, _ := json.Marshal(map[string]string{"org_id": org, "sub": "worker-1"})
	payload := base64.RawURLEncoding.EncodeToString(payloadJSON)
	return header + "." + payload + ".sig"
}

func discardLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// fixtureItem returns a valid batch item targeting srvURL with org "org-1".
func fixtureItem(resultEndpoint string) BatchWorkItem {
	org := "org-1"
	return BatchWorkItem{
		BatchJobID:      "batch:due_checkpoint:1",
		WorkType:        WorkTypeCodeSurvivalScan,
		ContractVersion: CodeSurvivalContractVersion,
		OrgID:           org,
		ProjectID:       "proj-1",
		AttributionID:   "attr-1",
		Checkpoint:      30,
		PRRepo:          "owner/repo",
		PRNumber:        123,
		MergeSha:        "abcdef0123456789abcdef0123456789abcdef01",
		Needs:           BatchWorkNeeds{NeedsGo: true, NeedsNode: true},
		//nolint:gosec // G101: fake token in a test fixture, not a real credential.
		GitCredential: BatchWorkGitCredential{
			Token:    "ghs_secrettoken",
			CloneURL: "https://x-access-token:ghs_secrettoken@github.com/owner/repo.git",
		},
		ResultEndpoint: resultEndpoint,
		ResultAuth:     signedlessJWT(org),
	}
}

// captureServer returns an httptest server that records the last posted body +
// auth header, and a pointer to the decoded result.
func captureServer(t *testing.T, hits *atomic.Int32, gotAuth *string, gotResult *CodeSurvivalScanResult) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		*gotAuth = r.Header.Get("Authorization")
		b, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
		_ = json.Unmarshal(b, gotResult)
		w.WriteHeader(200)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
}

func TestExecutor_SurvivalPayload(t *testing.T) {
	merge := "abcdef0123456789abcdef0123456789abcdef01"
	git := newStubGit()
	// diff-tree lists one file.
	git.responses[key([]string{"diff-tree", "--no-commit-id", "--name-only", "-r", "--diff-filter=AM", merge})] = stubResp{out: "main.go"}
	// blame at merge: 4 lines authored by the merge.
	git.responses[key([]string{"blame", "-l", "--line-porcelain", merge, "--", "main.go"})] = stubResp{out: blameEntry(merge, 1) + blameEntry(merge, 2) + blameEntry(merge, 3) + blameEntry(merge, 4)}
	// blame at HEAD: 3 of the 4 still attributed to merge.
	git.responses[key([]string{"blame", "-l", "--line-porcelain", "HEAD", "--", "main.go"})] = stubResp{out: blameEntry(merge, 1) + blameEntry(merge, 2) + blameEntry("fedcba9876543210fedcba9876543210fedcba98", 3) + blameEntry(merge, 4)}

	var hits atomic.Int32
	var gotAuth string
	var gotResult CodeSurvivalScanResult
	srv := captureServer(t, &hits, &gotAuth, &gotResult)
	defer srv.Close()

	exec := NewExecutor(Options{
		GitRunner:        git,
		WorkareaProvider: func() (Workarea, error) { return Workarea{Path: "/tmp/wa", Release: func() {}}, nil },
		HTTPClient:       srv.Client(),
		Logger:           discardLogger(),
		WorkerVersion:    "v0.1.0-test",
		PoolProviderID:   "local",
		// Survival-only: a no-op (non-degraded) reachability pass keeps this an
		// RW3 survival test. RW4 reachability is exercised in
		// executor_reachability_test.go. Surviving lines fall in no span →
		// unknown → hot, so status stays ok (no down-weight, no partial).
		GoReachability: noopReachability,
		TSRunner:       &stubTSRunner{avail: false},
	})

	item := fixtureItem(srv.URL)
	item.MergeSha = merge
	if err := exec.Handle(context.Background(), item); err != nil {
		t.Fatalf("Handle returned error: %v", err)
	}

	if hits.Load() != 1 {
		t.Fatalf("expected exactly 1 result POST, got %d", hits.Load())
	}
	if gotAuth != "Bearer "+item.ResultAuth {
		t.Errorf("auth = %q, want bearer of resultAuth", gotAuth)
	}
	if gotResult.Status != StatusOK {
		t.Errorf("status = %q, want ok", gotResult.Status)
	}
	if gotResult.Survival.LinesTotalAtMerge != 4 {
		t.Errorf("linesTotalAtMerge = %d, want 4", gotResult.Survival.LinesTotalAtMerge)
	}
	if gotResult.Survival.LinesSurviving != 3 {
		t.Errorf("linesSurviving = %d, want 3", gotResult.Survival.LinesSurviving)
	}
	if gotResult.Survival.SurvivalRatePct == nil || *gotResult.Survival.SurvivalRatePct != 75 {
		t.Errorf("survivalRatePct = %v, want 75", gotResult.Survival.SurvivalRatePct)
	}
	if gotResult.ContractVersion != CodeSurvivalContractVersion {
		t.Errorf("contractVersion = %d", gotResult.ContractVersion)
	}
	// RW4: with a clean (no-op) reachability run and no resolved spans, all
	// surviving lines are unknown → hot (no down-weight). hotWeighted is
	// populated; perSymbol is empty (no span matched a surviving line).
	if gotResult.HotWeighted == nil {
		t.Errorf("hotWeighted should be populated after a clean reachability run")
	} else if gotResult.HotWeighted.HotLinesSurviving != 3 || gotResult.HotWeighted.ColdLinesSurviving != 0 {
		t.Errorf("hot/cold = %d/%d, want 3/0 (all unknown→hot)",
			gotResult.HotWeighted.HotLinesSurviving, gotResult.HotWeighted.ColdLinesSurviving)
	}
	if gotResult.PerSymbol == nil || len(gotResult.PerSymbol) != 0 {
		t.Errorf("perSymbol should be empty slice (no span matched), got %v", gotResult.PerSymbol)
	}
	if gotResult.Executor.WorkerVersion != "v0.1.0-test" || gotResult.Executor.PoolProviderID != "local" {
		t.Errorf("executor info = %+v", gotResult.Executor)
	}
}

func TestExecutor_Idempotent(t *testing.T) {
	// Re-running the same checkpoint against the same git state must produce a
	// byte-identical payload (modulo HEAD, which is deterministic in the stub).
	merge := "abcdef0123456789abcdef0123456789abcdef01"
	mkGit := func() *stubGit {
		g := newStubGit()
		g.responses[key([]string{"diff-tree", "--no-commit-id", "--name-only", "-r", "--diff-filter=AM", merge})] = stubResp{out: "a.go\nb.go"}
		g.responses[key([]string{"blame", "-l", "--line-porcelain", merge, "--", "a.go"})] = stubResp{out: blameEntry(merge, 1) + blameEntry(merge, 2)}
		g.responses[key([]string{"blame", "-l", "--line-porcelain", "HEAD", "--", "a.go"})] = stubResp{out: blameEntry(merge, 1) + blameEntry(merge, 2)}
		g.responses[key([]string{"blame", "-l", "--line-porcelain", merge, "--", "b.go"})] = stubResp{out: blameEntry(merge, 1)}
		g.responses[key([]string{"blame", "-l", "--line-porcelain", "HEAD", "--", "b.go"})] = stubResp{out: blameEntry(merge, 1)}
		return g
	}

	run := func() CodeSurvivalScanResult {
		var hits atomic.Int32
		var gotAuth string
		var got CodeSurvivalScanResult
		srv := captureServer(t, &hits, &gotAuth, &got)
		defer srv.Close()
		exec := NewExecutor(Options{
			GitRunner:        mkGit(),
			WorkareaProvider: func() (Workarea, error) { return Workarea{Path: "/tmp/wa", Release: func() {}}, nil },
			HTTPClient:       srv.Client(),
			Logger:           discardLogger(),
			GoReachability:   noopReachability,
			TSRunner:         &stubTSRunner{avail: false},
		})
		item := fixtureItem(srv.URL)
		item.MergeSha = merge
		_ = exec.Handle(context.Background(), item)
		return got
	}

	a := run()
	b := run()
	aj, _ := json.Marshal(a)
	bj, _ := json.Marshal(b)
	if string(aj) != string(bj) {
		t.Errorf("payloads differ across runs:\n a=%s\n b=%s", aj, bj)
	}
	if a.Survival.LinesTotalAtMerge != 3 || a.Survival.LinesSurviving != 3 {
		t.Errorf("counts = (%d,%d), want (3,3)", a.Survival.LinesTotalAtMerge, a.Survival.LinesSurviving)
	}
}

func TestExecutor_OrgMismatchRejected(t *testing.T) {
	git := newStubGit()
	var hits atomic.Int32
	var gotAuth string
	var got CodeSurvivalScanResult
	srv := captureServer(t, &hits, &gotAuth, &got)
	defer srv.Close()

	cloned := false
	exec := NewExecutor(Options{
		GitRunner: git,
		WorkareaProvider: func() (Workarea, error) {
			cloned = true // acquiring a workarea means we got past the org guard
			return Workarea{Path: "/tmp/wa", Release: func() {}}, nil
		},
		HTTPClient: srv.Client(),
		Logger:     discardLogger(),
	})

	item := fixtureItem(srv.URL)
	item.ResultAuth = signedlessJWT("EVIL-ORG") // claim != item.OrgID ("org-1")

	err := exec.Handle(context.Background(), item)
	if err == nil {
		t.Fatal("expected rejection error for org mismatch")
	}
	if !errors.Is(err, ErrOrgClaimMismatch) {
		t.Errorf("err = %v, want ErrOrgClaimMismatch", err)
	}
	if cloned {
		t.Error("workarea acquired despite org mismatch — guard must reject before any clone")
	}
	if hits.Load() != 0 {
		t.Errorf("posted %d results despite rejection, want 0", hits.Load())
	}
}

func TestExecutor_UnreachableMergeSha_Skipped(t *testing.T) {
	merge := "abcdef0123456789abcdef0123456789abcdef01"
	git := newStubGit()
	// cat-file errors → merge sha unreachable.
	git.responses[key([]string{"cat-file", "-e", merge + "^{commit}"})] = stubResp{err: errors.New("fatal: Not a valid object name")}

	var hits atomic.Int32
	var gotAuth string
	var got CodeSurvivalScanResult
	srv := captureServer(t, &hits, &gotAuth, &got)
	defer srv.Close()

	exec := NewExecutor(Options{
		GitRunner:        git,
		WorkareaProvider: func() (Workarea, error) { return Workarea{Path: "/tmp/wa", Release: func() {}}, nil },
		HTTPClient:       srv.Client(),
		Logger:           discardLogger(),
	})
	item := fixtureItem(srv.URL)
	item.MergeSha = merge

	if err := exec.Handle(context.Background(), item); err != nil {
		t.Fatalf("Handle should not error on skip, got %v", err)
	}
	if got.Status != StatusSkipped {
		t.Errorf("status = %q, want skipped", got.Status)
	}
	if got.SkipReason == nil || *got.SkipReason != SkipShallowHistory {
		t.Errorf("skipReason = %v, want shallow_history", got.SkipReason)
	}
}

func TestExecutor_CloneFailure_Skipped(t *testing.T) {
	git := newStubGit()
	merge := "abcdef0123456789abcdef0123456789abcdef01"
	git.responses[key([]string{"clone", "https://x-access-token:ghs_secrettoken@github.com/owner/repo.git", "/tmp/wa"})] = stubResp{err: errors.New("fatal: repository not found")}

	var hits atomic.Int32
	var gotAuth string
	var got CodeSurvivalScanResult
	srv := captureServer(t, &hits, &gotAuth, &got)
	defer srv.Close()

	exec := NewExecutor(Options{
		GitRunner:        git,
		WorkareaProvider: func() (Workarea, error) { return Workarea{Path: "/tmp/wa", Release: func() {}}, nil },
		HTTPClient:       srv.Client(),
		Logger:           discardLogger(),
	})
	item := fixtureItem(srv.URL)
	item.MergeSha = merge

	if err := exec.Handle(context.Background(), item); err != nil {
		t.Fatalf("Handle should not error on clone-skip, got %v", err)
	}
	if got.Status != StatusSkipped {
		t.Errorf("status = %q, want skipped", got.Status)
	}
	if got.SkipReason == nil || *got.SkipReason != SkipRepoGone {
		t.Errorf("skipReason = %v, want repo_gone", got.SkipReason)
	}
}

func TestExecutor_WorkareaReleased(t *testing.T) {
	// Credential scrub = workarea release; assert Release fires on every path.
	merge := "abcdef0123456789abcdef0123456789abcdef01"
	git := newStubGit()
	git.responses[key([]string{"diff-tree", "--no-commit-id", "--name-only", "-r", "--diff-filter=AM", merge})] = stubResp{out: ""}

	var hits atomic.Int32
	var gotAuth string
	var got CodeSurvivalScanResult
	srv := captureServer(t, &hits, &gotAuth, &got)
	defer srv.Close()

	var released atomic.Int32
	exec := NewExecutor(Options{
		GitRunner: git,
		WorkareaProvider: func() (Workarea, error) {
			return Workarea{Path: "/tmp/wa", Release: func() { released.Add(1) }}, nil
		},
		HTTPClient: srv.Client(),
		Logger:     discardLogger(),
	})
	item := fixtureItem(srv.URL)
	item.MergeSha = merge
	_ = exec.Handle(context.Background(), item)

	if released.Load() != 1 {
		t.Errorf("workarea Release called %d times, want 1 (credential scrub)", released.Load())
	}
}

func TestExecutor_UnknownContractVersion_Rejected(t *testing.T) {
	exec := NewExecutor(Options{GitRunner: newStubGit(), Logger: discardLogger()})
	item := fixtureItem("")
	item.ContractVersion = 999
	err := exec.Handle(context.Background(), item)
	if err == nil || !strings.Contains(err.Error(), "contract version") {
		t.Errorf("err = %v, want contract-version rejection", err)
	}
}
