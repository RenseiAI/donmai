package codeintel

import (
	"testing"
)

// TestBM25_Tokenize tests the code-aware tokenizer.
func TestBM25_Tokenize(t *testing.T) {
	tests := []struct {
		input    string
		contains []string // tokens that must appear
	}{
		{
			// camelCase expansion
			input:    "getUserById",
			contains: []string{"getuserbyid", "get", "user", "by", "id"},
		},
		{
			// snake_case expansion
			input:    "get_user_by_id",
			contains: []string{"get_user_by_id", "get", "user", "by", "id"},
		},
		{
			// kebab-case expansion
			input:    "get-user-by-id",
			contains: []string{"get-user-by-id", "get", "user", "by", "id"},
		},
		{
			// PascalCase
			input:    "SearchEngine",
			contains: []string{"searchengine", "search", "engine"},
		},
		{
			// ALL_CAPS static
			input:    "MAX_RESULTS",
			contains: []string{"max_results", "max", "results"},
		},
		{
			// Multiple words in a sentence
			input:    "find type usages",
			contains: []string{"find", "type", "usages"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.input, func(t *testing.T) {
			toks := tokenize(tc.input)
			tokSet := make(map[string]bool, len(toks))
			for _, t := range toks {
				tokSet[t] = true
			}
			for _, want := range tc.contains {
				if !tokSet[want] {
					t.Errorf("tokenize(%q) missing token %q; got %v", tc.input, want, toks)
				}
			}
		})
	}
}

// TestBM25_BasicRanking tests that BM25 ranks more-relevant documents higher.
func TestBM25_BasicRanking(t *testing.T) {
	symbols := []CodeSymbol{
		{Name: "getUserById", Kind: KindFunction, FilePath: "users.ts", Exported: true, Language: "typescript"},
		{Name: "createUser", Kind: KindFunction, FilePath: "users.ts", Exported: true, Language: "typescript"},
		{Name: "deletePost", Kind: KindFunction, FilePath: "posts.ts", Exported: true, Language: "typescript"},
		{Name: "getPostById", Kind: KindFunction, FilePath: "posts.ts", Exported: true, Language: "typescript"},
	}

	idx := buildInvertedIndex(symbols)
	scored := bm25Score("getUserById", idx)

	if len(scored) == 0 {
		t.Fatal("expected non-empty results")
	}
	// The top result should be the most relevant (getUserById or getPostById since they share "get"+"by"+"id").
	// getUserById should score highest since all tokens match.
	topIdx := scored[0].docID
	if symbols[topIdx].Name != "getUserById" {
		t.Errorf("expected getUserById as top result, got %q", symbols[topIdx].Name)
	}
}

// TestBM25_EmptyQuery returns empty results.
func TestBM25_EmptyQuery(t *testing.T) {
	symbols := []CodeSymbol{
		{Name: "doSomething", Kind: KindFunction, FilePath: "a.ts", Exported: true, Language: "typescript"},
	}
	idx := buildInvertedIndex(symbols)
	scored := bm25Score("", idx)
	if len(scored) != 0 {
		t.Errorf("expected empty results for empty query, got %d", len(scored))
	}
}

// TestBM25_EmptyIndex returns empty results.
func TestBM25_EmptyIndex(t *testing.T) {
	idx := buildInvertedIndex(nil)
	scored := bm25Score("search", idx)
	if len(scored) != 0 {
		t.Errorf("expected empty results for empty index, got %d", len(scored))
	}
}

// TestBM25_SplitCamelCase tests the camelCase splitter directly.
func TestBM25_SplitCamelCase(t *testing.T) {
	tests := []struct {
		input string
		want  []string
	}{
		{"helloWorld", []string{"hello", "World"}},
		{"HTMLParser", []string{"HTML", "Parser"}},
		{"getURLById", []string{"get", "URL", "By", "Id"}},
		{"simple", []string{"simple"}},
	}
	for _, tc := range tests {
		t.Run(tc.input, func(t *testing.T) {
			got := splitCamelCase(tc.input)
			if len(got) != len(tc.want) {
				t.Errorf("splitCamelCase(%q) = %v, want %v", tc.input, got, tc.want)
				return
			}
			for i, w := range tc.want {
				if got[i] != w {
					t.Errorf("splitCamelCase(%q)[%d] = %q, want %q", tc.input, i, got[i], w)
				}
			}
		})
	}
}

// TestBM25_SymbolToText verifies the symbol text used for indexing.
func TestBM25_SymbolToText(t *testing.T) {
	sym := CodeSymbol{
		Name:          "getUserById",
		Kind:          KindFunction,
		FilePath:      "src/users.ts",
		Signature:     "getUserById(id: string): User",
		Documentation: "Fetches a user by ID",
	}
	text := symbolToText(sym)
	for _, want := range []string{"getUserById", "function", "src/users.ts", "string", "Fetches"} {
		if len(text) == 0 {
			t.Fatal("symbolToText returned empty string")
		}
		found := false
		if len(text) > 0 {
			for i := 0; i <= len(text)-len(want); i++ {
				if text[i:i+len(want)] == want {
					found = true
					break
				}
			}
		}
		if !found {
			t.Errorf("symbolToText missing %q in %q", want, text)
		}
	}
}

// TestSearchCodeNative_BasicRanking tests full-text BM25 search over a corpus.
func TestSearchCodeNative_BasicRanking(t *testing.T) {
	// Build a minimal index with known content.
	files := map[string]FileIndex{
		"auth.ts": {
			FilePath: "auth.ts",
			GitHash:  "abc",
			Symbols: []CodeSymbol{
				{Name: "authenticateUser", Kind: KindFunction, FilePath: "auth.ts", Exported: true, Language: "typescript"},
				{Name: "verifyToken", Kind: KindFunction, FilePath: "auth.ts", Exported: true, Language: "typescript"},
			},
		},
		"users.ts": {
			FilePath: "users.ts",
			GitHash:  "def",
			Symbols: []CodeSymbol{
				{Name: "getUserById", Kind: KindFunction, FilePath: "users.ts", Exported: true, Language: "typescript"},
				{Name: "createUser", Kind: KindFunction, FilePath: "users.ts", Exported: true, Language: "typescript"},
			},
		},
	}

	// Collect symbols.
	var allSymbols []CodeSymbol
	for _, fi := range files {
		allSymbols = append(allSymbols, fi.Symbols...)
	}

	idx := buildInvertedIndex(allSymbols)
	scored := bm25Score("user", idx)
	if len(scored) == 0 {
		t.Fatal("expected results for query 'user'")
	}

	// Top result should relate to "user" — getUserById or createUser or authenticateUser.
	topSym := allSymbols[scored[0].docID]
	userRelated := false
	for _, s := range []string{"getUserById", "createUser", "authenticateUser"} {
		if topSym.Name == s {
			userRelated = true
			break
		}
	}
	if !userRelated {
		t.Errorf("top BM25 result for 'user' was %q, expected a user-related symbol", topSym.Name)
	}
}
