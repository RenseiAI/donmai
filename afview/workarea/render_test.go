package workarea_test

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/RenseiAI/agentfactory-tui/afclient"
	"github.com/RenseiAI/agentfactory-tui/afview/workarea"
)

var (
	testNow     = time.Date(2026, 4, 28, 12, 0, 0, 0, time.UTC)
	testEarlier = testNow.Add(-2 * time.Hour)
)

func sampleListResponse() *afclient.ListWorkareasResponse {
	return &afclient.ListWorkareasResponse{
		Active: []afclient.WorkareaSummary{{
			ID:         "wa-active-1",
			Kind:       afclient.WorkareaKindActive,
			ProviderID: "local-pool",
			SessionID:  "sess-abc",
			ProjectID:  "proj-xyz",
			Status:     afclient.WorkareaStatusAcquired,
			Ref:        "refs/heads/main",
			AgeSeconds: 7200,
			AcquiredAt: &testEarlier,
		}},
		Archived: []afclient.WorkareaSummary{{
			ID:         "wa-arch-1",
			Kind:       afclient.WorkareaKindArchived,
			ProviderID: "local-pool",
			SessionID:  "sess-def",
			Status:     afclient.WorkareaStatusArchived,
			Ref:        "a1b2c3d4e5f6",
			CreatedAt:  &testNow,
			SizeBytes:  1234,
		}},
	}
}

func TestRenderList_HappyPath(t *testing.T) {
	var buf bytes.Buffer
	if err := workarea.RenderList(&buf, sampleListResponse(), true); err != nil {
		t.Fatalf("render: %v", err)
	}
	out := buf.String()
	for _, want := range []string{"ACTIVE", "ARCHIVED", "wa-active-1", "wa-arch-1", "acquired", "archived"} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q\n--- output ---\n%s", want, out)
		}
	}
}

func TestRenderList_EmptyResponse(t *testing.T) {
	var buf bytes.Buffer
	resp := &afclient.ListWorkareasResponse{}
	if err := workarea.RenderList(&buf, resp, true); err != nil {
		t.Fatalf("render: %v", err)
	}
	if !strings.Contains(buf.String(), "No workareas found") {
		t.Errorf("expected 'No workareas found' for empty response, got:\n%s", buf.String())
	}
}

func TestRenderList_NilResponse(t *testing.T) {
	var buf bytes.Buffer
	if err := workarea.RenderList(&buf, nil, true); err != nil {
		t.Fatalf("render: %v", err)
	}
	if !strings.Contains(buf.String(), "No workareas found") {
		t.Errorf("expected 'No workareas found' for nil response, got:\n%s", buf.String())
	}
}

func TestPlainList_DeterministicOrder(t *testing.T) {
	resp := &afclient.ListWorkareasResponse{
		Archived: []afclient.WorkareaSummary{
			{ID: "z-1", Status: afclient.WorkareaStatusArchived},
			{ID: "a-1", Status: afclient.WorkareaStatusArchived},
			{ID: "m-1", Status: afclient.WorkareaStatusArchived},
		},
	}
	var buf bytes.Buffer
	if err := workarea.PlainList(&buf, resp); err != nil {
		t.Fatalf("plain list: %v", err)
	}
	out := buf.String()
	idxA := strings.Index(out, "a-1")
	idxM := strings.Index(out, "m-1")
	idxZ := strings.Index(out, "z-1")
	if !(idxA < idxM && idxM < idxZ) {
		t.Errorf("entries not sorted by id; saw a@%d m@%d z@%d\n%s", idxA, idxM, idxZ, out)
	}
}

func sampleWorkarea() *afclient.Workarea {
	return &afclient.Workarea{
		ID:                 "wa-001",
		Kind:               afclient.WorkareaKindArchived,
		ProviderID:         "local-pool",
		SessionID:          "sess-abc",
		Status:             afclient.WorkareaStatusArchived,
		Path:               "/var/rensei/wa-001/tree",
		Ref:                "refs/heads/main",
		Repository:         "github.com/acme/repo",
		CleanStateChecksum: "sha256:deadbeef",
		Toolchain:          map[string]string{"node": "20.18.1", "pnpm": "9.0.0"},
		Mode:               "exclusive",
		AcquirePath:        "pool-warm",
		AcquiredAt:         &testEarlier,
	}
}

func TestRenderInspect_HappyPath(t *testing.T) {
	var buf bytes.Buffer
	if err := workarea.RenderInspect(&buf, sampleWorkarea(), true); err != nil {
		t.Fatalf("inspect: %v", err)
	}
	out := buf.String()
	for _, want := range []string{
		"wa-001", "archived", "sess-abc", "github.com/acme/repo",
		"refs/heads/main", "exclusive", "pool-warm",
		"sha256:deadbeef", "Toolchain:", "node:", "20.18.1", "pnpm:", "9.0.0",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("inspect missing %q\n--- output ---\n%s", want, out)
		}
	}
}

func TestRenderInspect_NilWorkarea(t *testing.T) {
	var buf bytes.Buffer
	if err := workarea.RenderInspect(&buf, nil, true); err != nil {
		t.Fatalf("inspect: %v", err)
	}
	if !strings.Contains(buf.String(), "No workarea record") {
		t.Errorf("expected 'No workarea record' for nil")
	}
}

func TestRenderRestore_HappyPath(t *testing.T) {
	var buf bytes.Buffer
	res := &afclient.WorkareaRestoreResult{Workarea: afclient.Workarea{
		ID:              "wa-restore-1",
		Kind:            afclient.WorkareaKindActive,
		Status:          afclient.WorkareaStatusReady,
		SessionID:       "sess-investigation",
		Path:            "/tmp/restored/wa-restore-1",
		ArchiveLocation: "/var/rensei/wa-source",
	}}
	if err := workarea.RenderRestore(&buf, res, true); err != nil {
		t.Fatalf("restore: %v", err)
	}
	out := buf.String()
	for _, want := range []string{
		"Workarea restored", "wa-restore-1", "active", "ready",
		"sess-investigation", "/tmp/restored/wa-restore-1", "/var/rensei/wa-source",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("restore missing %q\n--- output ---\n%s", want, out)
		}
	}
}

func TestRenderRestore_NilResult(t *testing.T) {
	var buf bytes.Buffer
	_ = workarea.RenderRestore(&buf, nil, true)
	if !strings.Contains(buf.String(), "No restore result") {
		t.Errorf("expected 'No restore result' for nil")
	}
}

func TestRenderDiff_HappyPath(t *testing.T) {
	diff := &afclient.WorkareaDiffResult{
		Summary: afclient.WorkareaDiffSummary{
			WorkareaA: "wa-fail", WorkareaB: "wa-pass",
			Added: 1, Removed: 1, Modified: 2, Total: 4,
		},
		Entries: []afclient.WorkareaDiffEntry{
			{Path: "src/added.go", Status: afclient.WorkareaDiffStatusAdded},
			{Path: "src/main.go", Status: afclient.WorkareaDiffStatusModified, HashA: "a1", HashB: "b1"},
			{Path: "src/util.go", Status: afclient.WorkareaDiffStatusModified, HashA: "a2", HashB: "b2"},
			{Path: "src/removed.go", Status: afclient.WorkareaDiffStatusRemoved},
		},
	}
	var buf bytes.Buffer
	if err := workarea.RenderDiff(&buf, diff, true); err != nil {
		t.Fatalf("diff: %v", err)
	}
	out := buf.String()
	for _, want := range []string{
		"wa-fail", "wa-pass", "STATUS", "PATH",
		"added", "src/added.go",
		"modified", "src/main.go", "src/util.go",
		"removed", "src/removed.go",
		"added: 1", "removed: 1", "modified: 2", "total: 4",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("diff missing %q\n--- output ---\n%s", want, out)
		}
	}
}

func TestPlainDiff_DeterministicOrder(t *testing.T) {
	// Out-of-order input; the renderer must emit added → modified → removed.
	diff := &afclient.WorkareaDiffResult{
		Summary: afclient.WorkareaDiffSummary{Total: 4},
		Entries: []afclient.WorkareaDiffEntry{
			{Path: "z.go", Status: afclient.WorkareaDiffStatusRemoved},
			{Path: "a.go", Status: afclient.WorkareaDiffStatusModified},
			{Path: "b.go", Status: afclient.WorkareaDiffStatusAdded},
			{Path: "c.go", Status: afclient.WorkareaDiffStatusModified},
		},
	}
	var buf bytes.Buffer
	if err := workarea.PlainDiff(&buf, diff); err != nil {
		t.Fatalf("plain diff: %v", err)
	}
	out := buf.String()
	addedAt := strings.Index(out, "b.go")
	modifiedAAt := strings.Index(out, "a.go")
	modifiedCAt := strings.Index(out, "c.go")
	removedAt := strings.Index(out, "z.go")
	// Deterministic order: added (b.go) < modified (a.go, c.go) < removed (z.go).
	if !(addedAt < modifiedAAt && modifiedAAt < modifiedCAt && modifiedCAt < removedAt) {
		t.Errorf("ordering wrong: added@%d modA@%d modC@%d removed@%d\n%s",
			addedAt, modifiedAAt, modifiedCAt, removedAt, out)
	}
}

func TestRenderDiff_EmptyEntries(t *testing.T) {
	diff := &afclient.WorkareaDiffResult{
		Summary: afclient.WorkareaDiffSummary{
			WorkareaA: "a", WorkareaB: "b",
		},
		Entries: []afclient.WorkareaDiffEntry{},
	}
	var buf bytes.Buffer
	_ = workarea.RenderDiff(&buf, diff, true)
	if !strings.Contains(buf.String(), "No differences") {
		t.Errorf("expected 'No differences' for empty diff")
	}
}

func TestRenderDiff_NilResult(t *testing.T) {
	var buf bytes.Buffer
	_ = workarea.RenderDiff(&buf, nil, true)
	if !strings.Contains(buf.String(), "No diff result") {
		t.Errorf("expected 'No diff result' for nil")
	}
}

func TestNoColorEnv(t *testing.T) {
	// Just verify it doesn't panic; the actual env varies in CI.
	_ = workarea.NoColorEnv()
}

// TestRenderList_PlainFallbackHasNoANSI snapshots the plain mode output
// to assert there are no ANSI escape codes — rensei-smokes' diff smoke
// pins against this guarantee.
func TestRenderList_PlainFallbackHasNoANSI(t *testing.T) {
	var buf bytes.Buffer
	if err := workarea.PlainList(&buf, sampleListResponse()); err != nil {
		t.Fatalf("plain: %v", err)
	}
	if strings.Contains(buf.String(), "\033[") {
		t.Errorf("plain list contains ANSI escape sequences:\n%s", buf.String())
	}
}

func TestRenderInspect_PlainFallbackHasNoANSI(t *testing.T) {
	var buf bytes.Buffer
	if err := workarea.PlainInspect(&buf, sampleWorkarea()); err != nil {
		t.Fatalf("plain inspect: %v", err)
	}
	if strings.Contains(buf.String(), "\033[") {
		t.Errorf("plain inspect contains ANSI escape sequences:\n%s", buf.String())
	}
}

func TestRenderDiff_PlainFallbackHasNoANSI(t *testing.T) {
	diff := &afclient.WorkareaDiffResult{
		Summary: afclient.WorkareaDiffSummary{WorkareaA: "a", WorkareaB: "b", Total: 1},
		Entries: []afclient.WorkareaDiffEntry{
			{Path: "x.go", Status: afclient.WorkareaDiffStatusModified},
		},
	}
	var buf bytes.Buffer
	if err := workarea.PlainDiff(&buf, diff); err != nil {
		t.Fatalf("plain diff: %v", err)
	}
	if strings.Contains(buf.String(), "\033[") {
		t.Errorf("plain diff contains ANSI escape sequences:\n%s", buf.String())
	}
}
