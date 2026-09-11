package pi

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/RenseiAI/donmai/agent"
)

func TestSameVersionAndCopiedSidecarCannotSelectProfile(t *testing.T) {
	root := t.TempDir()
	binary := filepath.Join(root, "pi")
	if err := os.WriteFile(binary, []byte("different 0.85.1 binary"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(binary, 0o700); err != nil { //nolint:gosec // G302: executable fixture must be resolvable as the candidate binary.
		t.Fatal(err)
	}
	sidecar, err := os.ReadFile(filepath.Join(frozenArtifactRoot(t), artifactSidecarName))
	if err != nil {
		t.Fatal(err)
	}
	sidecarPath := filepath.Join(root, artifactSidecarName)
	if err := os.WriteFile(sidecarPath, sidecar, 0o600); err != nil { //nolint:gosec // G703: sidecarPath is a fixed basename beneath t.TempDir.
		t.Fatal(err)
	}
	if err := os.Chmod(sidecarPath, 0o444); err != nil { //nolint:gosec // G302: reproduce the frozen descriptor's compiled mode.
		t.Fatal(err)
	}
	_, err = New(Options{
		PiBin:        binary,
		VersionProbe: func(context.Context, string) (string, error) { return "0.85.1", nil },
	})
	if err == nil || !errors.Is(err, agent.ErrProviderUnavailable) {
		t.Fatalf("metadata-only same-version artifact error=%v, want provider unavailable", err)
	}

	if err := os.Remove(filepath.Join(root, artifactSidecarName)); err != nil {
		t.Fatal(err)
	}
	legacy, err := New(Options{
		PiBin:        binary,
		VersionProbe: func(context.Context, string) (string, error) { return "0.85.1", nil },
	})
	if err != nil {
		t.Fatalf("ordinary no-sidecar binary lost legacy behavior: %v", err)
	}
	t.Cleanup(func() { _ = legacy.Shutdown(context.Background()) })
	if legacy.artifact != nil {
		t.Fatal("no-sidecar binary selected a receipt artifact profile")
	}
}

func mutateCopiedArtifact(t *testing.T, root string) {
	t.Helper()
	path := filepath.Join(root, "README.md")
	if err := os.Chmod(path, 0o600); err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString("drift"); err != nil {
		_ = f.Close()
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestProfiledArtifactDriftFailsImmediatelyBeforeChildStart(t *testing.T) {
	root := copyArtifact(t)
	started := false
	p, err := New(Options{
		PiBin:            filepath.Join(root, "pi"),
		VersionProbe:     func(context.Context, string) (string, error) { return "0.85.1", nil },
		beforeChildStart: func() { mutateCopiedArtifact(t, root) },
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = p.Shutdown(context.Background()) })
	_, err = p.Spawn(context.Background(), agent.Spec{
		Prompt: "must not run", Cwd: t.TempDir(), Autonomous: true,
		OnProcessSpawned: func(int) { started = true },
	})
	if err == nil || !strings.Contains(err.Error(), "artifact changed before spawn") {
		t.Fatalf("Spawn error=%v, want pre-start artifact refusal", err)
	}
	if started {
		t.Fatal("OnProcessSpawned ran despite pre-start artifact refusal")
	}
}

func TestProfiledArtifactDriftStopsChildImmediatelyAfterStart(t *testing.T) {
	root := copyArtifact(t)
	p, err := New(Options{
		PiBin:            filepath.Join(root, "pi"),
		VersionProbe:     func(context.Context, string) (string, error) { return "0.85.1", nil },
		HandshakeTimeout: 20 * time.Second,
		afterChildStart:  func() { mutateCopiedArtifact(t, root) },
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = p.Shutdown(context.Background()) })
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	_, err = p.Spawn(ctx, agent.Spec{Prompt: "must not run", Cwd: t.TempDir(), Autonomous: true})
	if err == nil || !strings.Contains(err.Error(), "artifact changed after spawn") {
		t.Fatalf("Spawn error=%v, want post-start artifact refusal", err)
	}
}
