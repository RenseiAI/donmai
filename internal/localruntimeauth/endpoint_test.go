package localruntimeauth

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestEndpointPublishesOnlyCanonicalLoopbackAndReopens(t *testing.T) {
	store, root, queueRoot := newTestStore(t)
	for _, origin := range []string{
		"", "https://127.0.0.1:7734", "http://localhost:7734", "http://0.0.0.0:7734",
		"http://192.0.2.1:7734", "http://127.0.0.1", "http://127.0.0.1:0",
		"http://127.0.0.1:65536", "http://user@127.0.0.1:7734",
		"http://127.0.0.1:7734/api", "http://127.0.0.1:7734/",
		"http://127.0.0.1:7734?token=synthetic", "http://127.0.0.1:7734#fragment",
	} {
		if _, err := store.PublishEndpoint(context.Background(), origin, "daemon-one"); !errors.Is(err, ErrInvalidInput) {
			t.Fatalf("invalid origin accepted: %v", err)
		}
	}
	if _, err := store.Endpoint(); !errors.Is(err, ErrMissingEndpoint) {
		t.Fatalf("invalid origins created endpoint: %v", err)
	}
	store.fault = func(stage string) error {
		if stage == "before_rename" {
			return errors.New("injected endpoint pre-publication interruption")
		}
		return nil
	}
	if _, err := store.PublishEndpoint(context.Background(), "http://127.0.0.1:7734", "daemon-one"); err == nil {
		t.Fatal("interrupted endpoint write returned publication")
	}
	if _, err := store.Endpoint(); !errors.Is(err, ErrMissingEndpoint) {
		t.Fatalf("interrupted endpoint write left publication: %v", err)
	}
	store.fault = nil
	first, err := store.PublishEndpoint(context.Background(), "http://127.0.0.1:7734", "daemon-one")
	if err != nil || first.ScopeID != store.Identity().ScopeID || first.HostID != store.Identity().HostID ||
		first.WorkerID != store.Identity().WorkerID || !validDigest(first.QueueRootSHA256) {
		t.Fatalf("published endpoint identity invalid: %+v, %v", first, err)
	}
	if repeat, err := store.PublishEndpoint(context.Background(), first.Origin, first.InstanceID); err != nil || repeat != first {
		t.Fatalf("exact endpoint retry = %+v, %v", repeat, err)
	}
	second, err := store.PublishEndpoint(context.Background(), "http://[::1]:7744", "daemon-two")
	if err != nil || second.Origin != "http://[::1]:7744" || second.InstanceID != "daemon-two" {
		t.Fatalf("IPv6 loopback replacement = %+v, %v", second, err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := OpenExisting(root, queueRoot)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = reopened.Close() }()
	got, err := reopened.Endpoint()
	if err != nil || got != second {
		t.Fatalf("endpoint restart readback = %+v, %v", got, err)
	}
	if _, err := reopened.PublishEndpoint(context.Background(), first.Origin, "daemon-three"); !errors.Is(err, ErrReadOnly) {
		t.Fatalf("read-only endpoint replacement = %v", err)
	}
}

func TestEndpointBindingCorruptionAndFileRefusal(t *testing.T) {
	for _, kind := range []string{"wrong-queue", "wrong-host", "world-readable", "symlink", "unknown-field"} {
		t.Run(kind, func(t *testing.T) {
			store, root, _ := newTestStore(t)
			if _, err := store.PublishEndpoint(context.Background(), "http://127.0.0.1:7734", "daemon-one"); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(root, endpointFileName)
			raw, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			switch kind {
			case "wrong-queue", "wrong-host":
				var value Endpoint
				if err := json.Unmarshal(raw, &value); err != nil {
					t.Fatal(err)
				}
				if kind == "wrong-queue" {
					value.QueueRootSHA256 = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
				} else {
					value.HostID = "other-host"
				}
				raw, err = json.Marshal(value)
				if err == nil {
					replaceFixtureFile(t, path, raw)
				}
			case "world-readable":
				setFixtureMode(t, path, 0o644)
			case "symlink":
				outside := filepath.Join(t.TempDir(), "synthetic-endpoint")
				replaceFixtureFile(t, outside, raw)
				err = os.Remove(path)
				if err == nil {
					err = os.Symlink(outside, path)
				}
			case "unknown-field":
				modified := append([]byte(nil), raw[:len(raw)-1]...)
				modified = append(modified, []byte(`,"extra":true}`)...)
				replaceFixtureFile(t, path, modified)
			}
			if err != nil {
				t.Fatal(err)
			}
			if _, err := store.Endpoint(); err == nil {
				t.Fatal("corrupt endpoint was accepted")
			}
			if _, err := store.PublishEndpoint(context.Background(), "http://127.0.0.1:7735", "daemon-two"); err == nil {
				t.Fatal("endpoint publication overwrote mismatched or corrupt authority")
			}
		})
	}
}

func TestEndpointAmbiguousDirectorySyncRequiresReaffirmedReadback(t *testing.T) {
	store, root, queueRoot := newTestStore(t)
	store.dirSync = func(root *os.Root) error {
		if _, err := root.Lstat(endpointFileName); err == nil {
			return errors.New("injected endpoint directory sync after rename")
		}
		return nil
	}
	if _, err := store.PublishEndpoint(context.Background(), "http://127.0.0.1:7734", "daemon-one"); !errors.Is(err, ErrAmbiguousCommit) {
		t.Fatalf("post-rename failure = %v, want ambiguous", err)
	}
	if _, err := store.Endpoint(); !errors.Is(err, ErrAmbiguousCommit) {
		t.Fatalf("poisoned endpoint read = %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := OpenExisting(root, queueRoot)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = reopened.Close() }()
	confirmed, err := reopened.Endpoint()
	if err != nil || confirmed.Origin != "http://127.0.0.1:7734" || confirmed.InstanceID != "daemon-one" {
		t.Fatalf("endpoint reaffirmation = %+v, %v", confirmed, err)
	}
}
