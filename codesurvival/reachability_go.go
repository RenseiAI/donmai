package codesurvival

import (
	"context"
	"go/ast"
	"go/token"
	"go/types"
	"log/slog"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"

	runtimeenv "github.com/RenseiAI/donmai/runtime/env"
	"golang.org/x/tools/go/callgraph/cha"
	"golang.org/x/tools/go/callgraph/rta"
	"golang.org/x/tools/go/packages"
	"golang.org/x/tools/go/ssa"
	"golang.org/x/tools/go/ssa/ssautil"
)

// goReachableFiles is the subset of survivingByFile keys that are Go source.
func goFiles(survivingByFile map[string][]int) []string {
	var out []string
	for f := range survivingByFile {
		if strings.HasSuffix(f, ".go") {
			out = append(out, f)
		}
	}
	return out
}

// analyzeGoReachability loads the cloned module, builds an SSA call graph, seeds
// it from Go user-facing entrypoints, and resolves the set of reachable
// functions. It then maps every top-level func/method declaration in the
// surviving .go files to a line range and tags it hot (reachable) or cold (not).
//
// RTA vs CHA decision:
//   - If any `package main` with a `func main` is found, seed RTA (Rapid Type
//     Analysis) from those mains + the other entrypoints. RTA is precise: it
//     prunes methods of types never instantiated, so dead exported helpers in a
//     binary are correctly cold.
//   - If NO main package exists (a library-only repo), RTA has no natural root,
//     so fall back to CHA (Class Hierarchy Analysis) over the whole program and
//     treat any function reachable from an exported entrypoint as hot. CHA is
//     conservative (over-approximates reachability) → fewer false colds, which
//     is the safe direction (we never want to falsely down-weight live code).
//
// Entrypoint seeding (user-facing):
//   - `func main` in every `package main`
//   - exported http.Handler / http.HandlerFunc / methods named ServeHTTP
//   - cobra *cobra.Command Run / RunE function values
//
// Graceful degradation: a load error, zero packages, or zero entrypoints yields
// partial=true (the executor maps that to status:partial, hotWeighted=null).
// A file the analysis could not cover leaves its surviving lines unknown→hot.
func analyzeGoReachability(ctx context.Context, log *slog.Logger, repoPath string, survivingByFile map[string][]int) reachabilityResult {
	res := reachabilityResult{language: "go"}
	targets := goFiles(survivingByFile)
	if len(targets) == 0 {
		return res // nothing Go to classify
	}

	cfg := &packages.Config{
		Mode: packages.NeedName | packages.NeedFiles | packages.NeedSyntax |
			packages.NeedTypes | packages.NeedTypesInfo | packages.NeedDeps |
			packages.NeedImports | packages.NeedModule,
		Context: ctx,
		Dir:     repoPath,
		Tests:   false,
	}
	pkgs, err := packages.Load(cfg, "./...")
	if err != nil {
		log.Warn("code-survival: go reachability load failed; degrading", "err", err)
		res.partial = true
		return res
	}
	if packages.PrintErrors(pkgs) > 0 {
		// Type/parse errors on part of the tree: keep going with what loaded, but
		// mark partial so the executor reports status:partial.
		res.partial = true
	}
	if len(pkgs) == 0 {
		res.partial = true
		return res
	}

	prog, ssaPkgs := ssautil.AllPackages(pkgs, ssa.InstantiateGenerics)
	prog.Build()

	reachable, seeded := goReachableSet(prog, ssaPkgs)
	if !seeded {
		// No entrypoints found at all → cannot prove anything hot. Degrade.
		log.Warn("code-survival: go reachability found no entrypoints; degrading")
		res.partial = true
		return res
	}
	// Index reachable functions by their types.Object for O(1) decl lookup.
	reachableObjs := map[types.Object]bool{}
	for fn := range reachable {
		if obj := fn.Object(); obj != nil {
			reachableObjs[obj] = true
		}
	}

	// Map surviving .go files to their declared symbols and classify.
	targetSet := map[string]bool{}
	for _, f := range targets {
		targetSet[f] = true
	}
	for _, p := range pkgs {
		if p.Fset == nil {
			continue
		}
		for _, file := range p.Syntax {
			rel := relPath(repoPath, p.Fset, file)
			if rel == "" || !targetSet[rel] {
				continue
			}
			res.spans = append(res.spans, goFileSymbolSpans(p.Fset, file, rel, p, reachableObjs)...)
		}
	}
	return res
}

// goReachableSet builds the reachable-function set from the seeded entrypoints.
// Returns the set keyed by the *types.Func-backed SSA function and whether any
// entrypoint seed was found.
func goReachableSet(prog *ssa.Program, ssaPkgs []*ssa.Package) (map[*ssa.Function]bool, bool) {
	mains := ssautil.MainPackages(ssaPkgs)
	var roots []*ssa.Function
	for _, m := range mains {
		if fn := m.Func("main"); fn != nil {
			roots = append(roots, fn)
		}
		if fn := m.Func("init"); fn != nil {
			roots = append(roots, fn)
		}
	}
	// Seed user-facing handler/command entrypoints from all packages.
	roots = append(roots, extraEntrypoints(prog)...)

	reachable := map[*ssa.Function]bool{}
	if len(roots) > 0 && len(mains) > 0 {
		// RTA: precise, rooted at mains (+ handlers/commands).
		r := rta.Analyze(roots, false)
		if r != nil {
			for fn := range r.Reachable {
				reachable[fn] = true
			}
			return reachable, true
		}
	}

	// CHA fallback (library-only repo, or RTA produced nothing): conservative
	// whole-program call graph; mark everything reachable from a seed root hot.
	if len(roots) == 0 {
		return reachable, false
	}
	cg := cha.CallGraph(prog)
	cg.DeleteSyntheticNodes()
	seedNodes := map[*ssa.Function]bool{}
	for _, r := range roots {
		seedNodes[r] = true
	}
	// BFS over CHA edges from the seeds.
	queue := append([]*ssa.Function(nil), roots...)
	for len(queue) > 0 {
		fn := queue[0]
		queue = queue[1:]
		if reachable[fn] {
			continue
		}
		reachable[fn] = true
		node := cg.Nodes[fn]
		if node == nil {
			continue
		}
		for _, e := range node.Out {
			if e.Callee != nil && e.Callee.Func != nil && !reachable[e.Callee.Func] {
				queue = append(queue, e.Callee.Func)
			}
		}
	}
	return reachable, true
}

// extraEntrypoints scans every SSA function for user-facing seeds beyond main:
// exported ServeHTTP methods (http.Handler), funcs assignable to http.HandlerFunc
// (signature func(http.ResponseWriter, *http.Request)), and cobra Run/RunE
// function values (func(*cobra.Command, []string) [error]). These are roots even
// when not called from main, because the framework invokes them.
func extraEntrypoints(prog *ssa.Program) []*ssa.Function {
	var out []*ssa.Function
	for fn := range ssautil.AllFunctions(prog) {
		if fn == nil || fn.Signature == nil {
			continue
		}
		if isHandlerSignature(fn) || fn.Name() == "ServeHTTP" || isCobraRunSignature(fn) {
			out = append(out, fn)
		}
	}
	return out
}

// isHandlerSignature reports whether fn has the http.HandlerFunc shape
// func(http.ResponseWriter, *http.Request).
func isHandlerSignature(fn *ssa.Function) bool {
	sig := fn.Signature
	if sig.Params().Len() != 2 || sig.Results().Len() != 0 {
		return false
	}
	p0 := sig.Params().At(0).Type().String()
	p1 := sig.Params().At(1).Type().String()
	return p0 == "net/http.ResponseWriter" && p1 == "*net/http.Request"
}

// isCobraRunSignature reports whether fn matches cobra's Run/RunE field shape
// func(*cobra.Command, []string) or func(*cobra.Command, []string) error.
func isCobraRunSignature(fn *ssa.Function) bool {
	sig := fn.Signature
	if sig.Params().Len() != 2 {
		return false
	}
	if !strings.HasSuffix(sig.Params().At(0).Type().String(), "cobra.Command") {
		return false
	}
	if sig.Params().At(1).Type().String() != "[]string" {
		return false
	}
	return sig.Results().Len() == 0 ||
		(sig.Results().Len() == 1 && sig.Results().At(0).Type().String() == "error")
}

// goFileSymbolSpans walks a file's top-level func declarations, computes each
// one's line range, and classifies it via the reachable set. A func whose SSA
// function is in `reachable` is hot; otherwise cold. Method receivers are
// included in the symbol name (e.g. "(*T).M").
func goFileSymbolSpans(fset *token.FileSet, file *ast.File, rel string, pkg *packages.Package, reachableObjs map[types.Object]bool) []symbolSpan {
	var spans []symbolSpan
	for _, decl := range file.Decls {
		fd, ok := decl.(*ast.FuncDecl)
		if !ok || fd.Name == nil {
			continue
		}
		start := fset.Position(fd.Pos()).Line
		end := fset.Position(fd.End()).Line
		name := symbolName(fd)

		reach := ReachableCold
		if pkg.TypesInfo != nil && reachableObjs[pkg.TypesInfo.Defs[fd.Name]] {
			reach = ReachableHot
		}
		spans = append(spans, symbolSpan{
			file:      rel,
			symbol:    name,
			startLine: start,
			endLine:   end,
			reachable: reach,
		})
	}
	return spans
}

// symbolName renders a func decl's display name, prefixing the receiver type for
// methods: "Foo" or "(*T).Bar".
func symbolName(fd *ast.FuncDecl) string {
	if fd.Recv == nil || len(fd.Recv.List) == 0 {
		return fd.Name.Name
	}
	recv := recvTypeName(fd.Recv.List[0].Type)
	if recv == "" {
		return fd.Name.Name
	}
	return "(" + recv + ")." + fd.Name.Name
}

func recvTypeName(expr ast.Expr) string {
	switch t := expr.(type) {
	case *ast.StarExpr:
		return "*" + recvTypeName(t.X)
	case *ast.Ident:
		return t.Name
	case *ast.IndexExpr: // generic receiver T[P]
		return recvTypeName(t.X)
	case *ast.IndexListExpr:
		return recvTypeName(t.X)
	default:
		return ""
	}
}

// relPath returns the repo-relative path of a parsed file, matching the path
// shape git blame emits (forward slashes, relative to repoPath). Empty when the
// file is outside repoPath (a dependency).
func relPath(repoPath string, fset *token.FileSet, file *ast.File) string {
	abs := fset.Position(file.Pos()).Filename
	if abs == "" {
		return ""
	}
	rel, err := filepath.Rel(repoPath, abs)
	if err != nil || strings.HasPrefix(rel, "..") {
		return ""
	}
	return filepath.ToSlash(rel)
}

// goToolchainVersion reports the Go toolchain version present at scan time, for
// ScanExecutorInfo.toolchains.go (audit + drift detection). It prefers the
// runtime version the worker was compiled with (always present, no subprocess);
// falls back to `go version` only if needed. Returns "" when neither resolves.
func goToolchainVersion() string {
	if v := runtime.Version(); v != "" {
		return strings.TrimPrefix(v, "go")
	}
	cmd := exec.Command("go", "version") //nolint:gosec // fixed binary
	cmd.Env = runtimeenv.FilterRunnerOnly(cmd.Environ())
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	fields := strings.Fields(string(out))
	if len(fields) >= 3 {
		return strings.TrimPrefix(fields[2], "go")
	}
	return ""
}
