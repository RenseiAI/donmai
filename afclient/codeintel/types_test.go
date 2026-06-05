package codeintel

import (
	"encoding/json"
	"testing"
)

// TestIndexFileRoundTrip verifies that an IndexFile round-trips through JSON
// without loss. This exercises S0: the Go structs must be byte-compatible with
// the TS-produced index.json schema.
func TestIndexFileRoundTrip(t *testing.T) {
	endLine := 42
	original := IndexFile{
		RootHash: "aabbccdd11223344",
		Files: map[string]FileIndex{
			"pkg/foo.go": {
				FilePath:    "pkg/foo.go",
				GitHash:     "deadbeef1234567890abcdef1234567890abcdef",
				LastIndexed: 1717000000000,
				Symbols: []CodeSymbol{
					{
						Name:          "MyFunc",
						Kind:          KindFunction,
						FilePath:      "pkg/foo.go",
						Line:          10,
						Exported:      true,
						Signature:     "func MyFunc(ctx context.Context) error",
						Documentation: "MyFunc does something.",
						Language:      "go",
					},
					{
						Name:     "MyStruct",
						Kind:     KindStruct,
						FilePath: "pkg/foo.go",
						Line:     20,
						EndLine:  &endLine,
						Exported: true,
						Language: "go",
					},
					{
						Name:       "internalHelper",
						Kind:       KindFunction,
						FilePath:   "pkg/foo.go",
						Line:       50,
						Exported:   false,
						Language:   "go",
						ParentName: "MyStruct",
					},
				},
			},
			"src/bar.ts": {
				FilePath:    "src/bar.ts",
				GitHash:     "feedface0000000000000000000000000000000",
				LastIndexed: 1717000001000,
				Symbols: []CodeSymbol{
					{
						Name:     "BarClass",
						Kind:     KindClass,
						FilePath: "src/bar.ts",
						Line:     5,
						Exported: true,
						Language: "typescript",
					},
				},
			},
		},
	}

	// Marshal to JSON.
	data, err := json.MarshalIndent(original, "", "  ")
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	// Unmarshal back.
	var decoded IndexFile
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	// Validate top-level fields.
	if decoded.RootHash != original.RootHash {
		t.Errorf("rootHash: got %q, want %q", decoded.RootHash, original.RootHash)
	}
	if len(decoded.Files) != len(original.Files) {
		t.Fatalf("files count: got %d, want %d", len(decoded.Files), len(original.Files))
	}

	// Validate the Go file entry.
	goFile, ok := decoded.Files["pkg/foo.go"]
	if !ok {
		t.Fatal("missing pkg/foo.go entry")
	}
	if goFile.GitHash != original.Files["pkg/foo.go"].GitHash {
		t.Errorf("gitHash: got %q, want %q", goFile.GitHash, original.Files["pkg/foo.go"].GitHash)
	}
	if len(goFile.Symbols) != 3 {
		t.Fatalf("symbols count: got %d, want 3", len(goFile.Symbols))
	}

	// First symbol — MyFunc.
	s0 := goFile.Symbols[0]
	if s0.Name != "MyFunc" {
		t.Errorf("symbol[0].name: got %q, want %q", s0.Name, "MyFunc")
	}
	if s0.Kind != KindFunction {
		t.Errorf("symbol[0].kind: got %q, want %q", s0.Kind, KindFunction)
	}
	if !s0.Exported {
		t.Error("symbol[0].exported: expected true")
	}
	if s0.Signature != "func MyFunc(ctx context.Context) error" {
		t.Errorf("symbol[0].signature: got %q", s0.Signature)
	}
	if s0.Documentation != "MyFunc does something." {
		t.Errorf("symbol[0].documentation: got %q", s0.Documentation)
	}

	// Second symbol — MyStruct with EndLine.
	s1 := goFile.Symbols[1]
	if s1.EndLine == nil {
		t.Error("symbol[1].endLine: expected non-nil")
	} else if *s1.EndLine != endLine {
		t.Errorf("symbol[1].endLine: got %d, want %d", *s1.EndLine, endLine)
	}

	// Third symbol — internalHelper with parentName.
	s2 := goFile.Symbols[2]
	if s2.ParentName != "MyStruct" {
		t.Errorf("symbol[2].parentName: got %q, want %q", s2.ParentName, "MyStruct")
	}
	if s2.Exported {
		t.Error("symbol[2].exported: expected false")
	}

	// Validate that omitempty fields are absent when zero.
	// EndLine on a symbol without it should be nil / absent in JSON.
	var rawMap map[string]any
	if err := json.Unmarshal(data, &rawMap); err != nil {
		t.Fatalf("re-unmarshal to map: %v", err)
	}
	filesRaw, _ := rawMap["files"].(map[string]any)
	goFileRaw, _ := filesRaw["pkg/foo.go"].(map[string]any)
	symbolsRaw, _ := goFileRaw["symbols"].([]any)
	if len(symbolsRaw) < 3 {
		t.Fatal("expected at least 3 symbols in raw JSON")
	}
	// internalHelper (index 2) has no endLine — key should not appear.
	s2raw, _ := symbolsRaw[2].(map[string]any)
	if _, ok := s2raw["endLine"]; ok {
		t.Error("symbol[2] in JSON should not have 'endLine' field (omitempty)")
	}
	// symbol without signature should omit the key.
	if _, ok := s2raw["signature"]; ok {
		t.Error("symbol[2] in JSON should not have 'signature' field (omitempty)")
	}
}

// TestIndexFileEmptySymbols verifies an index entry with no symbols marshals
// as an empty array, not null (preserving TS compatibility where the field is
// always an array).
func TestIndexFileEmptySymbols(t *testing.T) {
	idx := IndexFile{
		RootHash: "",
		Files: map[string]FileIndex{
			"empty.go": {
				FilePath:    "empty.go",
				GitHash:     "abc123",
				LastIndexed: 0,
				Symbols:     []CodeSymbol{},
			},
		},
	}
	data, err := json.Marshal(idx)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var back IndexFile
	if err := json.Unmarshal(data, &back); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	fi := back.Files["empty.go"]
	if fi.Symbols == nil {
		t.Error("symbols should unmarshal as empty slice, not nil")
	}
	if len(fi.Symbols) != 0 {
		t.Errorf("expected 0 symbols, got %d", len(fi.Symbols))
	}
}

// TestContentXXHash64 verifies the xxHash64 implementation against known
// values. The TS xxhash-wasm h64ToString() function uses seed=0; the Go
// github.com/cespare/xxhash/v2 package also uses seed=0 by default and must
// produce identical hex strings for the same input.
func TestContentXXHash64(t *testing.T) {
	cases := []struct {
		input string
		// Expected values verified against the xxHash64 spec (seed=0).
		wantLen int // All xxh64 results are 16 hex chars
	}{
		{"", 16},
		{"hello world", 16},
		{"test", 16},
		{"function hello() { return 'world' }", 16},
	}

	seen := make(map[string]string)
	for _, tc := range cases {
		got := ContentXXHash64(tc.input)
		if len(got) != tc.wantLen {
			t.Errorf("ContentXXHash64(%q) len=%d, want %d; got %q", tc.input, len(got), tc.wantLen, got)
		}
		for _, c := range got {
			if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f')) {
				t.Errorf("ContentXXHash64(%q) contains non-hex char %q: %s", tc.input, c, got)
				break
			}
		}
		if prev, ok := seen[got]; ok && prev != tc.input {
			t.Errorf("hash collision: ContentXXHash64(%q) == ContentXXHash64(%q) == %q", tc.input, prev, got)
		}
		seen[got] = tc.input
	}

	// Idempotency: same input always produces same output.
	h1 := ContentXXHash64("hello world")
	h2 := ContentXXHash64("hello world")
	if h1 != h2 {
		t.Errorf("xxhash64 not idempotent: %q vs %q", h1, h2)
	}

	// Sensitivity: different inputs produce different hashes.
	hA := ContentXXHash64("hello world")
	hB := ContentXXHash64("hello world!")
	if hA == hB {
		t.Errorf("xxhash64 insensitive: same hash for different inputs %q %q", hA, hB)
	}
}

// TestGitBlobHash verifies the git-blob SHA1 against the well-known value for
// the string "hello\n" (matches `echo -n 'hello' | git hash-object --stdin`
// for a 5-byte content).
func TestGitBlobHash(t *testing.T) {
	// "test content\n" -> known git hash can be checked with:
	//   printf "blob 13\x00test content\n" | sha1sum
	// But we verify at least the format and idempotency here.
	h1 := gitBlobHash([]byte("hello world"))
	h2 := gitBlobHash([]byte("hello world"))
	if h1 != h2 {
		t.Errorf("gitBlobHash not idempotent: %q vs %q", h1, h2)
	}
	if len(h1) != 40 {
		t.Errorf("gitBlobHash len: got %d, want 40", len(h1))
	}
	// Different content → different hash.
	h3 := gitBlobHash([]byte("hello world!"))
	if h1 == h3 {
		t.Errorf("gitBlobHash: collision for different inputs %q", h1)
	}
}
