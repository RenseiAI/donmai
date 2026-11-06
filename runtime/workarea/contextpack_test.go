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
	"syscall"
	"testing"
	"time"
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

// TestValidateContextPackRefusesCaseVariantCollision pins the case-folded
// duplicate-destination check as load-bearing: destinations that differ only
// by letter case collide, so a pack carrying both must refuse with
// context_pack_collision. The check runs before any filesystem write, which
// keeps case-insensitive checkouts from silently overlaying one document on
// another. Reverting the fold to an exact comparison accepts this pack.
func TestValidateContextPackRefusesCaseVariantCollision(t *testing.T) {
	pack := ContextPackV1{Version: ContextPackProtocol, PackID: "pack1", Documents: []ContextDocumentV1{
		testPackDoc(t, "notes/Guide.md", "first"),
		testPackDoc(t, "notes/guide.md", "second"),
	}}
	err := ValidateContextPack(pack)
	var packErr *ContextPackError
	if !errors.As(err, &packErr) {
		t.Fatalf("case-variant destinations accepted: err=%v", err)
	}
	if packErr.Reason != ReasonContextPackCollision {
		t.Fatalf("reason = %q, want %q (err=%v)", packErr.Reason, ReasonContextPackCollision, err)
	}
	if _, err := DecodeContextPack(encodePack(t, pack)); err == nil {
		t.Fatal("case-variant carrier accepted by Decode")
	}
}

func TestMaterializeContextPackRefusesBadPackIdentity(t *testing.T) {
	for _, packID := range []string{"../evil", "", ".hidden"} {
		t.Run("packId="+strconv.Quote(packID), func(t *testing.T) {
			pack := ContextPackV1{Version: ContextPackProtocol, PackID: packID, Documents: []ContextDocumentV1{testPackDoc(t, "doc.md", "x")}}
			root := RootPath(t.TempDir())
			err := func() error {
				_, err := MaterializeContextPack(root, pack)
				return err
			}()
			if err == nil {
				t.Fatalf("packId %q accepted by Materialize", packID)
			}
			var packErr *ContextPackError
			if !errors.As(err, &packErr) {
				t.Fatalf("refusal is not a pack error: %v", err)
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

func publishPackForRedelivery(t *testing.T, root RootPath) ContextPackV1 {
	t.Helper()
	pack := ContextPackV1{Version: ContextPackProtocol, PackID: "pack1", Documents: []ContextDocumentV1{
		testPackDoc(t, "doc.md", "negotiated bytes"),
		testPackDoc(t, "notes/guide.md", "more bytes"),
	}}
	if _, err := MaterializeContextPack(root, pack); err != nil {
		t.Fatalf("Materialize: %v", err)
	}
	return pack
}

func redeliverMustRefuse(t *testing.T, root RootPath, pack ContextPackV1, what string) error {
	t.Helper()
	report, err := MaterializeContextPack(root, pack)
	if err == nil {
		t.Fatalf("redelivery accepted %s (included=%d)", what, len(report.Included))
	}
	return err
}

// TestMaterializeContextPackRedeliveryReprovesDocuments pins the
// redelivery trust fix: after a successful publish, a deleted, modified
// or symlinked document must refuse instead of reporting the original
// digest as still included.
func TestMaterializeContextPackRedeliveryReprovesDocuments(t *testing.T) {
	finalRel := ContextPackDir(RootPath("."), "pack1")
	tamper := map[string]func(t *testing.T, abs string){
		"deleted": func(t *testing.T, abs string) {
			if err := os.Remove(abs); err != nil {
				t.Fatal(err)
			}
		},
		"modified": func(t *testing.T, abs string) {
			if err := os.WriteFile(abs, []byte("tampered bytes!!"), 0o600); err != nil {
				t.Fatal(err)
			}
		},
		"symlinked": func(t *testing.T, abs string) {
			outside := filepath.Join(t.TempDir(), "outside.md")
			if err := os.WriteFile(outside, []byte("outside"), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.Remove(abs); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(outside, abs); err != nil {
				t.Fatal(err)
			}
		},
	}
	for name, fn := range tamper {
		t.Run(name, func(t *testing.T) {
			root := RootPath(t.TempDir())
			pack := publishPackForRedelivery(t, root)
			docAbs := filepath.Join(string(root), filepath.FromSlash(finalRel), "doc.md")
			fn(t, docAbs)
			err := redeliverMustRefuse(t, root, pack, name+" state")
			var packErr *ContextPackError
			if !errors.As(err, &packErr) {
				t.Fatalf("redelivery refusal is not a pack error: %v", err)
			}
		})
	}
}

// TestMaterializeContextPackRefusesForgedMarker pins the marker-identity
// fix: a marker with matching pack identity and digest entries but forged
// dir or version is refused, never returned unchanged.
func TestMaterializeContextPackRefusesForgedMarker(t *testing.T) {
	for _, forge := range []func(*ContextPackReport){func(r *ContextPackReport) { r.Dir = "../../outside" }, func(r *ContextPackReport) { r.Version = "unrecognized-version" }} {
		root := RootPath(t.TempDir())
		pack := publishPackForRedelivery(t, root)
		markerAbs := filepath.Join(string(root), filepath.FromSlash(ContextPackDir(root, pack.PackID)), ContextPackMarkerName)
		raw, err := os.ReadFile(markerAbs)
		if err != nil {
			t.Fatal(err)
		}
		var stored ContextPackReport
		if err := json.Unmarshal(raw, &stored); err != nil {
			t.Fatal(err)
		}
		forge(&stored)
		forged, err := json.Marshal(stored)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(markerAbs, forged, 0o600); err != nil {
			t.Fatal(err)
		}
		err = redeliverMustRefuse(t, root, pack, "forged-report state")
		var packErr *ContextPackError
		if !errors.As(err, &packErr) || packErr.Reason != ReasonContextPackConflict {
			t.Fatalf("forged marker reason = %v, want %q", err, ReasonContextPackConflict)
		}
	}
}

// TestMaterializeContextPackRefusesPlantedMarker pins the fast-path guard:
// an operator-authored marker that names the dispatched pack identity
// without any published document refuses instead of reporting success.
func TestMaterializeContextPackRefusesPlantedMarker(t *testing.T) {
	root := RootPath(t.TempDir())
	pack := ContextPackV1{Version: ContextPackProtocol, PackID: "pack1", Documents: []ContextDocumentV1{testPackDoc(t, "doc.md", "x")}}
	sum := sha256.Sum256([]byte("x"))
	digest := "sha256:" + hex.EncodeToString(sum[:])
	planted, err := json.Marshal(ContextPackReport{
		PackID:  "pack1",
		Version: ContextPackProtocol,
		Dir:     ContextPackDir(root, "pack1"),
		Included: []ContextPackIncluded{
			{Path: "doc.md", Bytes: 1, Digest: digest},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	plantDir := filepath.Join(string(root), filepath.FromSlash(ContextPackDir(root, "pack1")))
	if err := os.MkdirAll(plantDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(plantDir, ContextPackMarkerName), planted, 0o600); err != nil {
		t.Fatal(err)
	}
	report, err := MaterializeContextPack(root, pack)
	if err == nil {
		t.Fatalf("repository marker trusted: included=%d documentMissing=%v", len(report.Included), true)
	}
}

// TestMaterializeContextPackMarkerFIFORefusesWithoutBlocking pins the
// FIFO guard: a pre-existing FIFO at the marker path refuses promptly
// instead of blocking Open until an external writer connects.
func TestMaterializeContextPackMarkerFIFORefusesWithoutBlocking(t *testing.T) {
	root := RootPath(t.TempDir())
	pack := ContextPackV1{Version: ContextPackProtocol, PackID: "pack1", Documents: []ContextDocumentV1{testPackDoc(t, "doc.md", "x")}}
	packDir := filepath.Join(string(root), filepath.FromSlash(ContextPackDir(root, "pack1")))
	if err := os.MkdirAll(packDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Mkfifo(filepath.Join(packDir, ContextPackMarkerName), 0o600); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		_, err := MaterializeContextPack(root, pack)
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("FIFO marker accepted")
		}
		var packErr *ContextPackError
		if !errors.As(err, &packErr) {
			t.Fatalf("FIFO refusal is not a pack error: %v", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("materialization blocked opening FIFO")
	}
}

// TestMaterializeContextPackRefusesSymlinkedAncestors pins the dedicated
// directory fix: a symlinked metadata ancestor must refuse before any
// document is published, and the link target must stay untouched.
func TestMaterializeContextPackRefusesSymlinkedAncestors(t *testing.T) {
	for _, linkRel := range []string{DeclarationDirName, DeclarationDirName + "/" + ContextPackDirName} {
		t.Run(filepath.FromSlash(linkRel), func(t *testing.T) {
			root := RootPath(t.TempDir())
			parent := filepath.Dir(filepath.Join(string(root), filepath.FromSlash(linkRel)))
			if err := os.MkdirAll(parent, 0o700); err != nil {
				t.Fatal(err)
			}
			target := t.TempDir()
			if err := os.WriteFile(filepath.Join(target, "sentinel.md"), []byte("repository bytes"), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(target, filepath.Join(string(root), filepath.FromSlash(linkRel))); err != nil {
				t.Fatal(err)
			}
			pack := ContextPackV1{Version: ContextPackProtocol, PackID: "pack1", Documents: []ContextDocumentV1{testPackDoc(t, "guide.md", "negotiated bytes")}}
			if _, err := MaterializeContextPack(root, pack); err == nil {
				t.Fatal("symlinked ancestor accepted")
			}
			var packErr *ContextPackError
			if _, err := MaterializeContextPack(root, pack); !errors.As(err, &packErr) || packErr.Reason != ReasonContextPackUnsafe {
				t.Fatalf("ancestor refusal reason = %v, want %q", err, ReasonContextPackUnsafe)
			}
			entries, err := os.ReadDir(target)
			if err != nil {
				t.Fatal(err)
			}
			if len(entries) != 1 || entries[0].Name() != "sentinel.md" {
				t.Fatalf("link target changed: %v", entries)
			}
			body, err := os.ReadFile(filepath.Join(target, "sentinel.md"))
			if err != nil || string(body) != "repository bytes" {
				t.Fatalf("link target overwritten: %q, err=%v", body, err)
			}
		})
	}
}

// TestMaterializeContextPackUppercaseDigestRedeliverySucceeds pins digest
// normalization: an uppercase hexadecimal digest is accepted once and the
// identical carrier redelivers byte-identical documents instead of
// refusing with a false digest mismatch.
func TestMaterializeContextPackUppercaseDigestRedeliverySucceeds(t *testing.T) {
	root := RootPath(t.TempDir())
	doc := testPackDoc(t, "doc.md", "negotiated bytes")
	doc.Digest = "sha256:" + strings.ToUpper(strings.TrimPrefix(doc.Digest, "sha256:"))
	pack := ContextPackV1{Version: ContextPackProtocol, PackID: "pack1", Documents: []ContextDocumentV1{doc}}
	first, err := MaterializeContextPack(root, pack)
	if err != nil {
		t.Fatalf("uppercase digest refused on first delivery: %v", err)
	}
	if len(first.Included) != 1 {
		t.Fatalf("included = %+v", first.Included)
	}
	body, err := os.ReadFile(filepath.Join(string(root), filepath.FromSlash(first.Dir), "doc.md"))
	if err != nil || string(body) != "negotiated bytes" {
		t.Fatalf("published bytes = %q, err=%v", body, err)
	}
	again, err := MaterializeContextPack(root, pack)
	if err != nil {
		t.Fatalf("identical uppercase carrier refused on redelivery: %v", err)
	}
	if len(again.Included) != 1 || again.Included[0].Path != "doc.md" {
		t.Fatalf("redelivery included = %+v", again.Included)
	}
	rebody, err := os.ReadFile(filepath.Join(string(root), filepath.FromSlash(again.Dir), "doc.md"))
	if err != nil || string(rebody) != "negotiated bytes" {
		t.Fatalf("redelivered bytes = %q, err=%v", rebody, err)
	}
}

// TestMaterializeContextPackLegacyUppercaseMarkerRedeliverySucceeds pins the
// case-insensitive digest comparison as load-bearing: a marker written by an
// older publisher with uppercase hexadecimal digests must still match the
// identical carrier on redelivery. Reverting sameIncludedDigests to a
// case-sensitive comparison refuses this redelivery with a false conflict.
func TestMaterializeContextPackLegacyUppercaseMarkerRedeliverySucceeds(t *testing.T) {
	root := RootPath(t.TempDir())
	doc := testPackDoc(t, "doc.md", "negotiated bytes")
	pack := ContextPackV1{Version: ContextPackProtocol, PackID: "pack1", Documents: []ContextDocumentV1{doc}}
	first, err := MaterializeContextPack(root, pack)
	if err != nil {
		t.Fatalf("first delivery: %v", err)
	}
	markerPath := filepath.Join(string(root), filepath.FromSlash(first.Dir), ContextPackMarkerName)
	raw, err := os.ReadFile(markerPath)
	if err != nil {
		t.Fatalf("read marker: %v", err)
	}
	var report ContextPackReport
	if err := json.Unmarshal(raw, &report); err != nil {
		t.Fatalf("decode marker: %v", err)
	}
	for i := range report.Included {
		report.Included[i].Digest = "sha256:" + strings.ToUpper(strings.TrimPrefix(report.Included[i].Digest, "sha256:"))
	}
	upper, err := json.Marshal(report)
	if err != nil {
		t.Fatalf("encode marker: %v", err)
	}
	if err := os.WriteFile(markerPath, upper, 0o600); err != nil {
		t.Fatalf("seed legacy marker: %v", err)
	}
	again, err := MaterializeContextPack(root, pack)
	if err != nil {
		t.Fatalf("legacy uppercase marker refused on redelivery: %v", err)
	}
	if len(again.Included) != 1 || again.Included[0].Path != "doc.md" {
		t.Fatalf("redelivery included = %+v", again.Included)
	}
}
