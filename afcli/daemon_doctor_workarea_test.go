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

// TestDaemonDoctorJSONIncludesWorkareaStore pins the AC-3 JSON shape: the
// doctor report must carry workareaStore.present/healthy/storeId/
// needsReanchor/reason, true needsReanchor before repair and healthy after
// --fix (with the workareaRepair note). Dropping the workareaStore key must
// turn this test RED.
func TestDaemonDoctorJSONIncludesWorkareaStore(t *testing.T) {
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
	storeID := workareaStoreIDForDoctorJSONTest(t, worktreeParent)
	renumberDeviceForDoctorTest(t, filepath.Join(worktreeParent, ".workarea-acquisitions", "store.json"), 1000)
	renumberDeviceForDoctorTest(t, filepath.Join(worktreeParent, ".workarea-acquisitions.identity.json"), 1000)

	beforeCmd := newDaemonDoctorCmd("donmai")
	beforeOut := &bytes.Buffer{}
	beforeErr := &bytes.Buffer{}
	beforeCmd.SetOut(beforeOut)
	beforeCmd.SetErr(beforeErr)
	beforeCmd.SetArgs([]string{"--json"})
	_ = beforeCmd.Execute()
	var beforeDoc map[string]any
	if err := json.Unmarshal(beforeOut.Bytes(), &beforeDoc); err != nil {
		t.Fatalf("expected --json to emit valid JSON; got: %v\noutput:\n%s\nstderr:\n%s", err, beforeOut.String(), beforeErr.String())
	}
	beforeStore, ok := beforeDoc["workareaStore"].(map[string]any)
	if !ok {
		t.Fatalf("expected JSON to include workareaStore; got keys: %v", jsonKeys(beforeDoc))
	}
	if beforeStore["present"] != true {
		t.Errorf("workareaStore.present = %v, want true", beforeStore["present"])
	}
	if beforeStore["healthy"] != false {
		t.Errorf("workareaStore.healthy = %v, want false before --fix", beforeStore["healthy"])
	}
	if beforeStore["needsReanchor"] != true {
		t.Errorf("workareaStore.needsReanchor = %v, want true before --fix", beforeStore["needsReanchor"])
	}
	if beforeStore["storeId"] != storeID {
		t.Errorf("workareaStore.storeId = %v, want %q", beforeStore["storeId"], storeID)
	}
	if _, ok := beforeStore["reason"].(string); !ok {
		t.Errorf("workareaStore.reason should be a string; got: %v", beforeStore)
	}

	fixCmd := newDaemonDoctorCmd("donmai")
	fixOut := &bytes.Buffer{}
	fixErr := &bytes.Buffer{}
	fixCmd.SetOut(fixOut)
	fixCmd.SetErr(fixErr)
	fixCmd.SetArgs([]string{"--json", "--fix"})
	_ = fixCmd.Execute()
	var afterDoc map[string]any
	if err := json.Unmarshal(fixOut.Bytes(), &afterDoc); err != nil {
		t.Fatalf("expected --json --fix to emit valid JSON; got: %v\noutput:\n%s\nstderr:\n%s", err, fixOut.String(), fixErr.String())
	}
	afterStore, ok := afterDoc["workareaStore"].(map[string]any)
	if !ok {
		t.Fatalf("expected --fix JSON to include workareaStore; got keys: %v", jsonKeys(afterDoc))
	}
	if afterStore["needsReanchor"] != false {
		t.Errorf("workareaStore.needsReanchor = %v, want false after --fix", afterStore["needsReanchor"])
	}
	if afterStore["healthy"] != true {
		t.Errorf("workareaStore.healthy = %v, want true after --fix", afterStore["healthy"])
	}
	repairNote, ok := afterDoc["workareaRepair"].(string)
	if !ok || repairNote == "" {
		t.Errorf("expected --fix JSON to include a non-empty workareaRepair note; got: %v", jsonKeys(afterDoc))
	}
}

func workareaStoreIDForDoctorJSONTest(t *testing.T, worktreeParent string) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(worktreeParent, ".workarea-acquisitions", "store.json")) //nolint:gosec // test-only TempDir path
	if err != nil {
		t.Fatal(err)
	}
	var doc doctorWorkareaStoreDoc
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	return doc.StoreID
}

func jsonKeys(doc map[string]any) []string {
	keys := make([]string, 0, len(doc))
	for key := range doc {
		keys = append(keys, key)
	}
	return keys
}
