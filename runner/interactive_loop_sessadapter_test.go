package runner

import (
	"testing"

	"github.com/RenseiAI/donmai/agent"
)

// fakeAttributedSession implements the OPTIONAL systemAttributedWriter
// capability attachclient/session.go defines (structurally — this package
// never imports attachclient's private type). It embeds a nil
// agent.InteractiveSession because sessAdapter.WriteAttributedInput's
// forwarding branch is the only method this test exercises.
type fakeAttributedSession struct {
	agent.InteractiveSession
	gotUserID []byte
	gotData   []byte
}

func (f *fakeAttributedSession) WriteAttributedInput(userID, p []byte) (int, error) {
	f.gotUserID = append([]byte(nil), userID...)
	f.gotData = append([]byte(nil), p...)
	return len(p), nil
}

// TestSessAdapterWriteAttributedInput pins the forwarding shim described on
// sessAdapter.WriteAttributedInput: embedding agent.InteractiveSession only
// promotes methods THAT interface declares, so without the explicit
// forwarder attachclient's optional-capability type assertion would always
// miss even when the real session underneath supports it.
func TestSessAdapterWriteAttributedInput(t *testing.T) {
	t.Run("forwards to the underlying session when it implements the capability", func(t *testing.T) {
		fake := &fakeAttributedSession{}
		adapter := sessAdapter{fake}

		n, err := adapter.WriteAttributedInput([]byte("system:pty-nudge"), []byte("\r"))
		if err != nil {
			t.Fatalf("WriteAttributedInput: %v", err)
		}
		if n != 1 {
			t.Errorf("n = %d, want 1", n)
		}
		if string(fake.gotUserID) != "system:pty-nudge" {
			t.Errorf("userID forwarded = %q, want %q", fake.gotUserID, "system:pty-nudge")
		}
		if string(fake.gotData) != "\r" {
			t.Errorf("data forwarded = %q, want %q", fake.gotData, "\r")
		}
	})

	t.Run("falls back to plain WriteInput when the underlying session lacks the capability", func(t *testing.T) {
		rec := &recordingInteractiveSession{}
		adapter := sessAdapter{rec}

		n, err := adapter.WriteAttributedInput([]byte("system:pty-nudge"), []byte("hello"))
		if err != nil {
			t.Fatalf("WriteAttributedInput: %v", err)
		}
		if n != 5 {
			t.Errorf("n = %d, want 5", n)
		}
		writes := rec.recordedWrites()
		if len(writes) != 1 || string(writes[0]) != "hello" {
			t.Fatalf("recordedWrites = %q, want [\"hello\"] via the WriteInput fallback", writes)
		}
	})
}
