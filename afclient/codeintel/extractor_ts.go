package codeintel

import (
	"path/filepath"
	"regexp"
	"strings"
)

// TypeScriptExtractor is a pure-regex symbol extractor for TypeScript and
// JavaScript source files. It is a faithful port of the TS
// TypeScriptExtractor class from
// donmai-libraries/packages/code-intelligence/src/parser/typescript-extractor.ts.
//
// The extractor intentionally avoids tree-sitter or any CGo dependency; the
// same regex patterns are used on both sides to ensure byte-identical symbol
// output for a given source file.
type TypeScriptExtractor struct{}

// Extract parses source (TypeScript or JavaScript) and returns a FileAST.
// filePath is used only to derive the language tag and to populate
// symbol.filePath — no file I/O is performed.
func (e *TypeScriptExtractor) Extract(source, filePath string) FileAST {
	ext := strings.ToLower(filepath.Ext(filePath))
	language := "typescript"
	if ext == ".js" || ext == ".jsx" || ext == ".mjs" || ext == ".cjs" {
		language = "javascript"
	}

	lines := strings.Split(source, "\n")
	var symbols []CodeSymbol
	var imports []string
	var exports []string

	var currentJSDoc string
	inBlockComment := false

	for i, line := range lines {
		trimmed := strings.TrimSpace(line)

		// Track JSDoc / block comments (/**, /*…*/).
		if strings.HasPrefix(trimmed, "/**") {
			endIdx := strings.Index(source[lineOffset(lines, i):], "*/")
			if endIdx != -1 {
				commentStart := lineOffset(lines, i)
				currentJSDoc = strings.TrimSpace(source[commentStart : commentStart+endIdx+2])
			}
			if !strings.Contains(trimmed, "*/") {
				inBlockComment = true
			}
			continue
		}
		if inBlockComment {
			if strings.Contains(trimmed, "*/") {
				inBlockComment = false
			}
			continue
		}
		if strings.HasPrefix(trimmed, "//") || strings.HasPrefix(trimmed, "/*") {
			continue
		}
		if trimmed == "" {
			continue
		}

		// Imports.
		if m := reImport.FindStringSubmatch(trimmed); m != nil {
			imports = append(imports, m[1])
			continue
		}
		if m := reDynImport.FindStringSubmatch(trimmed); m != nil {
			imports = append(imports, m[1])
			// Do not continue; may also match other patterns on the same line.
		}

		isExported := strings.HasPrefix(trimmed, "export ")
		effective := trimmed
		if isExported {
			effective = reExportPrefix.ReplaceAllString(effective, "")
		}

		// Function declarations.
		if m := reFuncDecl.FindStringSubmatch(effective); m != nil {
			name := m[1]
			sig := extractSignatureTS(effective)
			sym := makeSymbolTS(name, KindFunction, filePath, i+1, isExported, sig, currentJSDoc, language, "")
			end, ok := findFuncExtentTS(lines, i, 0)
			setEndLineTS(&sym, end, ok)
			symbols = append(symbols, sym)
			if isExported {
				exports = append(exports, name)
			}
			currentJSDoc = ""
			continue
		}

		// Arrow / const functions.
		if m := reArrowFunc.FindStringSubmatch(effective); m != nil {
			name := m[1]
			arrowIdx := strings.Index(effective, "=>")
			sig := ""
			if arrowIdx > 0 {
				sig = strings.TrimSpace(effective[:arrowIdx])
			}
			sym := makeSymbolTS(name, KindFunction, filePath, i+1, isExported, sig, currentJSDoc, language, "")
			// Scan for the body '{' AFTER the '=>' on the raw line so a
			// destructured parameter's '{' is never taken for the body; an
			// expression-body arrow (no '{') simply records no extent.
			if rawArrow := strings.Index(line, "=>"); rawArrow >= 0 {
				end, ok := findFuncExtentTS(lines, i, rawArrow+2)
				setEndLineTS(&sym, end, ok)
			}
			symbols = append(symbols, sym)
			if isExported {
				exports = append(exports, name)
			}
			currentJSDoc = ""
			continue
		}

		// const function expression.
		if m := reFuncExpr.FindStringSubmatch(effective); m != nil {
			name := m[1]
			sym := makeSymbolTS(name, KindFunction, filePath, i+1, isExported, "", currentJSDoc, language, "")
			end, ok := findFuncExtentTS(lines, i, 0)
			setEndLineTS(&sym, end, ok)
			symbols = append(symbols, sym)
			if isExported {
				exports = append(exports, name)
			}
			currentJSDoc = ""
			continue
		}

		// Class declarations.
		if m := reClassDecl.FindStringSubmatch(effective); m != nil {
			name := m[1]
			endLine := findBlockEndTS(lines, i)
			endLine1 := endLine + 1 // EndLine is 1-based like Line
			sym := makeSymbolTS(name, KindClass, filePath, i+1, isExported, strings.SplitN(effective, "{", 2)[0], currentJSDoc, language, "")
			sym.EndLine = &endLine1
			symbols = append(symbols, sym)
			if isExported {
				exports = append(exports, name)
			}
			// Extract methods/properties inside the class.
			classMembers := extractClassMembersTS(lines, i, endLine, filePath, name, language)
			symbols = append(symbols, classMembers...)
			currentJSDoc = ""
			continue
		}

		// Interface declarations.
		if m := reIfaceDecl.FindStringSubmatch(effective); m != nil {
			name := m[1]
			endLine := findBlockEndTS(lines, i)
			endLine1 := endLine + 1 // EndLine is 1-based like Line
			sym := makeSymbolTS(name, KindInterface, filePath, i+1, isExported, strings.SplitN(effective, "{", 2)[0], currentJSDoc, language, "")
			sym.EndLine = &endLine1
			symbols = append(symbols, sym)
			if isExported {
				exports = append(exports, name)
			}
			currentJSDoc = ""
			continue
		}

		// Type alias.
		if m := reTypeAlias.FindStringSubmatch(effective); m != nil {
			name := m[1]
			symbols = append(symbols, makeSymbolTS(name, KindType, filePath, i+1, isExported, "", currentJSDoc, language, ""))
			if isExported {
				exports = append(exports, name)
			}
			currentJSDoc = ""
			continue
		}

		// Enum.
		if m := reEnumDecl.FindStringSubmatch(effective); m != nil {
			name := m[1]
			symbols = append(symbols, makeSymbolTS(name, KindEnum, filePath, i+1, isExported, "", currentJSDoc, language, ""))
			if isExported {
				exports = append(exports, name)
			}
			currentJSDoc = ""
			continue
		}

		// Variable declarations (non-function).
		if m := reVarDecl.FindStringSubmatch(effective); m != nil {
			name := m[1]
			symbols = append(symbols, makeSymbolTS(name, KindVariable, filePath, i+1, isExported, "", currentJSDoc, language, ""))
			if isExported {
				exports = append(exports, name)
			}
			currentJSDoc = ""
			continue
		}

		// Decorator.
		if m := reDecorator.FindStringSubmatch(trimmed); m != nil {
			symbols = append(symbols, makeSymbolTS(m[1], KindDecorator, filePath, i+1, false, "", "", language, ""))
			continue
		}

		// Re-exports: export { A, B } from '...'
		if m := reReExport.FindStringSubmatch(trimmed); m != nil {
			imports = append(imports, m[2])
			for _, part := range strings.Split(m[1], ",") {
				parts := reWS.Split(strings.TrimSpace(part), -1)
				name := parts[len(parts)-1]
				exports = append(exports, strings.TrimSpace(name))
			}
			continue
		}

		// export * from '...'
		if m := reReExportAll.FindStringSubmatch(trimmed); m != nil {
			imports = append(imports, m[1])
			continue
		}

		// Clear JSDoc if this is a non-empty, non-comment line that didn't match.
		if len(trimmed) > 0 && !strings.HasPrefix(trimmed, "*") && !strings.HasPrefix(trimmed, "//") {
			currentJSDoc = ""
		}
	}

	return FileAST{
		FilePath: filePath,
		Language: language,
		Symbols:  symbols,
		Imports:  imports,
		Exports:  exports,
	}
}

// extractClassMembersTS scans lines[startLine+1..endLine] for method and
// property declarations inside a TypeScript class body.
func extractClassMembersTS(lines []string, startLine, endLine int, filePath, className, language string) []CodeSymbol {
	var out []CodeSymbol
	for i := startLine + 1; i <= endLine && i < len(lines); i++ {
		trimmed := strings.TrimSpace(lines[i])
		if trimmed == "{" || trimmed == "}" || trimmed == "" {
			continue
		}
		// Method.
		if m := reClassMethod.FindStringSubmatch(trimmed); m != nil {
			name := m[1]
			if !isControlKeyword(name) {
				sym := makeSymbolTS(name, KindMethod, filePath, i+1, false, "", "", language, className)
				// Body extent; a bodyless overload signature (terminated by
				// ';') records none.
				end, ok := findFuncExtentTS(lines, i, 0)
				setEndLineTS(&sym, end, ok)
				out = append(out, sym)
				continue
			}
		}
		// Property.
		if m := reClassProp.FindStringSubmatch(trimmed); m != nil {
			name := m[1]
			if !isControlKeyword(name) {
				out = append(out, makeSymbolTS(name, KindProperty, filePath, i+1, false, "", "", language, className))
			}
		}
	}
	return out
}

// isControlKeyword returns true for TS/JS keywords that look like identifiers
// but are not valid method/property names in the contexts we match.
func isControlKeyword(name string) bool {
	switch name {
	case "if", "for", "while", "switch", "return", "new":
		return true
	}
	return false
}

func makeSymbolTS(name string, kind SymbolKind, filePath string, line int, exported bool, sig, doc, language, parentName string) CodeSymbol {
	s := CodeSymbol{
		Name:     name,
		Kind:     kind,
		FilePath: filePath,
		Line:     line,
		Exported: exported,
	}
	if sig != "" {
		s.Signature = strings.TrimSpace(sig)
	}
	if doc != "" {
		s.Documentation = doc
	}
	if language != "" {
		s.Language = language
	}
	if parentName != "" {
		s.ParentName = parentName
	}
	return s
}

// extractSignatureTS extracts a compact function signature from a declaration
// line, including any explicit return type annotation.
func extractSignatureTS(line string) string {
	parenEnd := strings.Index(line, ")")
	if parenEnd == -1 {
		idx := strings.Index(line, "{")
		if idx == -1 {
			return strings.TrimSpace(line)
		}
		return strings.TrimSpace(line[:idx])
	}
	afterParen := line[parenEnd+1:]
	if m := reReturnType.FindStringSubmatch(afterParen); m != nil {
		return strings.TrimSpace(line[:parenEnd+1]) + ": " + strings.TrimSpace(m[1])
	}
	return strings.TrimSpace(line[:parenEnd+1])
}

// findBlockEndTS returns the line index of the closing brace that matches the
// opening brace on or after startLine, or a capped estimate if not found.
// Brace counting is string/comment-aware (scanBlockExtent), so a '}' inside a
// string literal, template literal, or comment never closes the block early.
func findBlockEndTS(lines []string, startLine int) int {
	if end, ok := scanBlockExtent(lines, startLine, scanTS, blockScanOpts{noLookaheadCap: true}); ok {
		return end
	}
	end := startLine + 100
	if end >= len(lines) {
		end = len(lines) - 1
	}
	return end
}

// findFuncExtentTS returns the 0-based closing-brace line of the function
// body opening on (or shortly after) startLine, scanning from startCol on
// that line. ok=false for bodyless forms — overload signatures and declare
// stubs (a code-context ';' terminates the declaration before any body '{')
// and expression-body arrow functions — which get no extent rather than a
// wrong one.
func findFuncExtentTS(lines []string, startLine, startCol int) (int, bool) {
	return scanBlockExtent(lines, startLine, scanTS, blockScanOpts{stopAtSemi: true, startCol: startCol})
}

// setEndLineTS stamps sym.EndLine (1-based, like Line) from a 0-based scan
// result when the body was found.
func setEndLineTS(sym *CodeSymbol, end int, ok bool) {
	if !ok {
		return
	}
	endLine1 := end + 1
	sym.EndLine = &endLine1
}

// lineOffset returns the byte offset in the joined source string at which
// lines[lineIdx] begins.
func lineOffset(lines []string, lineIdx int) int {
	offset := 0
	for i := 0; i < lineIdx && i < len(lines); i++ {
		offset += len(lines[i]) + 1 // +1 for the newline
	}
	return offset
}

// ── Compiled regexps (mirrors the TS patterns) ────────────────────────────────

var (
	// import ... from '...'
	reImport = regexp.MustCompile(`(?s)^import\s+(?:(?:type\s+)?(?:\{[^}]*\}|[\w*]+(?:\s+as\s+\w+)?)\s+from\s+)?['"]([^'"]+)['"]`)
	// import('...')
	reDynImport = regexp.MustCompile(`import\(['"]([^'"]+)['"]\)`)
	// strip 'export (default )?'
	reExportPrefix = regexp.MustCompile(`^export\s+(?:default\s+)?`)
	// function foo(
	reFuncDecl = regexp.MustCompile(`^(?:async\s+)?function\s*\*?\s+(\w+)`)
	// const/let/var foo = ... =>
	reArrowFunc = regexp.MustCompile(`^(?:const|let|var)\s+(\w+)\s*(?::\s*[^=]+)?\s*=\s*(?:async\s+)?(?:\([^)]*\)|[^=])\s*=>`)
	// const/let/var foo = ... function
	reFuncExpr = regexp.MustCompile(`^(?:const|let|var)\s+(\w+)\s*(?::\s*[^=]+)?\s*=\s*(?:async\s+)?function`)
	// class Foo
	reClassDecl = regexp.MustCompile(`^(?:abstract\s+)?class\s+(\w+)`)
	// interface Foo
	reIfaceDecl = regexp.MustCompile(`^interface\s+(\w+)`)
	// type Foo
	reTypeAlias = regexp.MustCompile(`^type\s+(\w+)`)
	// (const) enum Foo
	reEnumDecl = regexp.MustCompile(`^(?:const\s+)?enum\s+(\w+)`)
	// const/let/var foo =
	reVarDecl = regexp.MustCompile(`^(?:const|let|var)\s+(\w+)\s*(?::\s*[^=]+)?\s*=`)
	// @Decorator
	reDecorator = regexp.MustCompile(`^@(\w+)`)
	// export { A, B } from '...'
	reReExport = regexp.MustCompile(`^export\s+\{([^}]+)\}\s+from\s+['"]([^'"]+)['"]`)
	// export * from '...'
	reReExportAll = regexp.MustCompile(`^export\s+\*\s+from\s+['"]([^'"]+)['"]`)
	// class member method: (public|private|protected|static|async|...)*  name(
	reClassMethod = regexp.MustCompile(`^(?:(?:public|private|protected|static|async|abstract|override|readonly)\s+)*(\w+)\s*(?:<[^>]*)?\(`)
	// class member property: (public|...) name [?!]: or name =
	reClassProp = regexp.MustCompile(`^(?:(?:public|private|protected|static|readonly|abstract|override)\s+)*(\w+)\s*[?!]?\s*[:=]`)
	// return type annotation: ': SomeType'
	reReturnType = regexp.MustCompile(`^\s*:\s*([^{]+)`)
	// whitespace splitter
	reWS = regexp.MustCompile(`\s+`)
)
