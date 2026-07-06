package codeintel

import (
	"testing"
)

// sampleTS is a representative TypeScript source file used across multiple
// test cases. It exercises all patterns the TypeScriptExtractor handles.
const sampleTS = `import { Foo } from './foo'
import type { Bar } from './bar'
import('dynamic')

/**
 * MyClass is an example class.
 */
export class MyClass extends Foo {
  private value: number = 0
  readonly name: string = 'test'

  constructor(name: string) {
    super()
  }

  public doWork(input: string): boolean {
    return input.length > 0
  }
}

export interface MyInterface {
  id: string
  run(): void
}

export type MyType = string | number

export enum Direction {
  Up = 'UP',
  Down = 'DOWN',
}

/** myFunc does work */
export async function myFunc(x: number): Promise<void> {
  return
}

export const arrowFn = async (x: string): Promise<boolean> => {
  return x.length > 0
}

export const funcExpr = function() {
  return 42
}

export const myVar = 'hello'

@Injectable()
class DecoratedClass {}

export { MyClass as Renamed } from './other'
`

func TestTypeScriptExtractor_BasicExtraction(t *testing.T) {
	ext := &TypeScriptExtractor{}
	ast := ext.Extract(sampleTS, "src/sample.ts")

	if ast.Language != "typescript" {
		t.Errorf("language: got %q, want %q", ast.Language, "typescript")
	}
	if ast.FilePath != "src/sample.ts" {
		t.Errorf("filePath: got %q, want %q", ast.FilePath, "src/sample.ts")
	}

	// All expected imports must have been found.
	importsFound := make(map[string]bool)
	for _, imp := range ast.Imports {
		importsFound[imp] = true
	}
	for _, exp := range []string{"./foo", "./bar", "dynamic", "./other"} {
		if !importsFound[exp] {
			t.Errorf("missing import %q; got %v", exp, ast.Imports)
		}
	}
}

func TestTypeScriptExtractor_Symbols(t *testing.T) {
	ext := &TypeScriptExtractor{}
	ast := ext.Extract(sampleTS, "src/sample.ts")

	// Build a lookup by name+kind.
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
		{"MyClass", KindClass, true, "typescript"},
		{"MyInterface", KindInterface, true, "typescript"},
		{"MyType", KindType, true, "typescript"},
		{"Direction", KindEnum, true, "typescript"},
		{"myFunc", KindFunction, true, "typescript"},
		// arrowFn with explicit return type annotation `: Promise<boolean>` between
		// `)` and `=>` does not match the arrow-function regex; it falls through to
		// the variable pattern. This mirrors the TS extractor behaviour for the same
		// edge case.
		{"arrowFn", KindVariable, true, "typescript"},
		{"funcExpr", KindFunction, true, "typescript"},
		{"myVar", KindVariable, true, "typescript"},
		{"Injectable", KindDecorator, false, "typescript"},
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

func TestTypeScriptExtractor_ClassMembers(t *testing.T) {
	ext := &TypeScriptExtractor{}
	ast := ext.Extract(sampleTS, "src/sample.ts")

	// Methods and properties inside MyClass should appear with parentName="MyClass".
	doWork := findSymbol(ast.Symbols, "doWork", KindMethod)
	if doWork == nil {
		t.Fatal("missing method doWork")
	}
	if doWork.ParentName != "MyClass" {
		t.Errorf("doWork.parentName: got %q, want %q", doWork.ParentName, "MyClass")
	}
}

func TestTypeScriptExtractor_Documentation(t *testing.T) {
	ext := &TypeScriptExtractor{}
	ast := ext.Extract(sampleTS, "src/sample.ts")

	myFunc := findSymbol(ast.Symbols, "myFunc", KindFunction)
	if myFunc == nil {
		t.Fatal("missing symbol myFunc")
	}
	if myFunc.Documentation == "" {
		t.Error("myFunc.documentation: expected non-empty JSDoc")
	}
}

func TestTypeScriptExtractor_Exports(t *testing.T) {
	ext := &TypeScriptExtractor{}
	ast := ext.Extract(sampleTS, "src/sample.ts")

	if len(ast.Exports) == 0 {
		t.Error("expected non-empty exports list")
	}
	exportSet := make(map[string]bool, len(ast.Exports))
	for _, e := range ast.Exports {
		exportSet[e] = true
	}
	for _, name := range []string{"MyClass", "MyInterface", "MyType", "Direction", "myFunc", "arrowFn", "funcExpr", "myVar"} {
		if !exportSet[name] {
			t.Errorf("missing export %q; got %v", name, ast.Exports)
		}
	}
}

func TestTypeScriptExtractor_JavaScript(t *testing.T) {
	src := `const add = (a, b) => a + b
module.exports = { add }
`
	ext := &TypeScriptExtractor{}
	ast := ext.Extract(src, "lib/util.js")
	if ast.Language != "javascript" {
		t.Errorf("language: got %q, want javascript", ast.Language)
	}
}

func TestTypeScriptExtractor_EmptyFile(t *testing.T) {
	ext := &TypeScriptExtractor{}
	ast := ext.Extract("", "empty.ts")
	if len(ast.Symbols) != 0 {
		t.Errorf("expected 0 symbols for empty file, got %d", len(ast.Symbols))
	}
}

func TestTypeScriptExtractor_ReExports(t *testing.T) {
	src := `export { Alpha, Beta } from './alpha'
export * from './gamma'
`
	ext := &TypeScriptExtractor{}
	ast := ext.Extract(src, "index.ts")
	if !contains(ast.Imports, "./alpha") {
		t.Errorf("expected import ./alpha; got %v", ast.Imports)
	}
	if !contains(ast.Imports, "./gamma") {
		t.Errorf("expected import ./gamma; got %v", ast.Imports)
	}
	if !contains(ast.Exports, "Alpha") {
		t.Errorf("expected export Alpha; got %v", ast.Exports)
	}
}

// ── helpers ───────────────────────────────────────────────────────────────────

func symbolNames(syms []CodeSymbol) []string {
	names := make([]string, len(syms))
	for i, s := range syms {
		names[i] = string(s.Kind) + ":" + s.Name
	}
	return names
}

func findSymbol(syms []CodeSymbol, name string, kind SymbolKind) *CodeSymbol {
	for i := range syms {
		if syms[i].Name == name && syms[i].Kind == kind {
			return &syms[i]
		}
	}
	return nil
}

func contains(ss []string, s string) bool {
	for _, v := range ss {
		if v == s {
			return true
		}
	}
	return false
}

// TestTypeScriptExtractor_LineIsDeclarationKeywordLine pins 1-based line
// attribution at the declaration keyword for TS symbols, including class
// members and EndLine (the closing-brace line).
func TestTypeScriptExtractor_LineIsDeclarationKeywordLine(t *testing.T) {
	src := `/**
 * GreetingService provides greetings.
 * Multi-line JSDoc so doc start and keyword line differ.
 */
export class GreetingService {
  greet(name: string): string {
    return name
  }
}

export function greetUser(name: string): string {
  return name
}
`
	ext := &TypeScriptExtractor{}
	ast := ext.Extract(src, "svc.ts")

	cls := findSymbol(ast.Symbols, "GreetingService", KindClass)
	if cls == nil {
		t.Fatal("missing class GreetingService")
	}
	if cls.Line != 5 {
		t.Errorf("class Line = %d, want 5 (1-based keyword line)", cls.Line)
	}
	if cls.EndLine == nil || *cls.EndLine != 9 {
		t.Errorf("class EndLine = %v, want 9 (1-based closing-brace line)", cls.EndLine)
	}
	method := findSymbol(ast.Symbols, "greet", KindMethod)
	if method == nil {
		t.Fatal("missing method greet")
	}
	if method.Line != 6 {
		t.Errorf("method Line = %d, want 6", method.Line)
	}
	fn := findSymbol(ast.Symbols, "greetUser", KindFunction)
	if fn == nil {
		t.Fatal("missing function greetUser")
	}
	if fn.Line != 11 {
		t.Errorf("function Line = %d, want 11", fn.Line)
	}
}
