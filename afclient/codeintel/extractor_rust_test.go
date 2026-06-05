package codeintel

import (
	"testing"
)

// sampleRust is a representative Rust source file used across test cases.
const sampleRust = `use std::collections::HashMap;
use crate::utils::helper;

/// Public function with doc comment.
pub fn public_func(x: u32) -> u32 {
    x + 1
}

/// Private function.
fn private_func() {}

/// A public struct.
pub struct MyStruct {
    pub field: String,
}

/// A private struct.
struct InternalStruct {}

/// A public enum.
pub enum MyEnum {
    Variant1,
    Variant2,
}

/// A public trait.
pub trait MyTrait {
    fn do_something(&self);
}

/// Trait implementation.
impl MyTrait for MyStruct {
    fn do_something(&self) {}
}

/// A macro definition.
macro_rules! my_macro {
    () => {};
}

pub const MY_CONST: u32 = 42;
static PRIVATE_STATIC: &str = "hidden";

pub type MyAlias = String;

pub mod submodule;
`

func TestRustExtractor_Language(t *testing.T) {
	ext := &RustExtractor{}
	ast := ext.Extract(sampleRust, "src/lib.rs")
	if ast.Language != "rust" {
		t.Errorf("language: got %q, want rust", ast.Language)
	}
	if ast.FilePath != "src/lib.rs" {
		t.Errorf("filePath: got %q, want src/lib.rs", ast.FilePath)
	}
}

func TestRustExtractor_Imports(t *testing.T) {
	ext := &RustExtractor{}
	ast := ext.Extract(sampleRust, "src/lib.rs")
	want := map[string]bool{
		"std::collections::HashMap": true,
		"crate::utils::helper":      true,
	}
	for _, imp := range ast.Imports {
		delete(want, imp)
	}
	if len(want) > 0 {
		t.Errorf("missing imports: %v (got %v)", want, ast.Imports)
	}
}

func TestRustExtractor_Symbols(t *testing.T) {
	ext := &RustExtractor{}
	ast := ext.Extract(sampleRust, "src/lib.rs")

	byName := make(map[string]CodeSymbol)
	for _, s := range ast.Symbols {
		byName[s.Name] = s
	}

	tests := []struct {
		name     string
		wantKind SymbolKind
		exported bool
	}{
		{"public_func", KindFunction, true},
		{"private_func", KindFunction, false},
		{"MyStruct", KindStruct, true},
		{"InternalStruct", KindStruct, false},
		{"MyEnum", KindEnum, true},
		{"MyTrait", KindTrait, true},
		{"MY_CONST", KindVariable, true},
		{"PRIVATE_STATIC", KindVariable, false},
		{"MyAlias", KindType, true},
		{"submodule", KindModule, true},
		{"my_macro", KindMacro, false}, // macro_rules! is not pub by default
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			sym, ok := byName[tc.name]
			if !ok {
				t.Fatalf("symbol %q not found; got: %v", tc.name, pySymbolNames(ast.Symbols))
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

func TestRustExtractor_ImplBlock(t *testing.T) {
	ext := &RustExtractor{}
	ast := ext.Extract(sampleRust, "src/lib.rs")
	byName := make(map[string]CodeSymbol)
	for _, s := range ast.Symbols {
		byName[s.Name] = s
	}
	// impl MyTrait for MyStruct
	if _, ok := byName["MyTrait for MyStruct"]; !ok {
		t.Error("expected impl block 'MyTrait for MyStruct'")
	}
}

func TestRustExtractor_DocComment(t *testing.T) {
	src := `/// The answer.
pub fn answer() -> u32 {
    42
}
`
	ext := &RustExtractor{}
	ast := ext.Extract(src, "lib.rs")
	if len(ast.Symbols) == 0 {
		t.Fatal("no symbols")
	}
	sym := ast.Symbols[0]
	if sym.Documentation == "" {
		t.Error("expected non-empty documentation from doc comment")
	}
	if sym.Signature == "" {
		t.Error("expected non-empty signature")
	}
}

func TestRustExtractor_Exports(t *testing.T) {
	ext := &RustExtractor{}
	ast := ext.Extract(sampleRust, "src/lib.rs")
	exportSet := make(map[string]bool)
	for _, e := range ast.Exports {
		exportSet[e] = true
	}
	if !exportSet["public_func"] {
		t.Error("expected public_func in exports")
	}
	if exportSet["private_func"] {
		t.Error("expected private_func not in exports")
	}
}
