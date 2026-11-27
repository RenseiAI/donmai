package workarea

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func packRefusal(t *testing.T, err error) *ContextPackError {
	t.Helper()
	var packErr *ContextPackError
	if !errors.As(err, &packErr) {
		t.Fatalf("refusal is not a typed pack error: %v", err)
	}
	return packErr
}

// TestMaterializeContextPackWritesPackIgnoreFile pins how a flat checkout
// keeps the published pack out of Git: an ignore file inside the pack
// directory itself, and nothing outside it. With a real checkout at the
// root, `git status` shows nothing and `git ls-files --ignored` lists only
// the pack.
//
// RED (with the ignore file no longer written): the file is missing and
// `git status` reports the pack as untracked.
func TestMaterializeContextPackWritesPackIgnoreFile(t *testing.T) {
	root := t.TempDir()
	hasGit := false
	if _, err := exec.LookPath("git"); err == nil {
		hasGit = true
		gitIn(t, root, "init", "-q", "-b", "main")
	}
	pack := ContextPackV1{Version: ContextPackProtocol, PackID: "pack1", Documents: []ContextDocumentV1{
		testPackDoc(t, "brief.md", "negotiated"),
		testPackDoc(t, "notes/pack.json", "a nested name may reuse a reserved leaf"),
	}}
	if _, err := MaterializeContextPack(RootPath(root), pack); err != nil {
		t.Fatalf("Materialize: %v", err)
	}
	packDir := filepath.Join(root, filepath.FromSlash(ContextPackDir("pack1")))
	body, err := os.ReadFile(filepath.Join(packDir, ContextPackIgnoreName)) //nolint:gosec // G304: test-owned path.
	if err != nil || string(body) != ContextPackIgnoreContent {
		t.Fatalf("pack ignore file = %q, %v; want %q", body, err, ContextPackIgnoreContent)
	}
	// Nothing else under the context directory: staging is gone and no
	// ignore file is written above the pack.
	entries, err := os.ReadDir(filepath.Dir(packDir))
	if err != nil || len(entries) != 1 || entries[0].Name() != "pack1" {
		t.Fatalf("context directory entries = %v, %v; want only the pack", entries, err)
	}
	if _, err := os.Stat(filepath.Join(root, DeclarationDirName, ContextPackIgnoreName)); !os.IsNotExist(err) {
		t.Fatalf("ignore file written above the pack (stat err %v)", err)
	}
	if !hasGit {
		return
	}
	if status := gitIn(t, root, "status", "--porcelain", "--untracked-files=all"); status != "" {
		t.Fatalf("published pack visible to git status:\n%s", status)
	}
	for _, line := range strings.Split(gitIn(t, root, "ls-files", "--others", "--ignored", "--exclude-standard"), "\n") {
		if line != "" && !strings.HasPrefix(line, ContextPackDir("pack1")+"/") {
			t.Fatalf("ignored path outside the pack: %q", line)
		}
	}
}

// TestMaterializeContextPackRedeliveryReprovesIgnoreFile pins redelivery:
// a removed, rewritten or replaced ignore file would let the flat checkout
// stage the documents, so it refuses like any other tampered publication.
func TestMaterializeContextPackRedeliveryReprovesIgnoreFile(t *testing.T) {
	tamper := map[string]func(t *testing.T, abs string){
		"deleted": func(t *testing.T, abs string) {
			if err := os.Remove(abs); err != nil {
				t.Fatal(err)
			}
		},
		"rewritten": func(t *testing.T, abs string) {
			if err := os.WriteFile(abs, []byte("!*\n"), 0o600); err != nil {
				t.Fatal(err)
			}
		},
		"directory": func(t *testing.T, abs string) {
			if err := os.Remove(abs); err != nil {
				t.Fatal(err)
			}
			if err := os.Mkdir(abs, 0o700); err != nil {
				t.Fatal(err)
			}
		},
	}
	for name, mutate := range tamper {
		t.Run(name, func(t *testing.T) {
			root := RootPath(t.TempDir())
			pack := publishPackForRedelivery(t, root)
			mutate(t, filepath.Join(string(root), filepath.FromSlash(ContextPackDir("pack1")), ContextPackIgnoreName))
			err := redeliverMustRefuse(t, root, pack, "a tampered ignore file")
			if packRefusal(t, err).Path != ContextPackIgnoreName {
				t.Fatalf("refusal path = %q; want the ignore file", packRefusal(t, err).Path)
			}
		})
	}
	root := RootPath(t.TempDir())
	pack := publishPackForRedelivery(t, root)
	if _, err := MaterializeContextPack(root, pack); err != nil {
		t.Fatalf("untouched redelivery refused: %v", err)
	}
}

// TestValidateContextPackRefusesReservedNames pins the names that share the
// pack directory with the documents: the marker and the ignore file, in any
// letter case and as a parent directory, collide; the same leaf deeper in
// the tree is an ordinary document.
func TestValidateContextPackRefusesReservedNames(t *testing.T) {
	for _, path := range []string{"pack.json", "PACK.JSON", ".gitignore", ".GitIgnore", "pack.json/x.md", ".gitignore/x"} {
		pack := ContextPackV1{Version: ContextPackProtocol, PackID: "pack1", Documents: []ContextDocumentV1{testPackDoc(t, path, "x")}}
		err := ValidateContextPack(pack)
		if err == nil {
			t.Fatalf("reserved destination %q accepted", path)
		}
		if got := packRefusal(t, err).Reason; got != ReasonContextPackCollision {
			t.Fatalf("reserved %q reason = %q; want %q", path, got, ReasonContextPackCollision)
		}
	}
	for _, path := range []string{"notes/pack.json", "notes/.gitignore", "pack.json.md"} {
		pack := ContextPackV1{Version: ContextPackProtocol, PackID: "pack1", Documents: []ContextDocumentV1{testPackDoc(t, path, "x")}}
		if err := ValidateContextPack(pack); err != nil {
			t.Fatalf("ordinary document %q refused: %v", path, err)
		}
	}
}

// TestValidateContextPackRefusesControlCharacters pins clean diagnostics: a
// path carrying a control character is refused, and the refusal never
// echoes the raw path into the error text.
func TestValidateContextPackRefusesControlCharacters(t *testing.T) {
	for _, path := range []string{"a\nb.md", "a\x00b.md", "a\tb.md", "a\x1bb.md", "a\x7fb.md", "a\u0085b.md"} {
		pack := ContextPackV1{Version: ContextPackProtocol, PackID: "pack1", Documents: []ContextDocumentV1{testPackDoc(t, path, "x")}}
		err := ValidateContextPack(pack)
		if err == nil {
			t.Fatalf("control-character path %q accepted", path)
		}
		packErr := packRefusal(t, err)
		if packErr.Reason != ReasonContextPackUnsafe || packErr.Path != "" {
			t.Fatalf("refusal for %q = %+v; want unsafe with no echoed path", path, packErr)
		}
		if strings.ContainsFunc(err.Error(), func(r rune) bool { return r < 0x20 || r == 0x7f }) {
			t.Fatalf("refusal text carries a control character: %q", err.Error())
		}
	}
}

// TestMaterializeContextPackAcceptsDocumentCountBoundary pins the bound
// itself: exactly MaxContextPackDocuments publishes; one more refuses.
func TestMaterializeContextPackAcceptsDocumentCountBoundary(t *testing.T) {
	pack := ContextPackV1{Version: ContextPackProtocol, PackID: "pack1"}
	for i := 0; i < MaxContextPackDocuments; i++ {
		pack.Documents = append(pack.Documents, testPackDoc(t, "doc-"+strconv.Itoa(i)+".md", "x"))
	}
	report, err := MaterializeContextPack(RootPath(t.TempDir()), pack)
	if err != nil || len(report.Included) != MaxContextPackDocuments {
		t.Fatalf("pack at the document bound = %d included, %v", len(report.Included), err)
	}
	pack.Documents = append(pack.Documents, testPackDoc(t, "one-more.md", "x"))
	_, err = MaterializeContextPack(RootPath(t.TempDir()), pack)
	if err == nil || packRefusal(t, err).Reason != ReasonContextPackLimit {
		t.Fatalf("pack over the document bound = %v; want a limit refusal", err)
	}
}

func gitIn(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...) //nolint:gosec // G204: test fixture with literal arguments.
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}
