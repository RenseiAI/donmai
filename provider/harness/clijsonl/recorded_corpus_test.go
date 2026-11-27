package clijsonl

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/RenseiAI/donmai/agent"
)

// TestRecordedCorpusConformance maps every line of the recorded Claude Code
// stream-json corpus (scripts/record-claude-stream-json.py) and asserts the
// normalized-event invariants of the harness interaction contract: nothing
// fails to decode, no frame is unclassified, every tool result pairs with a
// prior call by id, every call gets a result, and each transcript ends on
// exactly one terminal result.
func TestRecordedCorpusConformance(t *testing.T) {
	t.Parallel()
	files, err := filepath.Glob(filepath.Join("testdata", "recorded", "*.jsonl"))
	if err != nil || len(files) == 0 {
		t.Fatalf("no recorded corpus: %v", err)
	}
	for _, file := range files {
		t.Run(filepath.Base(file), func(t *testing.T) {
			t.Parallel()
			f, err := os.Open(file)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = f.Close() })
			calls := map[string]bool{}
			results := map[string]bool{}
			var last agent.Event
			terminals := 0
			sc := bufio.NewScanner(f)
			sc.Buffer(make([]byte, 0, 1<<20), 1<<24)
			for sc.Scan() {
				line := sc.Bytes()
				var head struct {
					Type    string `json:"type"`
					Subtype string `json:"subtype"`
				}
				if err := json.Unmarshal(line, &head); err != nil {
					t.Fatalf("corpus line is not JSON: %v", err)
				}
				if head.Type == "system" && systemSubtypes[head.Subtype] == systemUnclassified {
					t.Errorf("system subtype %q is unclassified; add it to systemSubtypes", head.Subtype)
				}
				for _, ev := range mapLine(line) {
					switch e := ev.(type) {
					case agent.ErrorEvent:
						if strings.HasPrefix(e.Code, "decode_") {
							t.Errorf("decode failure on a recorded %s line: %s", head.Type, e.Message)
						}
					case agent.SystemEvent:
						if e.Subtype == "unknown" {
							t.Errorf("recorded %s line mapped to an unknown system event", head.Type)
						}
					case agent.ToolUseEvent:
						if e.ToolUseID == "" || e.ToolName == "" {
							t.Errorf("tool use without id or name: %+v", e)
						}
						calls[e.ToolUseID] = true
					case agent.ToolResultEvent:
						if !calls[e.ToolUseID] {
							t.Errorf("tool result %q has no prior call", e.ToolUseID)
						}
						results[e.ToolUseID] = true
					case agent.ResultEvent:
						terminals++
					}
					last = ev
				}
			}
			if err := sc.Err(); err != nil {
				t.Fatal(err)
			}
			for id := range calls {
				if !results[id] {
					t.Errorf("tool call %q never got a result", id)
				}
			}
			if _, ok := last.(agent.ResultEvent); !ok || terminals != 1 {
				t.Errorf("transcript ends on %T with %d terminal results; want exactly one, last", last, terminals)
			}
		})
	}
}

// TestRecordedCorpusProvenance pins the corpus to one recorded CLI version.
func TestRecordedCorpusProvenance(t *testing.T) {
	t.Parallel()
	raw, err := os.ReadFile(filepath.Join("testdata", "recorded", "RECORDING.json"))
	if err != nil {
		t.Fatal(err)
	}
	var rec struct {
		CLIVersion string   `json:"cliVersion"`
		Scenarios  []string `json:"scenarios"`
	}
	if err := json.Unmarshal(raw, &rec); err != nil {
		t.Fatal(err)
	}
	if rec.CLIVersion == "" || len(rec.Scenarios) == 0 {
		t.Fatalf("recording provenance incomplete: %+v", rec)
	}
	for _, name := range rec.Scenarios {
		if _, err := os.Stat(filepath.Join("testdata", "recorded", name+".jsonl")); err != nil {
			t.Errorf("scenario %s listed but not recorded: %v", name, err)
		}
	}
}

func TestLivenessSystemFramesAreNotRows(t *testing.T) {
	t.Parallel()
	for _, sub := range []string{"status", "thinking_tokens"} {
		events := mapLine([]byte(`{"type":"system","subtype":"` + sub + `","status":"requesting","session_id":"s"}`))
		if len(events) != 1 {
			t.Fatalf("%s mapped to %d events", sub, len(events))
		}
		if se, ok := events[0].(agent.SystemEvent); !ok || se.Subtype != agent.SystemSubtypeStreamProgress {
			t.Errorf("%s = %#v; want the stream-progress liveness signal", sub, events[0])
		}
	}
}
