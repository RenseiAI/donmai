package localruntimeauth

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func newTestStore(t *testing.T) (*Store, string, string) {
	t.Helper()
	parent := t.TempDir()
	root := filepath.Join(parent, "auth")
	queueRoot := filepath.Join(parent, "queue")
	store, err := Bootstrap(root, queueRoot)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store, root, queueRoot
}

func TestBootstrapReopenAndReadOnlyIdentity(t *testing.T) {
	store, root, queueRoot := newTestStore(t)
	identity := store.Identity()
	if !validAuthority(identity) {
		t.Fatal("minted identity invalid")
	}
	operator, err := store.OperatorCredential()
	if err != nil || !validToken(operator.BearerToken(), operatorTokenPrefix) {
		t.Fatalf("operator credential validity: %v", err)
	}
	if err := store.VerifyOperator(operator.BearerToken()); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := OpenExisting(root, queueRoot)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = reopened.Close() }()
	got, err := reopened.OperatorCredential()
	if err != nil || !equalSecret(got.BearerToken(), operator.BearerToken()) || reopened.Identity() != identity {
		t.Fatalf("reopen changed authority or operator credential: %v", err)
	}
	if _, err := reopened.PublishEndpoint(context.Background(), "http://127.0.0.1:7734", "daemon-one"); !errors.Is(err, ErrReadOnly) {
		t.Fatalf("read-only endpoint publication = %v", err)
	}
	if _, err := Bootstrap(root, filepath.Join(filepath.Dir(queueRoot), "other-queue")); !errors.Is(err, ErrBindingMismatch) {
		t.Fatalf("queue-root mismatch = %v", err)
	}
	if _, err := OpenExisting(root, filepath.Join(filepath.Dir(queueRoot), "other-queue")); !errors.Is(err, ErrBindingMismatch) {
		t.Fatalf("read-only queue-root mismatch = %v", err)
	}
}

func TestBootstrapConcurrentFirstCreateLoadsOneWinner(t *testing.T) {
	parent := t.TempDir()
	root := filepath.Join(parent, "auth")
	queueRoot := filepath.Join(parent, "queue")
	type result struct {
		identity string
		token    string
		err      error
	}
	results := make(chan result, 8)
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			store, err := Bootstrap(root, queueRoot)
			if err != nil {
				results <- result{err: err}
				return
			}
			credential, err := store.OperatorCredential()
			results <- result{identity: store.Identity().ScopeID + "/" + store.Identity().HostID + "/" + store.Identity().WorkerID, token: credential.BearerToken(), err: err}
			_ = store.Close()
		}()
	}
	wg.Wait()
	close(results)
	var first result
	for got := range results {
		if got.err != nil {
			t.Fatalf("concurrent bootstrap: %v", got.err)
		}
		if first.identity == "" {
			first = got
		} else if got.identity != first.identity || !equalSecret(got.token, first.token) {
			t.Fatal("concurrent bootstrap returned different authority or credential")
		}
	}
}

func TestOpenExistingNeverCreatesMissingOrPartialState(t *testing.T) {
	parent := t.TempDir()
	root := filepath.Join(parent, "auth")
	queueRoot := filepath.Join(parent, "queue")
	if _, err := OpenExisting(root, queueRoot); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("missing read-only root = %v", err)
	}
	if _, err := os.Lstat(root); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("read-only open created root: %v", err)
	}
	if err := os.Mkdir(root, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenExisting(root, queueRoot); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("empty read-only root = %v", err)
	}
	if err := os.Mkdir(filepath.Join(root, attemptsDirectory), 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenExisting(root, queueRoot); !errors.Is(err, ErrCorruptState) {
		t.Fatalf("partial read-only bootstrap = %v", err)
	}
	if _, err := Bootstrap(root, queueRoot); !errors.Is(err, ErrCorruptState) {
		t.Fatalf("partial prior attempt state minted a new authority: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(root, bootstrapFileName)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("partial state gained a bootstrap credential: %v", err)
	}
}

func TestExistingWriterLockWithoutIdentityRequiresRecovery(t *testing.T) {
	parent := t.TempDir()
	root := filepath.Join(parent, "auth")
	queueRoot := filepath.Join(parent, "queue")
	if err := os.Mkdir(root, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, writerLockFileName), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenExisting(root, queueRoot); !errors.Is(err, ErrCorruptState) {
		t.Fatalf("lock-only read-only open = %v", err)
	}
	if _, err := os.Lstat(filepath.Join(root, bootstrapFileName)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("lock-only read-only open minted authority: %v", err)
	}
	if err := os.Mkdir(queueRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := Bootstrap(root, queueRoot); !errors.Is(err, ErrCorruptState) {
		t.Fatalf("existing lock-only bootstrap minted a replacement: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(root, bootstrapFileName)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("existing lock-only bootstrap minted identity: %v", err)
	}
}

func TestMissingBootstrapAfterAttemptsNeverRotatesOperator(t *testing.T) {
	store, root, queueRoot := newTestStore(t)
	ref := testAttemptRef(store)
	if _, err := store.CreateAttempt(context.Background(), "HandleOne", ref); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(root, bootstrapFileName)); err != nil {
		t.Fatal(err)
	}
	if err := store.VerifyOperator("incorrect"); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("missing bootstrap still verified an operator: %v", err)
	}
	if _, err := store.OperatorCredential(); !errors.Is(err, ErrCorruptState) {
		t.Fatalf("missing bootstrap returned cached operator: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := Bootstrap(root, queueRoot); !errors.Is(err, ErrCorruptState) {
		t.Fatalf("missing bootstrap with attempts minted replacement: %v", err)
	}
}

func TestMissingBootstrapWithPriorQueueStateCannotMintIdentity(t *testing.T) {
	store, root, queueRoot := newTestStore(t)
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(root, bootstrapFileName)); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(queueRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(queueRoot, "transactions"), 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := Bootstrap(root, queueRoot); !errors.Is(err, ErrCorruptState) {
		t.Fatalf("missing bootstrap amid prior queue state minted an identity: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(root, bootstrapFileName)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("missing bootstrap was replaced: %v", err)
	}
}

func TestMissingBootstrapAfterUnusedInitializationCannotMintIdentity(t *testing.T) {
	store, root, queueRoot := newTestStore(t)
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(root, bootstrapFileName)); err != nil {
		t.Fatal(err)
	}
	if _, err := Bootstrap(root, queueRoot); !errors.Is(err, ErrCorruptState) {
		t.Fatalf("lost unused bootstrap minted replacement identity: %v", err)
	}
	if _, err := OpenExisting(root, queueRoot); !errors.Is(err, ErrCorruptState) {
		t.Fatalf("lost unused bootstrap read-only open = %v", err)
	}
}

func TestMissingInitializationMarkerRefusesExistingBootstrap(t *testing.T) {
	store, root, queueRoot := newTestStore(t)
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(root, initializedFileName)); err != nil {
		t.Fatal(err)
	}
	if _, err := Bootstrap(root, queueRoot); !errors.Is(err, ErrCorruptState) {
		t.Fatalf("bootstrap without durable initialization marker = %v", err)
	}
}

func TestBootstrapRejectsRootAndRecordCorruption(t *testing.T) {
	t.Run("world-readable root", func(t *testing.T) {
		store, root, queueRoot := newTestStore(t)
		_ = store.Close()
		setFixtureMode(t, root, 0o755)
		if _, err := OpenExisting(root, queueRoot); !errors.Is(err, ErrCorruptState) {
			t.Fatalf("world-readable auth root = %v", err)
		}
	})
	t.Run("symlink root", func(t *testing.T) {
		parent := t.TempDir()
		root := filepath.Join(parent, "auth-link")
		if err := os.Symlink(t.TempDir(), root); err != nil {
			t.Fatal(err)
		}
		if _, err := Bootstrap(root, filepath.Join(parent, "queue")); !errors.Is(err, ErrCorruptState) {
			t.Fatalf("symlink root = %v", err)
		}
	})
	for _, kind := range []string{"world-readable", "symlink", "hardlink", "unknown-field", "duplicate-field", "wrong-version", "oversized"} {
		t.Run(kind, func(t *testing.T) {
			store, root, queueRoot := newTestStore(t)
			if err := store.Close(); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(root, bootstrapFileName)
			original, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			switch kind {
			case "world-readable":
				setFixtureMode(t, path, 0o644)
			case "symlink":
				outside := filepath.Join(t.TempDir(), "outside")
				replaceFixtureFile(t, outside, original)
				err = os.Remove(path)
				if err == nil {
					err = os.Symlink(outside, path)
				}
			case "hardlink":
				err = os.Link(path, filepath.Join(root, "second-link"))
			case "unknown-field":
				replaceFixtureFile(t, path, append(bytes.TrimSuffix(original, []byte("}")), []byte(`,"extra":true}`)...))
			case "duplicate-field":
				replaceFixtureFile(t, path, append(bytes.TrimSuffix(original, []byte("}")), []byte(`,"version":1}`)...))
			case "wrong-version":
				replaceFixtureFile(t, path, bytes.Replace(original, []byte(`"version":1`), []byte(`"version":9`), 1))
			case "oversized":
				replaceFixtureFile(t, path, []byte(strings.Repeat("x", maxProtectedBytes+1)))
			}
			if err != nil {
				t.Fatal(err)
			}
			if _, err := OpenExisting(root, queueRoot); !errors.Is(err, ErrCorruptState) {
				t.Fatalf("%s protected bootstrap = %v", kind, err)
			}
			if _, err := Bootstrap(root, queueRoot); !errors.Is(err, ErrCorruptState) {
				t.Fatalf("%s bootstrap recovery = %v", kind, err)
			}
		})
	}
}

func TestCredentialFormattingNeverIncludesBearer(t *testing.T) {
	store, _, _ := newTestStore(t)
	credential, err := store.OperatorCredential()
	if err != nil {
		t.Fatal(err)
	}
	formatted := []struct {
		name  string
		value string
	}{
		{"credential-v", fmt.Sprintf("%v", credential)},
		{"credential-plus", fmt.Sprintf("%+v", credential)},
		{"credential-sharp", fmt.Sprintf("%#v", credential)},
		{"credential-q", fmt.Sprintf("%q", credential)},
		{"credential-s", fmt.Sprintf("%s", credential)},
		{"credential-x", fmt.Sprintf("%x", credential)},
		{"credential-p", fmt.Sprintf("%p", credential)},
		{"store-v", fmt.Sprintf("%v", store)},
		{"store-plus", fmt.Sprintf("%+v", store)},
		{"store-sharp", fmt.Sprintf("%#v", store)},
		{"error", fmt.Errorf("credential=%v", credential).Error()},
	}
	raw, err := json.Marshal(struct {
		Credential Credential `json:"credential"`
	}{Credential: credential})
	if err != nil {
		t.Fatal(err)
	}
	jsonLeak := bytes.Contains(raw, []byte(credential.BearerToken()))
	for _, item := range formatted {
		if strings.Contains(item.value, credential.BearerToken()) {
			t.Errorf("credential escaped %s formatting", item.name)
		}
	}
	if jsonLeak {
		t.Fatal("credential escaped JSON redaction")
	}
}
