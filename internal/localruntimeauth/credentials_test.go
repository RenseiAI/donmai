package localruntimeauth

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/RenseiAI/donmai/internal/localqueue"
)

func testAttemptRef(store *Store) localqueue.AttemptRef {
	identity := store.Identity()
	return localqueue.AttemptRef{
		ScopeID: identity.ScopeID, WorkerID: identity.WorkerID,
		SessionID: "session-one", AttemptID: "attempt-one", Generation: 1,
	}
}

func TestAttemptCredentialExactRetryRolesAndReopen(t *testing.T) {
	store, root, queueRoot := newTestStore(t)
	ref := testAttemptRef(store)
	operator, err := store.OperatorCredential()
	if err != nil {
		t.Fatal(err)
	}
	attempt, err := store.CreateAttempt(context.Background(), "HandleOne", ref)
	if err != nil || !validToken(attempt.BearerToken(), attemptTokenPrefix) || equalSecret(attempt.BearerToken(), operator.BearerToken()) {
		t.Fatalf("attempt role or entropy invalid: %v", err)
	}
	retry, err := store.CreateAttempt(context.Background(), "HandleOne", ref)
	if err != nil || !equalSecret(retry.BearerToken(), attempt.BearerToken()) {
		t.Fatalf("exact attempt retry rotated secret: %v", err)
	}
	if err := store.VerifyAttempt("HandleOne", ref, attempt.BearerToken()); err != nil {
		t.Fatal(err)
	}
	if err := store.VerifyOperator(attempt.BearerToken()); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("attempt bearer crossed operator role: %v", err)
	}
	if err := store.VerifyAttempt("HandleOne", ref, operator.BearerToken()); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("operator bearer crossed attempt role: %v", err)
	}
	if err := store.VerifyOperator("incorrect"); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("incorrect operator bearer = %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := OpenExisting(root, queueRoot)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = reopened.Close() }()
	loaded, err := reopened.LoadAttempt("HandleOne", ref)
	if err != nil || !equalSecret(loaded.BearerToken(), attempt.BearerToken()) {
		t.Fatalf("attempt restart readback changed secret: %v", err)
	}
	if _, err := reopened.CreateAttempt(context.Background(), "HandleTwo", ref); !errors.Is(err, ErrReadOnly) {
		t.Fatalf("read-only attempt create = %v", err)
	}
}

func TestAttemptRefAndHandleRefusals(t *testing.T) {
	store, _, _ := newTestStore(t)
	ref := testAttemptRef(store)
	attempt, err := store.CreateAttempt(context.Background(), "HandleOne", ref)
	if err != nil {
		t.Fatal(err)
	}
	wrong := []localqueue.AttemptRef{
		func() localqueue.AttemptRef { r := ref; r.ScopeID = "other-scope"; return r }(),
		func() localqueue.AttemptRef { r := ref; r.SessionID = "session-two"; return r }(),
		func() localqueue.AttemptRef { r := ref; r.AttemptID = "attempt-two"; return r }(),
		func() localqueue.AttemptRef { r := ref; r.Generation++; return r }(),
		func() localqueue.AttemptRef { r := ref; r.WorkerID = "other-worker"; return r }(),
	}
	for _, changed := range wrong {
		if _, err := store.LoadAttempt("HandleOne", changed); err == nil {
			t.Fatal("changed attempt ref loaded original credential")
		}
		if _, err := store.CreateAttempt(context.Background(), "HandleOne", changed); err == nil {
			t.Fatal("changed attempt ref rotated credential handle")
		}
		if err := store.VerifyAttempt("HandleOne", changed, attempt.BearerToken()); !errors.Is(err, ErrUnauthorized) {
			t.Fatalf("changed attempt ref verified bearer: %v", err)
		}
	}
	for _, handle := range []string{"../escape", "rsp_fake", "locat_not-a-handle", "", "A"} {
		if _, err := store.CreateAttempt(context.Background(), handle, ref); !errors.Is(err, ErrInvalidInput) {
			t.Fatalf("invalid handle accepted: %v", err)
		}
	}
	if err := store.VerifyAttempt("OtherHandle", ref, attempt.BearerToken()); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("wrong handle verified original attempt: %v", err)
	}
	loaded, err := store.LoadAttempt("HandleOne", ref)
	if err != nil || !equalSecret(loaded.BearerToken(), attempt.BearerToken()) {
		t.Fatalf("refusals changed original credential: %v", err)
	}
}

func TestConcurrentExactAttemptCreationReturnsOneSecret(t *testing.T) {
	store, _, _ := newTestStore(t)
	ref := testAttemptRef(store)
	type result struct {
		token string
		err   error
	}
	results := make(chan result, 8)
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			credential, err := store.CreateAttempt(context.Background(), "HandleOne", ref)
			results <- result{token: credential.BearerToken(), err: err}
		}()
	}
	wg.Wait()
	close(results)
	var first string
	for got := range results {
		if got.err != nil {
			t.Fatalf("concurrent create failed: %v", got.err)
		}
		if first == "" {
			first = got.token
		} else if !equalSecret(first, got.token) {
			t.Fatal("concurrent create returned different secrets")
		}
	}
}

func TestConcurrentWritersReturnOneAttemptSecret(t *testing.T) {
	first, root, queueRoot := newTestStore(t)
	second, err := Bootstrap(root, queueRoot)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = second.Close() }()
	ref := testAttemptRef(first)
	results := make(chan struct {
		credential Credential
		err        error
	}, 2)
	for _, writer := range []*Store{first, second} {
		go func() {
			credential, err := writer.CreateAttempt(context.Background(), "SharedHandle", ref)
			results <- struct {
				credential Credential
				err        error
			}{credential: credential, err: err}
		}()
	}
	a, b := <-results, <-results
	if a.err != nil || b.err != nil || !equalSecret(a.credential.BearerToken(), b.credential.BearerToken()) {
		t.Fatalf("concurrent exact writers disagreed: first=%v second=%v", a.err, b.err)
	}
}

func TestAttemptDirectoryMustRemainPrivateAndUnlinked(t *testing.T) {
	for _, kind := range []string{"world-readable", "symlink"} {
		t.Run(kind, func(t *testing.T) {
			store, root, _ := newTestStore(t)
			ref := testAttemptRef(store)
			if _, err := store.CreateAttempt(context.Background(), "HandleOne", ref); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(root, attemptsDirectory)
			switch kind {
			case "world-readable":
				setFixtureMode(t, path, 0o755)
			case "symlink":
				if err := os.Rename(path, path+"-old"); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(path+"-old", path); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := store.LoadAttempt("HandleOne", ref); !errors.Is(err, ErrCorruptState) {
				t.Fatalf("untrusted attempts directory read = %v", err)
			}
		})
	}
}

func TestAttemptMissingCorruptAndPermissionRefusal(t *testing.T) {
	for _, kind := range []string{"missing", "world-readable", "symlink", "hardlink", "corrupt"} {
		t.Run(kind, func(t *testing.T) {
			store, root, _ := newTestStore(t)
			ref := testAttemptRef(store)
			credential, err := store.CreateAttempt(context.Background(), "HandleOne", ref)
			if err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(root, attemptsDirectory, attemptName("HandleOne"))
			switch kind {
			case "missing":
				err = os.Remove(path)
			case "world-readable":
				setFixtureMode(t, path, 0o644)
			case "symlink":
				outside := filepath.Join(t.TempDir(), "synthetic-secret")
				data, readErr := os.ReadFile(path)
				if readErr != nil {
					t.Fatal(readErr)
				}
				replaceFixtureFile(t, outside, data)
				err = os.Remove(path)
				if err == nil {
					err = os.Symlink(outside, path)
				}
			case "hardlink":
				err = os.Link(path, filepath.Join(root, attemptsDirectory, "second-link"))
			case "corrupt":
				replaceFixtureFile(t, path, []byte(`{"version":9}`))
			}
			if err != nil {
				t.Fatal(err)
			}
			if _, err := store.LoadAttempt("HandleOne", ref); err == nil {
				t.Fatal("missing or corrupt attempt returned a secret")
			}
			if err := store.VerifyAttempt("HandleOne", ref, credential.BearerToken()); !errors.Is(err, ErrUnauthorized) {
				t.Fatalf("missing or corrupt attempt authenticated: %v", err)
			}
		})
	}
}

func TestAttemptFsyncFailuresAndAmbiguousRecovery(t *testing.T) {
	store, root, queueRoot := newTestStore(t)
	ref := testAttemptRef(store)
	actualSync := store.fileSync
	store.fileSync = func(*os.File) error { return errors.New("injected file fsync") }
	if _, err := store.CreateAttempt(context.Background(), "HandleOne", ref); err == nil {
		t.Fatal("file fsync failure returned a credential")
	}
	if _, err := store.LoadAttempt("HandleOne", ref); !errors.Is(err, ErrMissingCredential) {
		t.Fatalf("failed file fsync published attempt: %v", err)
	}
	store.fileSync = actualSync
	store.fault = func(stage string) error {
		if stage == "before_link" {
			return errors.New("injected pre-publication interruption")
		}
		return nil
	}
	if _, err := store.CreateAttempt(context.Background(), "HandleOne", ref); err == nil {
		t.Fatal("interrupted pre-publication write returned a credential")
	}
	if _, err := store.LoadAttempt("HandleOne", ref); !errors.Is(err, ErrMissingCredential) {
		t.Fatalf("interrupted pre-publication write left authority: %v", err)
	}
	store.fault = nil
	if _, err := store.CreateAttempt(context.Background(), "HandleOne", ref); err != nil {
		t.Fatal(err)
	}
	actualDirSync := store.dirSync
	store.dirSync = func(root *os.Root) error {
		if _, err := root.Lstat(attemptName("HandleTwo")); err == nil {
			return errors.New("injected directory fsync after link")
		}
		return nil
	}
	if _, err := store.CreateAttempt(context.Background(), "HandleTwo", ref); !errors.Is(err, ErrAmbiguousCommit) {
		t.Fatalf("directory fsync failure = %v, want ambiguous", err)
	}
	store.dirSync = actualDirSync
	if _, err := store.LoadAttempt("HandleTwo", ref); !errors.Is(err, ErrAmbiguousCommit) {
		t.Fatalf("poisoned store returned uncertain credential: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := OpenExisting(root, queueRoot)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = reopened.Close() }()
	confirmed, err := reopened.LoadAttempt("HandleTwo", ref)
	if err != nil || !validToken(confirmed.BearerToken(), attemptTokenPrefix) {
		t.Fatalf("reopen failed to reaffirm visible attempt: %v", err)
	}
	writer, err := Bootstrap(root, queueRoot)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = writer.Close() }()
	retry, err := writer.CreateAttempt(context.Background(), "HandleTwo", ref)
	if err != nil || !equalSecret(retry.BearerToken(), confirmed.BearerToken()) {
		t.Fatalf("post-ambiguity retry replaced credential: %v", err)
	}
}
