package vt

import (
	"bytes"
	"io"
	"testing"
	"time"
)

func TestCheckpointReceiverResponseWriter(t *testing.T) {
	producer := checkpointTerminal(t)
	var responses bytes.Buffer
	producer.SetResponseWriter(&responses)
	checkpointWrite(t, producer, "\x1b[?2048h\x1b[6n\x1b[c\x1b[?7$p\x1b]10;?\x07")
	if responses.Len() == 0 {
		t.Fatal("producer query responses were lost")
	}
	state := checkpointBytes(t, producer)
	restored := checkpointTerminal(t)
	restored.SetResponseWriter(io.Discard)
	if err := restored.RestoreCheckpoint(state); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = restored.WriteString("\x1b[6n\x1b[c\x1b[>c\x1b[?7$p\x1b]10;?\x07")
		restored.Resize(25, 9)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		// Unblock a regressed internal-pipe write without changing closed state.
		_ = restored.pw.CloseWithError(io.EOF)
		<-done
		t.Fatal("receiver output sink lost on restore; query/resize blocked")
	}
}

func TestCheckpointCloseReleasesBothOwnedPipes(t *testing.T) {
	e := NewEmulator(2, 2)
	e.SetResponseWriter(io.Discard)
	reader, writer := e.pr, e.pw
	if err := e.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := reader.Read(make([]byte, 1)); err != io.ErrClosedPipe {
		t.Fatalf("closed response reader=%v", err)
	}
	if _, err := writer.Write([]byte("reply")); err != io.ErrClosedPipe {
		t.Fatalf("closed response writer=%v", err)
	}
	if err := e.Close(); err != nil {
		t.Fatal("repeated Close failed")
	}
	if _, err := e.ExportCheckpoint(); err == nil {
		t.Fatal("closed engine exported live continuation")
	}
	if err := e.RestoreCheckpoint([]byte("{}")); err == nil {
		t.Fatal("closed engine was resurrected")
	}
}
