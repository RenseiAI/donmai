package codeintel

import (
	"regexp"
	"strings"
)

// PythonExtractor is a pure-regex symbol extractor for Python source files.
// It is a faithful port of the TS PythonExtractor class from
// donmai-libraries/packages/code-intelligence/src/parser/python-extractor.ts.
type PythonExtractor struct{}

// Extract parses Python source and returns a FileAST.
func (e *PythonExtractor) Extract(source, filePath string) FileAST {
	lines := strings.Split(source, "\n")
	var symbols []CodeSymbol
	var imports []string
	var exports []string

	var currentDocstring string

	for i, line := range lines {
		trimmed := strings.TrimSpace(line)

		// Skip comments.
		if strings.HasPrefix(trimmed, "#") {
			continue
		}

		// Single-line docstrings: """...""" or '''...'''
		if strings.HasPrefix(trimmed, `"""`) || strings.HasPrefix(trimmed, `'''`) {
			quote := trimmed[:3]
			rest := trimmed[3:]
			// Single-line docstring: ends on the same line.
			if idx := strings.Index(rest, quote); idx >= 0 {
				currentDocstring = rest[:idx]
			}
			continue
		}

		// Imports: from X import Y  /  import X
		if m := rePyImport.FindStringSubmatch(trimmed); m != nil {
			var module string
			if m[1] != "" {
				module = m[1]
			} else {
				// "import X, Y" — take first token before comma/as.
				first := strings.SplitN(m[2], ",", 2)[0]
				first = strings.SplitN(first, " as ", 2)[0]
				module = strings.TrimSpace(first)
			}
			imports = append(imports, module)
			continue
		}

		// Decorators: @name
		if m := rePyDecorator.FindStringSubmatch(trimmed); m != nil {
			symbols = append(symbols, CodeSymbol{
				Name:     m[1],
				Kind:     KindDecorator,
				FilePath: filePath,
				Line:     i,
				Exported: false,
				Language: "python",
			})
			continue
		}

		// Function definitions: def name( / async def name(
		if m := rePyFunc.FindStringSubmatch(trimmed); m != nil {
			name := m[1]
			indent := len(line) - len(strings.TrimLeft(line, " \t"))
			isMethod := indent > 0
			exported := !strings.HasPrefix(name, "_")
			kind := KindFunction
			if isMethod {
				kind = KindMethod
			}
			// Signature: everything up to (but not including) the colon.
			sig := trimmed
			if idx := strings.Index(trimmed, ":"); idx >= 0 {
				sig = trimmed[:idx]
			}
			sym := CodeSymbol{
				Name:      name,
				Kind:      kind,
				FilePath:  filePath,
				Line:      i,
				Exported:  exported,
				Signature: sig,
				Language:  "python",
			}
			if currentDocstring != "" {
				sym.Documentation = currentDocstring
				currentDocstring = ""
			}
			symbols = append(symbols, sym)
			if exported && !isMethod {
				exports = append(exports, name)
			}
			continue
		}

		// Class definitions: class Name
		if m := rePyClass.FindStringSubmatch(trimmed); m != nil {
			name := m[1]
			exported := !strings.HasPrefix(name, "_")
			sig := trimmed
			if idx := strings.Index(trimmed, ":"); idx >= 0 {
				sig = trimmed[:idx]
			}
			sym := CodeSymbol{
				Name:      name,
				Kind:      KindClass,
				FilePath:  filePath,
				Line:      i,
				Exported:  exported,
				Signature: sig,
				Language:  "python",
			}
			if currentDocstring != "" {
				sym.Documentation = currentDocstring
				currentDocstring = ""
			}
			symbols = append(symbols, sym)
			if exported {
				exports = append(exports, name)
			}
			continue
		}

		// Module-level variable assignment: NAME = ... (indent == 0)
		indent := len(line) - len(strings.TrimLeft(line, " \t"))
		if m := rePyVar.FindStringSubmatch(trimmed); m != nil && indent == 0 {
			name := m[1]
			exported := !strings.HasPrefix(name, "_")
			symbols = append(symbols, CodeSymbol{
				Name:     name,
				Kind:     KindVariable,
				FilePath: filePath,
				Line:     i,
				Exported: exported,
				Language: "python",
			})
			continue
		}
	}

	return FileAST{
		FilePath: filePath,
		Language: "python",
		Symbols:  symbols,
		Imports:  imports,
		Exports:  exports,
	}
}

// ── Compiled regexps ──────────────────────────────────────────────────────────

var (
	// from X import Y  /  import X
	rePyImport = regexp.MustCompile(`^(?:from\s+(\S+)\s+)?import\s+(.+)`)
	// @name
	rePyDecorator = regexp.MustCompile(`^@(\w+)`)
	// def name( / async def name(
	rePyFunc = regexp.MustCompile(`^(?:async\s+)?def\s+(\w+)\s*\(`)
	// class Name
	rePyClass = regexp.MustCompile(`^class\s+(\w+)`)
	// NAME = ... or NAME: Type = ...
	rePyVar = regexp.MustCompile(`^(\w+)\s*(?::\s*\w[^=]*)?\s*=`)
)
