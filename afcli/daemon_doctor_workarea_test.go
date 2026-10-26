package afcli

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/RenseiAI/donmai/internal/statepath"
	"github.com/RenseiAI/donmai/runtime/statehome"
	"github.com/RenseiAI/donmai/runtime/workarea"
)

// doctorWorkareaFileIdentity mirrors the durable identity JSON so the test
// can simulate a device renumber without reaching into unexported types.
type doctorWorkareaFileIdentity struct {
	Device    uint64 `json:"device"`
	Inode     uint64 `json:"inode"`
	BirthSec  int64  `json:"birthSec,omitempty"`
	BirthNsec int64  `json:"birthNsec,omitempty"`
	HasBirth  bool   `json:"hasBirth,omitempty"`
}

type doctorWorkareaStoreDoc struct {
	SchemaVersion string                     `json:"schemaVersion"`
	StoreID       string                     `json:"storeId"`
	Identity      doctorWorkareaFileIdentity `json:"identity"`
}

func renumberDeviceForDoctorTest(t *testing.T, path string, delta uint64) {
	t.Helper()
	raw, err := os.ReadFile(path) //nolint:gosec // test-only TempDir path
	if err != nil {
		t.Fatal(err)
	}
	var doc doctorWorkareaStoreDoc
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	if doc.Identity.Inode == 0 {
		t.Fatalf("store identity at %q has no inode", path)
	}
	doc.Identity.Device += delta
	if doc.Identity.Device == doc.Identity.Device-delta {
		doc.Identity.Device++
	}
	out, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, out, 0o600); err != nil {
		t.Fatal(err)
	}
}

// TestDaemonDoctorRefusesRenumberedStoreWithoutFix pins the AC-3 refusal:
// a device-only renumber must surface the re-anchor error and the
// Workarea store row instead of exiting 0.
func TestDaemonDoctorRefusesRenumberedStoreWithoutFix(t *testing.T) {
	home := t.TempDir()
	priorHome := statehome.BaseHome()
	statehome.SetBaseHome(home)
	t.Cleanup(func() { statehome.SetBaseHome(priorHome) })
	t.Setenv("HOME", home)

	worktreeParent := statepath.Resolve("worktrees", "/tmp/.donmai/worktrees")
	if err := os.MkdirAll(worktreeParent, 0o750); err != nil {
		t.Fatal(err)
	}
	if _, err := workarea.NewAcquisitionStore(worktreeParent, nil); err != nil {
		t.Fatal(err)
	}
	renumberDeviceForDoctorTest(t, filepath.Join(worktreeParent, ".workarea-acquisitions", "store.json"), 1000)
	renumberDeviceForDoctorTest(t, filepath.Join(worktreeParent, ".workarea-acquisitions.identity.json"), 1000)

	cmd := newDaemonDoctorCmd("donmai")
	buf := &bytes.Buffer{}
	cmd.SetOut(buf)
	cmd.SetErr(buf)
	cmd.SetArgs(nil)
	err := cmd.Execute()
	if err == nil {
		t.Fatalf("doctor without --fix on a renumbered store should fail; output:\n%s", buf.String())
	}
	if !strings.Contains(err.Error(), "re-anchor") {
		t.Fatalf("doctor error = %q, want re-anchor guidance", err.Error())
	}
	out := buf.String()
	if !strings.Contains(out, "Workarea store:") {
		t.Errorf("doctor output should include the Workarea store row; got:\n%s", out)
	}
	if !strings.Contains(out, "--fix") {
		t.Errorf("doctor output should point at --fix; got:\n%s", out)
	}
}

// TestDaemonDoctorFixReanchorsRenumberedStore pins the AC-3 repair wiring:
// --fix must re-anchor, print the re-anchored note, and leave a healthy
// store behind.
func TestDaemonDoctorFixReanchorsRenumberedStore(t *testing.T) {
	home := t.TempDir()
	priorHome := statehome.BaseHome()
	statehome.SetBaseHome(home)
	t.Cleanup(func() { statehome.SetBaseHome(priorHome) })
	t.Setenv("HOME", home)

	worktreeParent := statepath.Resolve("worktrees", "/tmp/.donmai/worktrees")
	if err := os.MkdirAll(worktreeParent, 0o750); err != nil {
		t.Fatal(err)
	}
	if _, err := workarea.NewAcquisitionStore(worktreeParent, nil); err != nil {
		t.Fatal(err)
	}
	renumberDeviceForDoctorTest(t, filepath.Join(worktreeParent, ".workarea-acquisitions", "store.json"), 1000)
	renumberDeviceForDoctorTest(t, filepath.Join(worktreeParent, ".workarea-acquisitions.identity.json"), 1000)

	before, err := workarea.DiagnoseAcquisitionStore(worktreeParent)
	if err != nil {
		t.Fatal(err)
	}
	if before.Healthy || !before.NeedsReanchor {
		t.Fatalf("pre-fix diagnosis = %+v, want needs-reanchor", before)
	}

	cmd := newDaemonDoctorCmd("donmai")
	buf := &bytes.Buffer{}
	cmd.SetOut(buf)
	cmd.SetErr(buf)
	cmd.SetArgs([]string{"--fix"})
	fixErr := cmd.Execute()
	if fixErr != nil && strings.Contains(fixErr.Error(), "re-anchor") {
		t.Fatalf("doctor --fix should clear the re-anchor condition; got: %v\noutput:\n%s", fixErr, buf.String())
	}

	out := buf.String()
	if !strings.Contains(out, "re-anchored") {
		t.Errorf("doctor --fix should print the re-anchored note; got:\n%s", out)
	}

	after, err := workarea.DiagnoseAcquisitionStore(worktreeParent)
	if err != nil {
		t.Fatal(err)
	}
	if !after.Healthy || after.NeedsReanchor {
		t.Fatalf("post-fix diagnosis = %+v, want healthy", after)
	}
	if _, err := workarea.NewAcquisitionStore(worktreeParent, nil); err != nil {
		t.Fatalf("store should open after doctor --fix: %v", err)
	}
}
