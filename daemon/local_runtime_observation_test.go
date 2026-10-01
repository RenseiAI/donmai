package daemon

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"testing"
)

func TestLocalObservationReplayRejectsUnsafePublishedFile(t *testing.T) {
	t.Parallel()
	// Mode is test-authored fault data, applied only to an owned temp file.
	for _, fixture := range []struct {
		name string
		mode os.FileMode
	}{{"public-mode", 0o644}, {"symlink", 0o600}} {
		t.Run(fixture.name, func(t *testing.T) {
			t.Parallel()
			f := startLocalHTTPFixture(t)
			session := f.admit(t, 9)
			token := f.claim(t, session.Session)
			raw := []byte(fmt.Sprintf(`{"workerId":%q,"activity":{"type":"assistant","content":"private fixture"}}`, f.runtime.identity.WorkerID))
			route := basePath(session.Session.SessionID, "activity")
			code, body := f.request(t, http.MethodPost, route, token, raw)
			if code != http.StatusOK {
				t.Fatalf("initial observation %d: %s", code, body)
			}
			projection, err := f.runtime.store.Session(context.Background(), f.runtime.identity.ScopeID, session.Session.SessionID)
			if err != nil {
				t.Fatal(err)
			}
			root := f.runtime.options.QueueRoot + ".observations"
			name := localDigest([]byte(projection.Attempt.AttemptID+"\x00activity\x00"+localDigest(raw))) + ".json"
			path := filepath.Join(root, name)
			switch fixture.name {
			case "public-mode":
				err = os.Chmod(path, fixture.mode)
			case "symlink":
				target := filepath.Join(root, "owned-target")
				if err = os.Rename(path, target); err == nil {
					err = os.Symlink("owned-target", path)
				}
			}
			if err != nil {
				t.Fatal(err)
			}
			code, _ = f.request(t, http.MethodPost, route, token, raw)
			if code != http.StatusConflict {
				t.Fatalf("unsafe %s observation acknowledged: %d", fixture.name, code)
			}
		})
	}
}

func TestLocalObservationRetryReaffirmsDirectoryDurability(t *testing.T) {
	t.Parallel()
	for _, phase := range []string{"parent", "publication"} {
		t.Run(phase, func(t *testing.T) {
			t.Parallel()
			f := startLocalHTTPFixture(t)
			session := f.admit(t, 10)
			token := f.claim(t, session.Session)
			raw := []byte(fmt.Sprintf(`{"workerId":%q,"activity":{"type":"assistant","content":"durability fixture"}}`, f.runtime.identity.WorkerID))
			path := f.runtime.options.QueueRoot + ".observations"
			revision := f.runtime.store.CurrentRevision()
			f.runtime.observationSync = func(root *os.Root) error {
				target := path
				if phase == "parent" {
					target = filepath.Dir(path)
				}
				if filepath.Clean(root.Name()) == filepath.Clean(target) {
					return errors.New("owned directory fsync fault")
				}
				return syncLocalDirectory(root)
			}
			for range 2 {
				code, _ := f.request(t, http.MethodPost, basePath(session.Session.SessionID, "activity"), token, raw)
				if code != http.StatusConflict {
					t.Fatalf("%s fsync ambiguity acknowledged: %d", phase, code)
				}
			}
			if f.runtime.store.CurrentRevision() != revision {
				t.Fatal("failed ancillary observation mutated authoritative journal")
			}
			f.runtime.observationSync = nil
			code, body := f.request(t, http.MethodPost, basePath(session.Session.SessionID, "activity"), token, raw)
			if code != http.StatusOK {
				t.Fatalf("durable retry %d: %s", code, body)
			}
		})
	}
}
