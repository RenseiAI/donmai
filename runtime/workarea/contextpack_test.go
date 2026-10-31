package workarea

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func testPackDoc(t *testing.T, path, content string) ContextDocumentV1 {
	t.Helper()
	sum := sha256.Sum256([]byte(content))
	return ContextDocumentV1{
		Path:          path,
		ContentBase64: base64.StdEncoding.EncodeToString([]byte(content)),
		Digest:        "sha256:" + hex.EncodeToString(sum[:]),
	}
}

func encodePack(t *testing.T, pack ContextPackV1) []byte {
	t.Helper()
	raw, err := json.Marshal(pack)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func TestDecodeContextPackRoundTrip(t *testing.T) {
	pack := ContextPackV1{Version: ContextPackProtocol, PackID: "pack1", Documents: []ContextDocumentV1{testPackDoc(t, "notes/guide.md", "hello")}}
	decoded, err := DecodeContextPack(encodePack(t, pack))
	if err != nil {
		t.Fatalf("DecodeContextPack: %v", err)
	}
	if decoded.PackID != "pack1" || len(decoded.Documents) != 1 {
		t.Fatalf("decoded = %+v", decoded)
	}
	if _, err := DecodeContextPack([]byte(`{"version":"unknown","packId":"x"}`)); err == nil {
		t.Fatal("unknown version accepted")
	}
}

func TestDecodeContextPackRefusesMalformedCarrier(t *testing.T) {
	pack := ContextPackV1{Version: ContextPackProtocol, PackID: "pack1", Documents: []ContextDocumentV1{testPackDoc(t, "doc.md", "x")}}
	valid := encodePack(t, pack)
	cases := map[string]struct {
		carrier    []byte
		wantReason ContextPackReason
	}{
		"unknown-field": {carrier: []byte(`{"version":"context-pack-v1","packId":"pack1","documents":[],"extra":1}`), wantReason: ReasonContextPackInvalid},
		"trailing-data": {carrier: append(append([]byte{}, valid...), 'x'), wantReason: ReasonContextPackInvalid},
		"empty":         {carrier: []byte{}, wantReason: ReasonContextPackLimit},
		"oversize":      {carrier: make([]byte, maxContextPackCarrierBytes+1), wantReason: ReasonContextPackLimit},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := DecodeContextPack(tc.carrier)
			if err == nil {
				t.Fatalf("%s carrier accepted", name)
			}
			var packErr *ContextPackError
			if !errors.As(err, &packErr) {
				t.Fatalf("%s refusal is not a pack error: %v", name, err)
			}
			if packErr.Reason != tc.wantReason {
				t.Fatalf("%s reason = %q, want %q (err=%v)", name, packErr.Reason, tc.wantReason, err)
			}
		})
	}
}

func TestDecodeContextPackRefusesBadPackIdentity(t *testing.T) {
	for _, packID := range []string{"../evil", "", ".hidden"} {
		t.Run("packId="+strconv.Quote(packID), func(t *testing.T) {
			pack := ContextPackV1{Version: ContextPackProtocol, PackID: packID, Documents: []ContextDocumentV1{testPackDoc(t, "doc.md", "x")}}
			if _, err := DecodeContextPack(encodePack(t, pack)); err == nil {
				t.Fatalf("packId %q accepted by Decode", packID)
			}
			root := RootPath(t.TempDir())
			if _, err := MaterializeContextPack(root, pack); err == nil {
				t.Fatalf("packId %q accepted by Materialize", packID)
			}
			if packID == "pack1" {
				t.Fatal("unreachable")
			}
			// No session-owned context directory may appear for a refused identity.
			if entries, _ := os.ReadDir(filepath.Join(string(root), DeclarationDirName, ContextPackDirName)); len(entries) != 0 {
				t.Fatalf("refused pack identity left %d entries", len(entries))
			}
		})
	}
}

func TestMaterializeContextPackPublishesBytes(t *testing.T) {
	root := RootPath(t.TempDir())
	pack := ContextPackV1{Version: ContextPackProtocol, PackID: "pack1", Documents: []ContextDocumentV1{
		testPackDoc(t, "notes/guide.md", "hello"),
		testPackDoc(t, "AGENTS.md", "instructions"),
	}}
	report, err := MaterializeContextPack(root, pack)
	if err != nil {
		t.Fatalf("Materialize: %v", err)
	}
	if len(report.Included) != 2 {
		t.Fatalf("included = %+v", report.Included)
	}
	for _, doc := range pack.Documents {
		body, err := os.ReadFile(filepath.Join(string(root), report.Dir, filepath.FromSlash(doc.Path)))
		if err != nil {
			t.Fatalf("read %s: %v", doc.Path, err)
		}
		want, _ := base64.StdEncoding.DecodeString(doc.ContentBase64)
		if string(body) != string(want) {
			t.Fatalf("byte mismatch for %s", doc.Path)
		}
	}
	// Idempotent redelivery preserves the same pack.
	again, err := MaterializeContextPack(root, pack)
	if err != nil || len(again.Included) != 2 {
		t.Fatalf("redelivery = %+v, err=%v", again, err)
	}
	// A different pack under the same identity is refused.
	other := pack
	other.Documents = []ContextDocumentV1{testPackDoc(t, "notes/guide.md", "changed")}
	if _, err := MaterializeContextPack(root, other); err == nil {
		t.Fatal("conflicting pack overwrite accepted")
	}
}

func TestMaterializeContextPackRefusesUnsafe(t *testing.T) {
	longPath := strings.Repeat("a", 200) + "/" + strings.Repeat("b", 100) + ".md"
	cases := map[string]struct {
		doc        ContextDocumentV1
		wantReason ContextPackReason
		wantDetail string
	}{
		"traversal":         {doc: testPackDoc(t, "../escape.md", "x"), wantReason: ReasonContextPackUnsafe},
		"absolute":          {doc: testPackDoc(t, "/abs.md", "x"), wantReason: ReasonContextPackUnsafe, wantDetail: "absolute path"},
		"backslash":         {doc: testPackDoc(t, "a\\b.md", "x"), wantReason: ReasonContextPackUnsafe, wantDetail: "backslash in path"},
		"empty-path":        {doc: testPackDoc(t, "", "x"), wantReason: ReasonContextPackLimit, wantDetail: "path length out of bounds"},
		"long-path":         {doc: testPackDoc(t, longPath, "x"), wantReason: ReasonContextPackLimit, wantDetail: "path length out of bounds"},
		"duplicate":         {doc: testPackDoc(t, "dup.md", "x"), wantReason: ReasonContextPackCollision},
		"digest":            {doc: ContextDocumentV1{Path: "ok.md", ContentBase64: base64.StdEncoding.EncodeToString([]byte("x")), Digest: "sha256:" + strings.Repeat("0", 64)}, wantReason: ReasonContextPackDigest},
		"bad-digest-format": {doc: ContextDocumentV1{Path: "ok.md", ContentBase64: base64.StdEncoding.EncodeToString([]byte("x")), Digest: "not-a-digest"}, wantReason: ReasonContextPackDigest},
		"bad-base64":        {doc: ContextDocumentV1{Path: "ok.md", ContentBase64: "!!!not-base64!!!", Digest: "sha256:" + strings.Repeat("0", 64)}, wantReason: ReasonContextPackInvalid},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			root := RootPath(t.TempDir())
			pack := ContextPackV1{Version: ContextPackProtocol, PackID: "pack1", Documents: []ContextDocumentV1{tc.doc}}
			if name == "duplicate" {
				pack.Documents = append(pack.Documents, ContextDocumentV1{Path: "DUP.md", ContentBase64: tc.doc.ContentBase64, Digest: tc.doc.Digest})
			}
			outside := filepath.Join(t.TempDir(), "outside.md")
			_, err := MaterializeContextPack(root, pack)
			if err == nil {
				t.Fatal("unsafe pack accepted")
			}
			var packErr *ContextPackError
			if !errors.As(err, &packErr) {
				t.Fatalf("refusal is not a pack error: %v", err)
			}
			if packErr.Reason != tc.wantReason {
				t.Fatalf("reason = %q, want %q (err=%v)", packErr.Reason, tc.wantReason, err)
			}
			if tc.wantDetail != "" && !strings.Contains(packErr.Detail, tc.wantDetail) {
				t.Fatalf("detail = %q, want substring %q", packErr.Detail, tc.wantDetail)
			}
			if _, statErr := os.Stat(outside); !os.IsNotExist(statErr) {
				t.Fatal("outside file changed")
			}
			if _, statErr := os.Stat(filepath.Join(string(root), DeclarationDirName, ContextPackDirName, "pack1")); !os.IsNotExist(statErr) {
				t.Fatal("partial pack visible")
			}
		})
	}
}

func TestMaterializeContextPackRefusesLimits(t *testing.T) {
	root := RootPath(t.TempDir())
	many := ContextPackV1{Version: ContextPackProtocol, PackID: "pack1"}
	for i := 0; i < MaxContextPackDocuments+1; i++ {
		many.Documents = append(many.Documents, testPackDoc(t, "doc-"+strconv.Itoa(i)+".md", "x"))
	}
	if _, err := MaterializeContextPack(root, many); err == nil {
		t.Fatal("document count limit accepted")
	}
	big := strings.Repeat("x", MaxContextPackBytesPerDocument+1)
	oversize := ContextPackV1{Version: ContextPackProtocol, PackID: "pack1", Documents: []ContextDocumentV1{testPackDoc(t, "big.md", big)}}
	if _, err := MaterializeContextPack(root, oversize); err == nil {
		t.Fatal("document size limit accepted")
	}
	// Total pack size exceeds the bound while each document stays under the
	// per-document bound: 5 documents at the per-document maximum.
	full := strings.Repeat("x", MaxContextPackBytesPerDocument)
	totalPack := ContextPackV1{Version: ContextPackProtocol, PackID: "pack1"}
	for i := 0; i < 5; i++ {
		totalPack.Documents = append(totalPack.Documents, testPackDoc(t, "total-"+strconv.Itoa(i)+".md", full))
	}
	if _, err := MaterializeContextPack(root, totalPack); err == nil {
		t.Fatal("total pack size limit accepted")
	} else {
		var packErr *ContextPackError
		if !errors.As(err, &packErr) || packErr.Reason != ReasonContextPackLimit {
			t.Fatalf("total refusal reason = %v, want %q", err, ReasonContextPackLimit)
		}
	}
	if _, statErr := os.Stat(filepath.Join(string(root), DeclarationDirName, ContextPackDirName, "pack1")); !os.IsNotExist(statErr) {
		t.Fatal("partial pack visible after limit refusal")
	}
}

func TestMaterializeContextPackRefusesSymlinkEscape(t *testing.T) {
	root := RootPath(t.TempDir())
	finalRel := ContextPackDir(root, "pack1")
	finalAbs := filepath.Join(string(root), filepath.FromSlash(finalRel))
	if err := os.MkdirAll(filepath.Dir(finalAbs), 0o700); err != nil {
		t.Fatal(err)
	}
	target := t.TempDir()
	if err := os.Symlink(target, finalAbs); err != nil {
		t.Fatal(err)
	}
	pack := ContextPackV1{Version: ContextPackProtocol, PackID: "pack1", Documents: []ContextDocumentV1{testPackDoc(t, "doc.md", "x")}}
	if _, err := MaterializeContextPack(root, pack); err == nil {
		t.Fatal("symlink-occupied pack accepted")
	}
	if entries, _ := os.ReadDir(target); len(entries) != 0 {
		t.Fatal("symlink target changed")
	}
}
