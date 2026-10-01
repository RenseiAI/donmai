package localqueue

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/RenseiAI/donmai/executioncell"
)

func TestHistoricalLocalTransportEvidenceRefusesWithoutRewrite(t *testing.T) {
	t.Parallel()
	root := filepath.Join(t.TempDir(), "queue")
	journal := filepath.Join(root, "transactions")
	if err := os.MkdirAll(journal, 0o700); err != nil {
		t.Fatal(err)
	}
	files, err := filepath.Glob("testdata/pre-transport-journal/*.json")
	if err != nil || len(files) != 2 {
		t.Fatalf("historical fixture: files=%d err=%v", len(files), err)
	}
	journalRoot, err := os.OpenRoot(journal)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = journalRoot.Close() })
	before := map[string][]byte{}
	for _, file := range files {
		raw, readErr := os.ReadFile(file)
		if readErr != nil {
			t.Fatal(readErr)
		}
		target := filepath.Base(file)
		if err = journalRoot.WriteFile(target, raw, 0o600); err != nil {
			t.Fatal(err)
		}
		before[target] = raw
	}
	store, err := Open(root, testBinding())
	if store != nil {
		_ = store.Close()
		t.Fatal("historical transport evidence reopened as executable")
	}
	if err == nil || !strings.Contains(err.Error(), "unsupported local runtime transport evidence") {
		t.Fatalf("missing incompatible-evidence diagnostic: %v", err)
	}
	after, err := filepath.Glob(filepath.Join(journal, "*.json"))
	if err != nil {
		t.Fatal(err)
	}
	if len(after) != len(before) {
		t.Fatal("replay changed historical journal length")
	}
	for file, raw := range before {
		got, readErr := journalRoot.ReadFile(file)
		if readErr != nil {
			t.Fatal(readErr)
		}
		if !bytes.Equal(raw, got) {
			t.Fatal("replay rewrote historical admission")
		}
	}
}

func TestLocalTransportEvidenceMarkerRefusesMutation(t *testing.T) {
	t.Parallel()
	for _, mode := range []string{"", "controller", "unknown"} {
		t.Run("mode_"+mode, func(t *testing.T) {
			t.Parallel()
			s, err := Open(filepath.Join(t.TempDir(), "queue"), testBinding())
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				if err := s.Close(); err != nil {
					t.Error(err)
				}
			})
			env := testEnvelope(t)
			var evidence map[string]json.RawMessage
			if err = json.Unmarshal(env.ProducerEvidence, &evidence); err != nil {
				t.Fatal(err)
			}
			var config map[string]any
			if err = json.Unmarshal(evidence["configuration"], &config); err != nil {
				t.Fatal(err)
			}
			config["RuntimeTransportMode"] = mode
			evidence["configuration"], err = executioncell.CanonicalJSON(config)
			if err != nil {
				t.Fatal(err)
			}
			env.ProducerEvidence, err = executioncell.CanonicalJSON(evidence)
			if err != nil {
				t.Fatal(err)
			}
			revision := s.CurrentRevision()
			if _, err = s.Admit(context.Background(), revision, env); err == nil || !strings.Contains(err.Error(), "unsupported local runtime transport evidence") {
				t.Fatalf("invalid transport admitted: %v", err)
			}
			if s.CurrentRevision() != revision {
				t.Fatal("refused evidence advanced journal")
			}
			if _, err = s.Admit(context.Background(), revision, testEnvelope(t)); err != nil {
				t.Fatal(err)
			}
		})
	}
}
