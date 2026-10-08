package claude

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/RenseiAI/donmai/agent"
	"github.com/RenseiAI/donmai/attachwire"
	"github.com/RenseiAI/donmai/attachwire/sanitize"
)

// fakeClaudeIdleTitle stands in for the real CLI's terminal handling: it draws
// the prompt glyph and parks the cursor after it, then sets the terminal title
// the way the real REPL does after a turn ends: OSC 0 with "✳ " (UTF-8
// E2 9C B3) followed by the --name value.
const fakeClaudeIdleTitle = `
name=""
while (($#)); do
  if [[ "$1" == "--name" ]]; then name="$2"; shift 2; continue; fi
  shift
done
printf '\342\235\257 '
printf '\033]0;\342\234\263 %s\007' "$name"
sleep 1
`

// TestSpawn_Interactive_SessionNameTitleNeverReachesTheViewerScreen drives the
// interactive PTY path end to end with a fake claude binary. The session name
// reaches the CLI as --name, the CLI puts it in its terminal title, and the
// title must stay out of everything a person sees: the recorded cast and the
// sanitized viewer stream. Before the sanitizer handled UTF-8 inside string
// bodies, the 0x9C byte inside "✳" ended the title early and
// " website-refactor" was rendered at the cursor, inside the prompt input.
func TestSpawn_Interactive_SessionNameTitleNeverReachesTheViewerScreen(t *testing.T) {
	t.Parallel()
	const name = "website-refactor"
	p := newFakeInteractiveProvider(t, fakeClaudeIdleTitle)
	castPath := filepath.Join(t.TempDir(), "term.cast")

	h, err := p.Spawn(context.Background(), agent.Spec{
		SessionName: name,
		Cwd:         t.TempDir(),
		Interactive: &agent.InteractiveSpec{Cols: 80, Rows: 24, RecordPath: castPath},
	})
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	t.Cleanup(func() { _ = h.Stop(context.Background()) })

	ic, ok := h.(agent.InteractiveCapable)
	if !ok {
		t.Fatal("handle does not implement agent.InteractiveCapable")
	}
	sess := ic.InteractiveSession()
	sub, err := sess.Subscribe(0)
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	t.Cleanup(func() { _ = sub.Close() })

	// Raw host leg: read until the title has arrived in full.
	wantTitle := []byte("\x1b]0;\xe2\x9c\xb3 " + name + "\x07")
	var raw []byte
	deadline := time.After(15 * time.Second)
	for !bytes.Contains(raw, wantTitle) {
		select {
		case f, ok := <-sub.Frames():
			if !ok {
				t.Fatalf("subscription closed before the title arrived; raw=%q", raw)
			}
			if f.Type == attachwire.TypeOutput {
				raw = append(raw, attachwire.DecodeOutput(f.Payload).Data...)
			}
		case <-deadline:
			t.Fatalf("timed out waiting for the --name title on the raw leg; raw=%q", raw)
		}
	}

	// Viewer leg: what a web or terminal viewer renders after the §9 sanitizer.
	view := sanitize.New().Write(raw)
	if bytes.Contains(view, []byte(name)) {
		t.Errorf("viewer stream renders the session name as text: %q", view)
	}
	if !bytes.Contains(view, []byte("\xe2\x9d\xaf ")) {
		t.Errorf("viewer stream lost the prompt: %q", view)
	}

	select {
	case <-sess.Done():
	case <-time.After(15 * time.Second):
		t.Fatal("timed out waiting for the fake claude to exit")
	}
	_ = h.Stop(context.Background())

	// Recorded cast: the "o" events are sanitized on the way to disk.
	cast := castOutput(t, castPath)
	if bytes.Contains(cast, []byte(name)) {
		t.Errorf("cast renders the session name as text: %q", cast)
	}
	if !bytes.Contains(cast, []byte("\xe2\x9d\xaf ")) {
		t.Errorf("cast lost the prompt: %q", cast)
	}
}

// castOutput concatenates the "o" event payloads of an asciicast v2 file,
// polling briefly while the recorder flushes after the child exits.
func castOutput(t *testing.T, path string) []byte {
	t.Helper()
	var out []byte
	for range 50 {
		out = out[:0]
		data, err := os.ReadFile(path) //nolint:gosec // test-owned temp path
		if err == nil {
			lines := bytes.Split(bytes.TrimRight(data, "\n"), []byte("\n"))
			for _, ln := range lines[1:] {
				var ev []json.RawMessage
				if json.Unmarshal(ln, &ev) != nil || len(ev) != 3 {
					continue
				}
				var code, chunk string
				if json.Unmarshal(ev[1], &code) != nil || code != "o" || json.Unmarshal(ev[2], &chunk) != nil {
					continue
				}
				out = append(out, chunk...)
			}
			if bytes.Contains(out, []byte("\xe2\x9d\xaf ")) {
				return out
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	return out
}
