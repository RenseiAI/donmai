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
