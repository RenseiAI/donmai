package codeintel

import (
	"testing"
)

// sampleGo is a representative Go source file used across multiple test cases.
const sampleGo = `package example

import (
	"context"
	"fmt"
)

import "os"

// MyStruct is a sample struct.
type MyStruct struct {
	Name string
	Age  int
}

// MyInterface defines the interface.
type MyInterface interface {
	Run(ctx context.Context) error
}

// MyType is a type alias.
type MyType = string

// privateType is unexported.
type privateType struct{}

// NewMyStruct creates a MyStruct.
func NewMyStruct(name string) *MyStruct {
	return &MyStruct{Name: name}
}

// Run implements MyInterface.
func (m *MyStruct) Run(ctx context.Context) error {
	_ = fmt.Sprintf("running %s", m.Name)
	return nil
}

// helper is unexported.
func helper() string {
	return os.Getenv("HOME")
}

var GlobalVar = "hello"
const privateConst = 42
`

func TestGoExtractor_Language(t *testing.T) {
	ext := &GoExtractor{}
	ast := ext.Extract(sampleGo, "pkg/example.go")
	if ast.Language != "go" {
		t.Errorf("language: got %q, want go", ast.Language)
	}
	if ast.FilePath != "pkg/example.go" {
		t.Errorf("filePath: got %q", ast.FilePath)
	}
}

func TestGoExtractor_Imports(t *testing.T) {
	ext := &GoExtractor{}
	ast := ext.Extract(sampleGo, "pkg/example.go")
	for _, want := range []string{"context", "fmt", "os"} {
		if !contains(ast.Imports, want) {
			t.Errorf("missing import %q; got %v", want, ast.Imports)
		}
	}
}

func TestGoExtractor_Symbols(t *testing.T) {
	ext := &GoExtractor{}
	ast := ext.Extract(sampleGo, "pkg/example.go")

	type key struct {
		name string
		kind SymbolKind
	}
	lookup := make(map[key]CodeSymbol, len(ast.Symbols))
	for _, s := range ast.Symbols {
		lookup[key{s.Name, s.Kind}] = s
	}

	cases := []struct {
		name     string
		kind     SymbolKind
		exported bool
		language string
	}{
		{"MyStruct", KindStruct, true, "go"},
		{"MyInterface", KindInterface, true, "go"},
		{"MyType", KindType, true, "go"},
		{"privateType", KindStruct, false, "go"},
		{"NewMyStruct", KindFunction, true, "go"},
		{"Run", KindMethod, true, "go"},
		{"helper", KindFunction, false, "go"},
		{"GlobalVar", KindVariable, true, "go"},
		{"privateConst", KindVariable, false, "go"},
	}

	for _, tc := range cases {
		sym, ok := lookup[key{tc.name, tc.kind}]
		if !ok {
			t.Errorf("missing symbol %q (%s); available: %v", tc.name, tc.kind, symbolNames(ast.Symbols))
			continue
		}
		if sym.Exported != tc.exported {
			t.Errorf("symbol %q exported: got %v, want %v", tc.name, sym.Exported, tc.exported)
		}
		if sym.Language != tc.language {
			t.Errorf("symbol %q language: got %q, want %q", tc.name, sym.Language, tc.language)
		}
	}
}

func TestGoExtractor_MethodReceiver(t *testing.T) {
	ext := &GoExtractor{}
	ast := ext.Extract(sampleGo, "pkg/example.go")
	run := findSymbol(ast.Symbols, "Run", KindMethod)
	if run == nil {
		t.Fatal("missing method Run")
	}
	if run.ParentName != "MyStruct" {
		t.Errorf("Run.parentName: got %q, want MyStruct", run.ParentName)
	}
}

func TestGoExtractor_Documentation(t *testing.T) {
	ext := &GoExtractor{}
	ast := ext.Extract(sampleGo, "pkg/example.go")

	// NewMyStruct has a leading doc comment.
	newFn := findSymbol(ast.Symbols, "NewMyStruct", KindFunction)
	if newFn == nil {
		t.Fatal("missing symbol NewMyStruct")
	}
	if newFn.Documentation == "" {
		t.Error("NewMyStruct.documentation: expected non-empty doc comment")
	}
}

func TestGoExtractor_Signature(t *testing.T) {
	ext := &GoExtractor{}
	ast := ext.Extract(sampleGo, "pkg/example.go")

	newFn := findSymbol(ast.Symbols, "NewMyStruct", KindFunction)
	if newFn == nil {
		t.Fatal("missing symbol NewMyStruct")
	}
	if newFn.Signature == "" {
		t.Error("NewMyStruct.signature: expected non-empty signature")
	}
}

func TestGoExtractor_Exports(t *testing.T) {
	ext := &GoExtractor{}
	ast := ext.Extract(sampleGo, "pkg/example.go")
	for _, want := range []string{"MyStruct", "MyInterface", "MyType", "NewMyStruct", "Run", "GlobalVar"} {
		if !contains(ast.Exports, want) {
			t.Errorf("missing export %q; got %v", want, ast.Exports)
		}
	}
	// Unexported names must NOT appear in exports.
	for _, notWanted := range []string{"privateType", "helper", "privateConst"} {
		if contains(ast.Exports, notWanted) {
			t.Errorf("unexpected export %q in exports list", notWanted)
		}
	}
}

func TestGoExtractor_EmptyFile(t *testing.T) {
	ext := &GoExtractor{}
	ast := ext.Extract("", "empty.go")
	if len(ast.Symbols) != 0 {
		t.Errorf("expected 0 symbols for empty file, got %d", len(ast.Symbols))
	}
}

func TestGoExtractor_InlineImport(t *testing.T) {
	src := `package main

import "fmt"

func main() {
	fmt.Println("hello")
}
`
	ext := &GoExtractor{}
	ast := ext.Extract(src, "main.go")
	if !contains(ast.Imports, "fmt") {
		t.Errorf("missing import fmt; got %v", ast.Imports)
	}
	main := findSymbol(ast.Symbols, "main", KindFunction)
	if main == nil {
		t.Fatal("missing symbol main")
	}
	if main.Exported {
		t.Error("main should not be considered exported in Go")
	}
}

// TestGoExtractor_LineIsDeclarationKeywordLine pins symbol line attribution:
// Line must be the 1-based line of the `func`/`type` keyword itself — not the
// doc-comment start, and not a 0-based index (the engine historically reported
// the definition one line early, e.g. newAgentRunCmd at afcli/agent_run.go:80
// was reported as 79).
func TestGoExtractor_LineIsDeclarationKeywordLine(t *testing.T) {
	src := `package pay

// ProcessPayment handles a payment.
// It has a multi-line doc comment so the doc start
// and the keyword line differ by several lines.
func ProcessPayment(id string) error {
	return nil
}

// Ledger is a documented struct.
type Ledger struct {
	Total int
}

func undocumented() {}
`
	ext := &GoExtractor{}
	ast := ext.Extract(src, "pay.go")

	tests := []struct {
		name     string
		kind     SymbolKind
		wantLine int
	}{
		{"ProcessPayment", KindFunction, 6},
		{"Ledger", KindStruct, 11},
		{"undocumented", KindFunction, 15},
	}
	for _, tt := range tests {
		sym := findSymbol(ast.Symbols, tt.name, tt.kind)
		if sym == nil {
			t.Fatalf("missing symbol %s", tt.name)
		}
		if sym.Line != tt.wantLine {
			t.Errorf("%s: Line = %d, want %d (the declaration keyword line, 1-based)",
				tt.name, sym.Line, tt.wantLine)
		}
	}
}

// TestGoExtractor_EndLine_BraceInStringOrComment pins the string/comment-aware
// block scanner: a '}' inside a string literal, rune literal, raw string, or a
// // comment must not close the function extent early. A truncated extent
// hashes the wrong span (or, below symbolHashMinLines, drops the fingerprint
// entirely), making symbol-granular dedup silently miss exact pastes.
//
// RED (against the naive brace counter): EndLine = 6 (the `// }` comment line
// closed the block), want 12.
func TestGoExtractor_EndLine_BraceInStringOrComment(t *testing.T) {
	src := `package p

func Render(names []string) string {
	open := "{"
	clos := "}"
	// } this commented brace must not close the block
	raw := ` + "`}`" + `
	if len(names) == 0 {
		return open + clos + raw + string('}')
	}
	return names[0]
}

func After() {}
`
	ast := (&GoExtractor{}).Extract(src, "render.go")
	fn := findSymbol(ast.Symbols, "Render", KindFunction)
	if fn == nil {
		t.Fatal("missing symbol Render")
	}
	if fn.EndLine == nil {
		t.Fatal("Render EndLine = nil, want 12")
	}
	if *fn.EndLine != 12 {
		t.Errorf("Render EndLine = %d, want 12 (braces in strings/comments must not truncate the extent)", *fn.EndLine)
	}
	after := findSymbol(ast.Symbols, "After", KindFunction)
	if after == nil || after.EndLine == nil || *after.EndLine != 14 {
		t.Errorf("After EndLine = %v, want 14 (one-liner closes on its own line)", after.EndLine)
	}
}

// TestGoExtractor_EndLine_WrappedSignature pins extent recording for a
// declaration whose parameter list wraps across lines: the opening '{' is not
// on the keyword line, so the scanner must look ahead for it. Without this,
// wrapped-signature functions get no EndLine and therefore no dedup
// fingerprint at all.
//
// RED (against the decl-line-contains-'{' gate): Combine EndLine = nil.
func TestGoExtractor_EndLine_WrappedSignature(t *testing.T) {
	src := `package p

func Combine(
	a int,
	b int,
) (int, error) {
	return a + b, nil
}

//go:noescape
func stub(a, b int) int

func Sub(a, b int) int { return a - b }
`
	ast := (&GoExtractor{}).Extract(src, "combine.go")
	fn := findSymbol(ast.Symbols, "Combine", KindFunction)
	if fn == nil {
		t.Fatal("missing symbol Combine")
	}
	if fn.EndLine == nil {
		t.Fatal("Combine EndLine = nil, want 8 (wrapped signature: '{' is on line 6, body closes line 8)")
	}
	if *fn.EndLine != 8 {
		t.Errorf("Combine EndLine = %d, want 8", *fn.EndLine)
	}
	// A bodyless declaration (assembly stub / linkname) must NOT borrow the next
	// declaration's brace: no extent at all.
	stub := findSymbol(ast.Symbols, "stub", KindFunction)
	if stub == nil {
		t.Fatal("missing symbol stub")
	}
	if stub.EndLine != nil {
		t.Errorf("bodyless stub EndLine = %d, want nil (must not borrow the next decl's brace)", *stub.EndLine)
	}
	sub := findSymbol(ast.Symbols, "Sub", KindFunction)
	if sub == nil || sub.EndLine == nil || *sub.EndLine != 13 {
		t.Errorf("Sub EndLine = %v, want 13", sub.EndLine)
	}
}
