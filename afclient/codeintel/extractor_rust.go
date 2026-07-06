package codeintel

import (
	"regexp"
	"strings"
)

// RustExtractor is a pure-regex symbol extractor for Rust source files.
// It is a faithful port of the TS RustExtractor class from
// donmai-libraries/packages/code-intelligence/src/parser/rust-extractor.ts.
type RustExtractor struct{}

// Extract parses Rust source and returns a FileAST.
func (e *RustExtractor) Extract(source, filePath string) FileAST {
	lines := strings.Split(source, "\n")
	var symbols []CodeSymbol
	var imports []string
	var exports []string

	// docBuf accumulates consecutive /// doc lines. strings.Builder (not
	// `s += ...`) keeps accumulation O(n) rather than O(n^2), so a huge doc
	// block cannot hang indexing for seconds. docString()/resetDoc() read/clear.
	var docBuf strings.Builder
	docString := docBuf.String
	resetDoc := docBuf.Reset

	for i, line := range lines {
		trimmed := strings.TrimSpace(line)

		// Doc-comments (///).
		if strings.HasPrefix(trimmed, "///") {
			body := strings.TrimSpace(trimmed[3:])
			if docBuf.Len() > 0 {
				docBuf.WriteByte('\n')
			}
			docBuf.WriteString(body)
			continue
		}

		// Regular comments: skip.
		if strings.HasPrefix(trimmed, "//") {
			continue
		}

		// Use statements (imports).
		if m := reRustUse.FindStringSubmatch(trimmed); m != nil {
			imports = append(imports, m[1])
			continue
		}

		isPublic := strings.HasPrefix(trimmed, "pub ")
		// Strip visibility prefix for the remaining pattern matches.
		effective := trimmed
		if isPublic {
			effective = reRustPubPrefix.ReplaceAllString(trimmed, "")
		}

		// Function declarations.
		if m := reRustFn.FindStringSubmatch(effective); m != nil {
			name := m[1]
			// Signature: up to (but not including) the opening brace.
			sig := effective
			if idx := strings.Index(effective, "{"); idx >= 0 {
				sig = strings.TrimSpace(effective[:idx])
			}
			sym := CodeSymbol{
				Name:      name,
				Kind:      KindFunction,
				FilePath:  filePath,
				Line:      i + 1,
				Exported:  isPublic,
				Signature: sig,
				Language:  "rust",
			}
			if docBuf.Len() > 0 {
				sym.Documentation = docString()
			}
			symbols = append(symbols, sym)
			if isPublic {
				exports = append(exports, name)
			}
			resetDoc()
			continue
		}

		// Struct declarations.
		if m := reRustStruct.FindStringSubmatch(effective); m != nil {
			name := m[1]
			sym := CodeSymbol{
				Name:     name,
				Kind:     KindStruct,
				FilePath: filePath,
				Line:     i + 1,
				Exported: isPublic,
				Language: "rust",
			}
			if docBuf.Len() > 0 {
				sym.Documentation = docString()
			}
			symbols = append(symbols, sym)
			if isPublic {
				exports = append(exports, name)
			}
			resetDoc()
			continue
		}

		// Enum declarations.
		if m := reRustEnum.FindStringSubmatch(effective); m != nil {
			name := m[1]
			sym := CodeSymbol{
				Name:     name,
				Kind:     KindEnum,
				FilePath: filePath,
				Line:     i + 1,
				Exported: isPublic,
				Language: "rust",
			}
			if docBuf.Len() > 0 {
				sym.Documentation = docString()
			}
			symbols = append(symbols, sym)
			if isPublic {
				exports = append(exports, name)
			}
			resetDoc()
			continue
		}

		// Trait declarations.
		if m := reRustTrait.FindStringSubmatch(effective); m != nil {
			name := m[1]
			sym := CodeSymbol{
				Name:     name,
				Kind:     KindTrait,
				FilePath: filePath,
				Line:     i + 1,
				Exported: isPublic,
				Language: "rust",
			}
			if docBuf.Len() > 0 {
				sym.Documentation = docString()
			}
			symbols = append(symbols, sym)
			if isPublic {
				exports = append(exports, name)
			}
			resetDoc()
			continue
		}

		// Impl blocks.
		if m := reRustImpl.FindStringSubmatch(effective); m != nil {
			traitName := m[1]
			typeName := m[2]
			name := typeName
			if traitName != "" {
				name = traitName + " for " + typeName
			}
			sym := CodeSymbol{
				Name:     name,
				Kind:     KindImpl,
				FilePath: filePath,
				Line:     i + 1,
				Exported: false,
				Language: "rust",
			}
			if docBuf.Len() > 0 {
				sym.Documentation = docString()
			}
			symbols = append(symbols, sym)
			resetDoc()
			continue
		}

		// Macro definitions.
		if m := reRustMacro.FindStringSubmatch(effective); m != nil {
			name := m[1]
			sym := CodeSymbol{
				Name:     name,
				Kind:     KindMacro,
				FilePath: filePath,
				Line:     i + 1,
				Exported: isPublic,
				Language: "rust",
			}
			if docBuf.Len() > 0 {
				sym.Documentation = docString()
			}
			symbols = append(symbols, sym)
			if isPublic {
				exports = append(exports, name)
			}
			resetDoc()
			continue
		}

		// Const / static.
		if m := reRustConst.FindStringSubmatch(effective); m != nil {
			name := m[1]
			sym := CodeSymbol{
				Name:     name,
				Kind:     KindVariable,
				FilePath: filePath,
				Line:     i + 1,
				Exported: isPublic,
				Language: "rust",
			}
			symbols = append(symbols, sym)
			if isPublic {
				exports = append(exports, name)
			}
			resetDoc()
			continue
		}

		// Type alias.
		if m := reRustType.FindStringSubmatch(effective); m != nil {
			name := m[1]
			sym := CodeSymbol{
				Name:     name,
				Kind:     KindType,
				FilePath: filePath,
				Line:     i + 1,
				Exported: isPublic,
				Language: "rust",
			}
			symbols = append(symbols, sym)
			if isPublic {
				exports = append(exports, name)
			}
			resetDoc()
			continue
		}

		// Module declarations.
		if m := reRustMod.FindStringSubmatch(effective); m != nil {
			name := m[1]
			sym := CodeSymbol{
				Name:     name,
				Kind:     KindModule,
				FilePath: filePath,
				Line:     i + 1,
				Exported: isPublic,
				Language: "rust",
			}
			symbols = append(symbols, sym)
			if isPublic {
				exports = append(exports, name)
			}
			resetDoc()
			continue
		}

		// Clear doc comment on any non-empty, non-comment, non-attribute line.
		if trimmed != "" && !strings.HasPrefix(trimmed, "#[") && !strings.HasPrefix(trimmed, "*") {
			resetDoc()
		}
	}

	return FileAST{
		FilePath: filePath,
		Language: "rust",
		Symbols:  symbols,
		Imports:  imports,
		Exports:  exports,
	}
}

// ── Compiled regexps ──────────────────────────────────────────────────────────

var (
	// use statement: (pub )? use <path>;
	reRustUse = regexp.MustCompile(`^(?:pub\s+)?use\s+(.+);`)
	// strip pub (crate) / pub(crate) / pub prefix
	reRustPubPrefix = regexp.MustCompile(`^pub\s+(?:\(crate\)\s+)?`)
	// fn / async fn / unsafe fn / async unsafe fn
	reRustFn = regexp.MustCompile(`^(?:async\s+)?(?:unsafe\s+)?fn\s+(\w+)`)
	// struct
	reRustStruct = regexp.MustCompile(`^struct\s+(\w+)`)
	// enum
	reRustEnum = regexp.MustCompile(`^enum\s+(\w+)`)
	// trait
	reRustTrait = regexp.MustCompile(`^trait\s+(\w+)`)
	// impl<T>? (Trait for )? Type
	reRustImpl = regexp.MustCompile(`^impl(?:<[^>]+>)?\s+(?:(\w+)\s+for\s+)?(\w+)`)
	// macro_rules! name
	reRustMacro = regexp.MustCompile(`^macro_rules!\s+(\w+)`)
	// const/static NAME
	reRustConst = regexp.MustCompile(`^(?:const|static)\s+(\w+)`)
	// type NAME
	reRustType = regexp.MustCompile(`^type\s+(\w+)`)
	// mod name
	reRustMod = regexp.MustCompile(`^mod\s+(\w+)`)
)
