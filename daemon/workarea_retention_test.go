package daemon

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/RenseiAI/donmai/runtime/worktree"
)

func TestRunRetentionPass_DropsExpiredArchive(t *testing.T) {
	parent := t.TempDir()
	archiveRoot := t.TempDir()
	registry := NewWorkareaArchiveRegistry(WorkareaArchiveOptions{Root: archiveRoot})
	dir := filepath.Join(archiveRoot, "wa-old")
	if err := os.MkdirAll(dir, 0o750); err != nil {
		t.Fatal(err)
	}
	created := time.Now().Add(-31 * 24 * time.Hour).UTC().Format(time.RFC3339Nano)
	body := `{"id":"wa-old","createdAt":"` + created + `"}` + "\n"
	if err := os.WriteFile(filepath.Join(dir, "manifest.json"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	manager, err := worktree.NewManager(worktree.Options{
		ParentDir: parent, RetentionArchiveRoot: archiveRoot,
	})
	if err != nil {
		t.Fatal(err)
	}
	report, err := RunRetentionPass(context.Background(), manager, registry, RetentionSettings{
		MaxAgeDays: -1, ArchiveMaxAgeDays: 30,
	})
	if err != nil {
		t.Fatal(err)
	}
	if report.ArchivesDropped != 1 || report.Expired != 0 {
		t.Fatalf("report = %+v, want one archive dropped and nothing expired", report)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("expired archive still exists: %v", err)
	}
}

func TestRunRetentionPass_NilManagerIsNoop(t *testing.T) {
	registry := NewWorkareaArchiveRegistry(WorkareaArchiveOptions{Root: t.TempDir()})
	report, err := RunRetentionPass(context.Background(), nil, registry, RetentionSettings{})
	if err != nil {
		t.Fatal(err)
	}
	if report.Expired != 0 || report.ArchivesDropped != 0 {
		t.Fatalf("report = %+v, want zero", report)
	}
}
