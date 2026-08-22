package sessionshim

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// shortTempDir returns a temp directory with a SHORT absolute path.
//
// t.TempDir() embeds the test name and a nesting counter, which on macOS pushes
// the path past 60 characters before anything is added to it. A unix socket path
// is capped near 104 bytes on the shortest supported platform (§D6), so a long
// base plus the registry's 32-hex socket name overflows and Listen fails with a
// confusing "invalid argument". Anchoring at /tmp keeps the fixture well inside
// the same bound production respects.
func shortTempDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "dss")
	if err != nil {
		dir = t.TempDir() // no /tmp: fall back and hope the path fits
		return dir
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
}

func TestIdentityValidateRejectsUnusableValues(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		id   Identity
		ok   bool
	}{
		{name: "well formed", id: Identity{OrgID: "org-1", SessionID: "sess-1"}, ok: true},
		{name: "uuid shaped", id: Identity{OrgID: "9f1c", SessionID: "79bddd74-52cb-4ace"}, ok: true},
		{name: "empty org", id: Identity{SessionID: "s"}},
		{name: "empty session", id: Identity{OrgID: "o"}},
		{name: "org with slash", id: Identity{OrgID: "a/b", SessionID: "s"}},
		{name: "session with backslash", id: Identity{OrgID: "o", SessionID: `a\b`}},
		{name: "session with NUL", id: Identity{OrgID: "o", SessionID: "a\x00b"}},
		{name: "org too long", id: Identity{OrgID: strings.Repeat("x", maxIdentityField+1), SessionID: "s"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := tc.id.Validate()
			if tc.ok {
				if err != nil {
					t.Fatalf("Validate = %v, want nil", err)
				}
				return
			}
			if !errors.Is(err, ErrInvalidIdentity) {
				t.Fatalf("Validate = %v, want ErrInvalidIdentity", err)
			}
		})
	}
}

func TestIdentityKeyCannotAliasTwoSessions(t *testing.T) {
	t.Parallel()

	// Validate permits ordinary punctuation in both halves, so a PRINTABLE
	// separator would make these two distinct sessions collide on one key — and
	// therefore on one registry filename. The unit separator cannot appear in a
	// validated field, so the encoding is unambiguous.
	a := Identity{OrgID: "a:b", SessionID: "c"}
	b := Identity{OrgID: "a", SessionID: "b:c"}
	if a.Key() == b.Key() {
		t.Fatalf("distinct identities share key %q", a.Key())
	}
	if a.RecordName() == b.RecordName() {
		t.Fatalf("distinct identities share record name %q", a.RecordName())
	}
}

func TestIdentityNamesAreFixedLengthAndDistinct(t *testing.T) {
	t.Parallel()

	// §D6: the on-disk name is a fixed-length digest so a long tenant identifier
	// cannot push a socket path past the platform limit.
	short := Identity{OrgID: "o", SessionID: "s"}
	long := Identity{OrgID: strings.Repeat("o", 200), SessionID: strings.Repeat("s", 200)}
	if len(short.SocketName()) != len(long.SocketName()) {
		t.Fatalf("socket name length varies with identity: %d vs %d", len(short.SocketName()), len(long.SocketName()))
	}
	if len(short.RecordName()) != len(long.RecordName()) {
		t.Fatalf("record name length varies with identity: %d vs %d", len(short.RecordName()), len(long.RecordName()))
	}

	// Record, tombstone, and socket names must never collide with each other,
	// or a tombstone would be scanned as a live record.
	names := map[string]string{
		short.RecordName():    "record",
		short.TombstoneName(): "tombstone",
		short.SocketName():    "socket",
	}
	if len(names) != 3 {
		t.Fatalf("name collision among record/tombstone/socket: %v", names)
	}
	if !strings.HasSuffix(short.TombstoneName(), tombstoneSuffix) {
		t.Fatalf("tombstone name %q lacks its suffix", short.TombstoneName())
	}
}

func TestIdentityNamesAreDeterministic(t *testing.T) {
	t.Parallel()

	// A daemon and a shim in different processes derive the same paths from the
	// same identity; a non-deterministic name would make discovery impossible.
	id := Identity{OrgID: "org-1", SessionID: "sess-1"}
	same := Identity{OrgID: "org-1", SessionID: "sess-1"}
	if id.RecordName() != same.RecordName() || id.SocketName() != same.SocketName() {
		t.Fatal("identity names are not deterministic across equal values")
	}
}

func TestSocketPathStaysWithinThePlatformLimit(t *testing.T) {
	t.Parallel()

	// 104 is the shortest limit across supported platforms. A registry under the
	// ordinary state directory plus the fixed-length socket name must fit with
	// room to spare, whatever the tenant identifiers look like.
	reg, err := NewRegistry(filepath.Join(shortTempDir(t), "session-shims"))
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	huge := Identity{OrgID: strings.Repeat("o", 200), SessionID: strings.Repeat("s", 200)}
	path := reg.SocketPath(huge)
	if len(path) >= 104 {
		t.Fatalf("socket path is %d bytes (%q); the shortest supported unix limit is 104", len(path), path)
	}
}
