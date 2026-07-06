package codeintel

import (
	"testing"
)

// samplePython is a representative Python file used across test cases.
const samplePython = `from os import path
import sys

"""This is a module docstring."""

MY_CONSTANT = 42
_private_var = "hidden"

@decorator_func
class MyClass:
    """Class docstring."""

    def __init__(self, name: str):
        self.name = name

    def public_method(self):
        pass

    def _private_method(self):
        pass

def top_level_func(x, y):
    return x + y

async def async_func():
    pass

def _private_func():
    pass
`

func TestPythonExtractor_Language(t *testing.T) {
	ext := &PythonExtractor{}
	ast := ext.Extract(samplePython, "example.py")
	if ast.Language != "python" {
		t.Errorf("language: got %q, want python", ast.Language)
	}
	if ast.FilePath != "example.py" {
		t.Errorf("filePath: got %q, want example.py", ast.FilePath)
	}
}

func TestPythonExtractor_Imports(t *testing.T) {
	ext := &PythonExtractor{}
	ast := ext.Extract(samplePython, "example.py")
	want := map[string]bool{"os": true, "sys": true}
	for _, imp := range ast.Imports {
		delete(want, imp)
	}
	if len(want) > 0 {
		t.Errorf("missing imports: %v (got %v)", want, ast.Imports)
	}
}

func TestPythonExtractor_Symbols(t *testing.T) {
	ext := &PythonExtractor{}
	ast := ext.Extract(samplePython, "example.py")

	byName := make(map[string]CodeSymbol)
	for _, s := range ast.Symbols {
		byName[s.Name] = s
	}

	tests := []struct {
		name     string
		wantKind SymbolKind
		exported bool
	}{
		{"MyClass", KindClass, true},
		{"__init__", KindMethod, false},
		{"public_method", KindMethod, true},
		{"_private_method", KindMethod, false},
		{"top_level_func", KindFunction, true},
		{"async_func", KindFunction, true},
		{"_private_func", KindFunction, false},
		{"MY_CONSTANT", KindVariable, true},
		{"_private_var", KindVariable, false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			sym, ok := byName[tc.name]
			if !ok {
				t.Fatalf("symbol %q not found; got symbols: %v", tc.name, pySymbolNames(ast.Symbols))
			}
			if sym.Kind != tc.wantKind {
				t.Errorf("kind: got %q, want %q", sym.Kind, tc.wantKind)
			}
			if sym.Exported != tc.exported {
				t.Errorf("exported: got %v, want %v", sym.Exported, tc.exported)
			}
		})
	}
}

func TestPythonExtractor_Exports(t *testing.T) {
	ext := &PythonExtractor{}
	ast := ext.Extract(samplePython, "example.py")
	exportSet := make(map[string]bool)
	for _, e := range ast.Exports {
		exportSet[e] = true
	}
	// Top-level exported class and function should be in exports.
	if !exportSet["MyClass"] {
		t.Error("expected MyClass in exports")
	}
	if !exportSet["top_level_func"] {
		t.Error("expected top_level_func in exports")
	}
	// Private names should not be exported.
	if exportSet["_private_func"] {
		t.Error("expected _private_func not in exports")
	}
}

func TestPythonExtractor_Decorator(t *testing.T) {
	ext := &PythonExtractor{}
	ast := ext.Extract(samplePython, "example.py")
	byName := make(map[string]CodeSymbol)
	for _, s := range ast.Symbols {
		byName[s.Name] = s
	}
	if _, ok := byName["decorator_func"]; !ok {
		t.Error("expected decorator_func symbol from @decorator_func")
	}
}

func TestPythonExtractor_SkipComments(t *testing.T) {
	src := `# this is a comment
def real_func():
    pass
`
	ext := &PythonExtractor{}
	ast := ext.Extract(src, "skip.py")
	for _, s := range ast.Symbols {
		if s.Name == "this" || s.Name == "comment" {
			t.Errorf("comment text leaked as symbol: %+v", s)
		}
	}
}

func TestPythonExtractor_Signature(t *testing.T) {
	src := `def greet(name: str, greeting: str = "hello") -> str:
    return greeting + name
`
	ext := &PythonExtractor{}
	ast := ext.Extract(src, "sig.py")
	if len(ast.Symbols) == 0 {
		t.Fatal("no symbols extracted")
	}
	sym := ast.Symbols[0]
	if sym.Signature == "" {
		t.Error("expected non-empty signature")
	}
}

// pySymbolNames returns a list of symbol names from a slice (for diagnostics).
func pySymbolNames(syms []CodeSymbol) []string {
	out := make([]string, len(syms))
	for i, s := range syms {
		out[i] = s.Name
	}
	return out
}

// TestPythonExtractor_LineIsDeclarationKeywordLine pins 1-based line
// attribution at the def/class keyword line.
func TestPythonExtractor_LineIsDeclarationKeywordLine(t *testing.T) {
	src := `import os


class Greeter:
    def greet(self, name):
        return name


def greet_user(name):
    return name
`
	ext := &PythonExtractor{}
	ast := ext.Extract(src, "svc.py")

	cls := findSymbol(ast.Symbols, "Greeter", KindClass)
	if cls == nil {
		t.Fatal("missing class Greeter")
	}
	if cls.Line != 4 {
		t.Errorf("class Line = %d, want 4 (1-based keyword line)", cls.Line)
	}
	fn := findSymbol(ast.Symbols, "greet_user", KindFunction)
	if fn == nil {
		t.Fatal("missing function greet_user")
	}
	if fn.Line != 9 {
		t.Errorf("function Line = %d, want 9", fn.Line)
	}
}
