package ptyhost

import (
	"bytes"
	"testing"
	"time"

	"github.com/RenseiAI/donmai/attachwire"
)

// TestLocalSingleDriverPolicy: the first live local attach is the driver; a
// concurrent second attach is read-only; closing the driver frees the pen for a
// later attach (§11.1 standalone single-local-driver policy).
func TestLocalSingleDriverPolicy(t *testing.T) {
	s := mustSpawn(t, Spec{Command: []string{"sleep", "30"}})

	d1, err := s.AttachLocal(LocalAttachOptions{})
	if err != nil {
		t.Fatalf("AttachLocal #1: %v", err)
	}
	if !d1.CanDrive() {
		t.Fatal("first attach should be the driver")
	}
	if d1.UserID() != LocalUserID {
		t.Errorf("driver userID = %q, want %q", d1.UserID(), LocalUserID)
	}

	d2, err := s.AttachLocal(LocalAttachOptions{})
	if err != nil {
		t.Fatalf("AttachLocal #2: %v", err)
	}
	if d2.CanDrive() {
		t.Error("second concurrent attach should be read-only")
	}
	if _, err := d2.WriteInput([]byte("x")); err != ErrLocalReadOnly {
		t.Errorf("read-only WriteInput err = %v, want ErrLocalReadOnly", err)
	}
	if err := d2.Resize(80, 24, 0, 0); err != ErrLocalReadOnly {
		t.Errorf("read-only Resize err = %v, want ErrLocalReadOnly", err)
	}

	// Driver may write.
	if _, err := d1.WriteInput([]byte("")); err != nil {
		t.Errorf("driver WriteInput err = %v", err)
	}

	// Closing the driver frees the pen.
	_ = d1.Close()
	_ = d2.Close()
	d3, err := s.AttachLocal(LocalAttachOptions{})
	if err != nil {
		t.Fatalf("AttachLocal #3: %v", err)
	}
	defer func() { _ = d3.Close() }()
	if !d3.CanDrive() {
		t.Error("attach after driver close should become the new driver")
	}
}

// TestLocalSanitizerApplied: an OSC 52 (clipboard) sequence emitted by the child
// is stripped from the local viewer feed (§9 defense-in-depth) but present on the
// raw host subscription (the host→relay leg carries raw bytes, §3.1).
func TestLocalSanitizerApplied(t *testing.T) {
	// Prints: A, OSC-52 clipboard-set (hostile), B.
	child := `printf 'A\033]52;c;SGVsbG8=\007B'; sleep 2`
	s := mustSpawn(t, Spec{Command: []string{"sh", "-c", child}})

	raw, err := s.Subscribe(0)
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	defer func() { _ = raw.Close() }()

	local, err := s.AttachLocal(LocalAttachOptions{FromSeq: 0})
	if err != nil {
		t.Fatalf("AttachLocal: %v", err)
	}
	defer func() { _ = local.Close() }()

	rawOut := collectOutput(raw.Frames(), 1500*time.Millisecond)
	localOut := collectOutput(local.Frames(), 1500*time.Millisecond)

	osc52 := []byte("\x1b]52;")
	if !bytes.Contains(rawOut, osc52) {
		t.Errorf("raw host subscription should carry the OSC 52 bytes, got %q", rawOut)
	}
	if bytes.Contains(localOut, osc52) {
		t.Errorf("local viewer feed must NOT carry OSC 52 (sanitizer failed), got %q", localOut)
	}
	if !bytes.Contains(localOut, []byte("A")) || !bytes.Contains(localOut, []byte("B")) {
		t.Errorf("local viewer feed lost the visible text, got %q", localOut)
	}
}

// TestLocalSnapshotPassthrough: a local attach can read the current screen.
func TestLocalSnapshotPassthrough(t *testing.T) {
	s := mustSpawn(t, Spec{Command: []string{"sh", "-c", "printf hi; sleep 2"}})
	la, err := s.AttachLocal(LocalAttachOptions{})
	if err != nil {
		t.Fatalf("AttachLocal: %v", err)
	}
	defer func() { _ = la.Close() }()
	scr, _, err := la.Snapshot()
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	if scr.Cols == 0 || scr.Rows == 0 {
		t.Errorf("snapshot geometry = %dx%d, want non-zero", scr.Cols, scr.Rows)
	}
	if _, err := scr.Encode(); err != nil {
		t.Errorf("snapshot not encodable: %v", err)
	}
	_ = attachwire.BufferPrimary
}
