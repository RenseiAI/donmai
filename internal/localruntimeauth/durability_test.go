package localruntimeauth

import (
	"bytes"
	"context"
	"errors"
	"os"
	"testing"
)

func TestPreopenedWriterReaffirmsVisibleAttemptBeforeRetry(t *testing.T) {
	first, root, queueRoot := newTestStore(t)
	ref := testAttemptRef(first)
	if _, err := first.CreateAttempt(context.Background(), "WarmHandle", ref); err != nil {
		t.Fatal(err)
	}
	second, err := Bootstrap(root, queueRoot)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = second.Close() }()
	if _, err := second.LoadAttempt("WarmHandle", ref); err != nil {
		t.Fatal(err)
	}
	first.dirSync = func(root *os.Root) error {
		if _, err := root.Lstat(attemptName("UncertainHandle")); err == nil {
			return errors.New("injected first directory fsync failure after link")
		}
		return nil
	}
	if _, err := first.CreateAttempt(context.Background(), "UncertainHandle", ref); !errors.Is(err, ErrAmbiguousCommit) {
		t.Fatalf("setup did not publish ambiguously: %v", err)
	}
	calls := 0
	second.dirSync = func(*os.Root) error { calls++; return errors.New("injected reaffirmation failure") }
	if _, err := second.CreateAttempt(context.Background(), "UncertainHandle", ref); err == nil || calls == 0 {
		t.Fatalf("pre-opened writer acknowledged uncertain attempt: err=%v reaffirmations=%d", err, calls)
	}
}

func TestPreopenedReaderReaffirmsVisibleAttemptBeforeLoadOrVerify(t *testing.T) {
	for _, operation := range []string{"load", "verify"} {
		t.Run(operation, func(t *testing.T) {
			first, root, queueRoot := newTestStore(t)
			ref := testAttemptRef(first)
			if _, err := first.CreateAttempt(context.Background(), "WarmHandle", ref); err != nil {
				t.Fatal(err)
			}
			second, err := OpenExisting(root, queueRoot)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = second.Close() }()
			if _, err := second.LoadAttempt("WarmHandle", ref); err != nil {
				t.Fatal(err)
			}
			first.dirSync = func(root *os.Root) error {
				if _, err := root.Lstat(attemptName("UncertainHandle")); err == nil {
					return errors.New("injected first directory fsync failure after link")
				}
				return nil
			}
			if _, err := first.CreateAttempt(context.Background(), "UncertainHandle", ref); !errors.Is(err, ErrAmbiguousCommit) {
				t.Fatalf("setup did not publish ambiguously: %v", err)
			}
			calls := 0
			second.dirSync = func(*os.Root) error { calls++; return errors.New("injected reader reaffirmation failure") }
			if operation == "load" {
				if _, err := second.LoadAttempt("UncertainHandle", ref); err == nil || calls == 0 {
					t.Fatalf("read-only load acknowledged uncertain attempt: err=%v reaffirmations=%d", err, calls)
				}
			} else if err := second.VerifyAttempt("UncertainHandle", ref, "synthetic-wrong-bearer"); !errors.Is(err, ErrUnauthorized) || calls == 0 {
				t.Fatalf("read-only verify bypassed reaffirmation: err=%v reaffirmations=%d", err, calls)
			}
		})
	}
}

func TestPreopenedWriterAndReaderReaffirmVisibleEndpoint(t *testing.T) {
	for _, readOnly := range []bool{false, true} {
		t.Run(map[bool]string{false: "writer", true: "reader"}[readOnly], func(t *testing.T) {
			first, root, queueRoot := newTestStore(t)
			if _, err := first.PublishEndpoint(context.Background(), "http://127.0.0.1:7734", "first-instance"); err != nil {
				t.Fatal(err)
			}
			var second *Store
			var err error
			if readOnly {
				second, err = OpenExisting(root, queueRoot)
			} else {
				second, err = Bootstrap(root, queueRoot)
			}
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = second.Close() }()
			if _, err := second.Endpoint(); err != nil {
				t.Fatal(err)
			}
			first.dirSync = func(root *os.Root) error {
				raw, err := root.ReadFile(endpointFileName)
				if err == nil && bytes.Contains(raw, []byte("uncertain-instance")) {
					return errors.New("injected first directory fsync failure after rename")
				}
				return nil
			}
			if _, err := first.PublishEndpoint(context.Background(), "http://127.0.0.1:7735", "uncertain-instance"); !errors.Is(err, ErrAmbiguousCommit) {
				t.Fatalf("setup did not publish ambiguously: %v", err)
			}
			calls := 0
			second.dirSync = func(*os.Root) error { calls++; return errors.New("injected endpoint reaffirmation failure") }
			if readOnly {
				if _, err := second.Endpoint(); err == nil || calls == 0 {
					t.Fatalf("pre-opened reader acknowledged uncertain endpoint: err=%v reaffirmations=%d", err, calls)
				}
			} else if _, err := second.PublishEndpoint(context.Background(), "http://127.0.0.1:7735", "uncertain-instance"); err == nil || calls == 0 {
				t.Fatalf("pre-opened writer acknowledged uncertain endpoint: err=%v reaffirmations=%d", err, calls)
			}
		})
	}
}
