package workarea

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
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
	cases := map[string]ContextDocumentV1{
		"traversal": testPackDoc(t, "../escape.md", "x"),
		"absolute":  testPackDoc(t, "/abs.md", "x"),
		"backslash": {Path: "a\\b.md", ContentBase64: base64.StdEncoding.EncodeToString([]byte("x")), Digest: "sha256:" + strings.Repeat("0", 64)},
		"duplicate": testPackDoc(t, "dup.md", "x"),
		"digest":    {Path: "ok.md", ContentBase64: base64.StdEncoding.EncodeToString([]byte("x")), Digest: "sha256:" + strings.Repeat("0", 64)},
	}
	for name, doc := range cases {
		t.Run(name, func(t *testing.T) {
			root := RootPath(t.TempDir())
			pack := ContextPackV1{Version: ContextPackProtocol, PackID: "pack1", Documents: []ContextDocumentV1{doc}}
			if name == "duplicate" {
				pack.Documents = append(pack.Documents, ContextDocumentV1{Path: "DUP.md", ContentBase64: doc.ContentBase64, Digest: doc.Digest})
			}
			outside := filepath.Join(t.TempDir(), "outside.md")
			if _, err := MaterializeContextPack(root, pack); err == nil {
				t.Fatal("unsafe pack accepted")
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
