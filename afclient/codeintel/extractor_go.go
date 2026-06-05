package codeintel

import (
	"regexp"
	"strings"
	"unicode"
)

// GoExtractor is a pure-regex symbol extractor for Go source files.
// It is a faithful port of the TS GoExtractor class from
// donmai-libraries/packages/code-intelligence/src/parser/go-extractor.ts.
type GoExtractor struct{}

// Extract parses Go source and returns a FileAST.
func (e *GoExtractor) Extract(source, filePath string) FileAST {
	lines := strings.Split(source, "\n")
	var symbols []CodeSymbol
	var imports []string
	var exports []string

	var currentComment string

	for i, line := range lines {
		trimmed := strings.TrimSpace(line)

		// Track doc-comments (// lines accumulate until a blank or non-comment line).
		if strings.HasPrefix(trimmed, "//") {
			body := strings.TrimSpace(trimmed[2:])
			if currentComment != "" {
				currentComment += "\n" + body
			} else {
				currentComment = body
			}
			continue
		}

		// Single-line import.
		if m := reGoImportSingle.FindStringSubmatch(trimmed); m != nil {
			imports = append(imports, m[1])
			continue
		}

		// Multi-line import block: import (
		if reGoImportBlock.MatchString(trimmed) {
			for j := i + 1; j < len(lines); j++ {
				il := strings.TrimSpace(lines[j])
				if il == ")" {
					break
				}
				if m := reGoImportPkg.FindStringSubmatch(il); m != nil {
					imports = append(imports, m[1])
				}
			}
			continue
		}

		// Function / method declarations.
		if m := reGoFunc.FindStringSubmatch(trimmed); m != nil {
			// m[1]=receiverVar m[2]=receiverType m[3]=funcName
			receiverType := m[2]
			name := m[3]
			kind := KindFunction
			if receiverType != "" {
				kind = KindMethod
			}
			exported := isExportedGo(name)
			sig := trimmed
			if idx := strings.Index(trimmed, "{"); idx >= 0 {
				sig = strings.TrimSpace(trimmed[:idx])
			}
			sym := CodeSymbol{
				Name:      name,
				Kind:      kind,
				FilePath:  filePath,
				Line:      i,
				Exported:  exported,
				Signature: sig,
				Language:  "go",
			}
			if currentComment != "" {
				sym.Documentation = currentComment
			}
			if receiverType != "" {
				sym.ParentName = receiverType
			}
			symbols = append(symbols, sym)
			if exported {
				exports = append(exports, name)
			}
			currentComment = ""
			continue
		}

		// Struct declaration.
		if m := reGoStruct.FindStringSubmatch(trimmed); m != nil {
			name := m[1]
			exported := isExportedGo(name)
			sym := CodeSymbol{
				Name:     name,
				Kind:     KindStruct,
				FilePath: filePath,
				Line:     i,
				Exported: exported,
				Language: "go",
			}
			if currentComment != "" {
				sym.Documentation = currentComment
			}
			symbols = append(symbols, sym)
			if exported {
				exports = append(exports, name)
			}
			currentComment = ""
			continue
		}

		// Interface declaration.
		if m := reGoIface.FindStringSubmatch(trimmed); m != nil {
			name := m[1]
			exported := isExportedGo(name)
			sym := CodeSymbol{
				Name:     name,
				Kind:     KindInterface,
				FilePath: filePath,
				Line:     i,
				Exported: exported,
				Language: "go",
			}
			if currentComment != "" {
				sym.Documentation = currentComment
			}
			symbols = append(symbols, sym)
			if exported {
				exports = append(exports, name)
			}
			currentComment = ""
			continue
		}

		// Type alias (not struct, not interface) — matched after the struct/interface
		// checks above so we only reach this branch for genuine type aliases.
		if m := reGoTypeAny.FindStringSubmatch(trimmed); m != nil {
			keyword := m[2]
			if keyword != "struct" && keyword != "interface" {
				name := m[1]
				exported := isExportedGo(name)
				sym := CodeSymbol{
					Name:     name,
					Kind:     KindType,
					FilePath: filePath,
					Line:     i,
					Exported: exported,
					Language: "go",
				}
				symbols = append(symbols, sym)
				if exported {
					exports = append(exports, name)
				}
				currentComment = ""
				continue
			}
		}

		// Variable / constant.
		if m := reGoVar.FindStringSubmatch(trimmed); m != nil {
			name := m[1]
			exported := isExportedGo(name)
			sym := CodeSymbol{
				Name:     name,
				Kind:     KindVariable,
				FilePath: filePath,
				Line:     i,
				Exported: exported,
				Language: "go",
			}
			symbols = append(symbols, sym)
			if exported {
				exports = append(exports, name)
			}
			currentComment = ""
			continue
		}

		// Clear accumulated comment on any non-empty, non-comment line.
		if trimmed != "" {
			currentComment = ""
		}
	}

	return FileAST{
		FilePath: filePath,
		Language: "go",
		Symbols:  symbols,
		Imports:  imports,
		Exports:  exports,
	}
}

// isExportedGo reports whether the given Go identifier starts with an
// upper-case Unicode letter, which is the Go convention for exported names.
func isExportedGo(name string) bool {
	if name == "" {
		return false
	}
	r := []rune(name)
	return unicode.IsUpper(r[0])
}

// ── Compiled regexps ──────────────────────────────────────────────────────────

var (
	// import "pkg"
	reGoImportSingle = regexp.MustCompile(`^import\s+"([^"]+)"`)
	// import (
	reGoImportBlock = regexp.MustCompile(`^import\s*\(`)
	// "pkg" inside import block (may have alias prefix like: alias "pkg")
	reGoImportPkg = regexp.MustCompile(`"([^"]+)"`)
	// func (r *Type) Name( or func Name(
	reGoFunc = regexp.MustCompile(`^func\s+(?:\((\w+)\s+\*?(\w+)\)\s+)?(\w+)\s*\(`)
	// type Foo struct
	reGoStruct = regexp.MustCompile(`^type\s+(\w+)\s+struct\b`)
	// type Foo interface
	reGoIface = regexp.MustCompile(`^type\s+(\w+)\s+interface\b`)
	// type Foo <anything> — matches all type declarations; callers filter out
	// struct/interface after using the more specific patterns above.
	reGoTypeAny = regexp.MustCompile(`^type\s+(\w+)\s+(\S+)`)
	// var/const Foo
	reGoVar = regexp.MustCompile(`^(?:var|const)\s+(\w+)`)
)
