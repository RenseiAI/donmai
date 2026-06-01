package codesurvival

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// TS/JS reachability runs a baked ts-morph node script as a subprocess. The Go
// side resolves the script + node, invokes it over the clone, and parses a JSON
// reachability report from stdout.
//
// SUBPROCESS INTERFACE (argv / stdout JSON contract):
//
//	node <script.js> --repo <repoPath> [--files <a.ts,b.tsx,...>] \
//	     [--timeout-ms 60000] [--max-files 50000]
//
// stdout: a single JSON object —
//
//	{
//	  "status": "ok" | "partial",         // partial = timeout/file-cap/parse-fail
//	  "language": "ts",
//	  "symbols": [
//	    { "file": "app/api/x/route.ts", "symbol": "GET",
//	      "startLine": 12, "endLine": 40, "reachable": "hot" | "cold" | "unknown" }
//	  ]
//	}
//
// file paths are repo-relative with forward slashes (matching git blame). The
// script exits 0 even on partial; a non-zero exit OR unparseable stdout is a
// crash → the Go side degrades to partial with no spans.

// tsReachabilityTimeout is the default hard wall for the ts-morph subprocess.
// Monorepo-scale projects can take a while; on exceed the script self-reports
// status:"partial" and the Go ctx deadline force-kills as a backstop.
const tsReachabilityTimeout = 60 * time.Second

// tsMaxFiles caps how many source files the ts-morph project loads before it
// reports partial. Keeps a 50k-file monorepo from OOM-ing the box.
const tsMaxFiles = 50000

// tsScriptEnv lets the worker image point at the baked script + node_modules.
// Defaults resolve the script relative to the binary's known bake location.
const (
	envTSReachabilityScript = "CODE_SURVIVAL_TS_REACHABILITY_SCRIPT"
	envNodeBin              = "CODE_SURVIVAL_NODE_BIN"
)

// tsReachabilityReport is the JSON the node script prints to stdout.
type tsReachabilityReport struct {
	Status   string            `json:"status"`
	Language string            `json:"language"`
	Symbols  []tsReachableSpan `json:"symbols"`
	Error    string            `json:"error,omitempty"`
}

type tsReachableSpan struct {
	File      string `json:"file"`
	Symbol    string `json:"symbol"`
	StartLine int    `json:"startLine"`
	EndLine   int    `json:"endLine"`
	Reachable string `json:"reachable"`
}

// tsRunner abstracts the node subprocess invocation so tests drive a golden
// fixture instead of spawning node. Production uses execTSRunner.
type tsRunner interface {
	run(ctx context.Context, scriptPath, repoPath string, files []string) ([]byte, error)
	// available reports whether node + the baked script are present. When false,
	// the executor skips the TS pass and degrades (toolchain absent → partial).
	available() (scriptPath string, ok bool)
}

// execTSRunner is the production tsRunner: it locates node + the baked script
// and runs them with the documented argv.
type execTSRunner struct{}

func (execTSRunner) available() (string, bool) {
	script := os.Getenv(envTSReachabilityScript)
	if script == "" {
		// Default bake location (see worker/Dockerfile). Resolve relative to the
		// donmai binary so a relocated image still finds it.
		if exe, err := os.Executable(); err == nil {
			script = filepath.Join(filepath.Dir(exe), "..", "lib", "code-survival", "ts-morph", "reachability.js")
		}
	}
	if script == "" {
		return "", false
	}
	//nolint:gosec // G703/G304: script path is an operator-controlled bake
	// location (env var or the binary's known sibling dir), not user input.
	if _, err := os.Stat(script); err != nil {
		return "", false
	}
	if _, err := exec.LookPath(nodeBin()); err != nil {
		return "", false
	}
	return script, true
}

func (execTSRunner) run(ctx context.Context, scriptPath, repoPath string, files []string) ([]byte, error) {
	args := []string{
		scriptPath,
		"--repo", repoPath,
		"--timeout-ms", strconv.Itoa(int(tsReachabilityTimeout / time.Millisecond)),
		"--max-files", strconv.Itoa(tsMaxFiles),
	}
	if len(files) > 0 {
		args = append(args, "--files", strings.Join(files, ","))
	}
	//nolint:gosec // G204: nodeBin() is a fixed binary name; args are built from
	// validated repo-relative paths + numeric constants above.
	cmd := exec.CommandContext(ctx, nodeBin(), args...)
	cmd.Dir = repoPath
	out, err := cmd.Output()
	return out, err
}

func nodeBin() string {
	if b := os.Getenv(envNodeBin); b != "" {
		return b
	}
	return "node"
}

// nodeToolchainVersion reports the node version present at scan time, for
// ScanExecutorInfo.toolchains.node. Returns "" when node is absent (degrade
// path). The leading "v" from `node --version` is stripped.
func nodeToolchainVersion(ctx context.Context) string {
	bin := nodeBin()
	if _, err := exec.LookPath(bin); err != nil {
		return ""
	}
	//nolint:gosec // G204: bin is a fixed/env-pinned node binary, no user args.
	out, err := exec.CommandContext(ctx, bin, "--version").Output()
	if err != nil {
		return ""
	}
	return strings.TrimPrefix(strings.TrimSpace(string(out)), "v")
}

// tsFiles is the subset of survivingByFile keys that are TS/JS source.
func tsFiles(survivingByFile map[string][]int) []string {
	var out []string
	for f := range survivingByFile {
		switch {
		case strings.HasSuffix(f, ".ts"),
			strings.HasSuffix(f, ".tsx"),
			strings.HasSuffix(f, ".js"),
			strings.HasSuffix(f, ".jsx"),
			strings.HasSuffix(f, ".mjs"),
			strings.HasSuffix(f, ".cjs"):
			out = append(out, f)
		}
	}
	return out
}

// analyzeTSReachability invokes the baked ts-morph script over the clone and
// maps its report to reachabilityResult. Degrades to partial (no down-weight)
// when node/script absent, on subprocess crash/timeout, or when the script
// itself reports partial. Survival is never affected.
func analyzeTSReachability(ctx context.Context, log *slog.Logger, runner tsRunner, repoPath string, survivingByFile map[string][]int) reachabilityResult {
	res := reachabilityResult{language: "ts"}
	targets := tsFiles(survivingByFile)
	if len(targets) == 0 {
		return res // nothing TS to classify
	}

	script, ok := runner.available()
	if !ok {
		log.Warn("code-survival: ts reachability toolchain absent (node/script); degrading")
		res.partial = true
		return res
	}

	runCtx, cancel := context.WithTimeout(ctx, tsReachabilityTimeout)
	defer cancel()

	out, err := runner.run(runCtx, script, repoPath, targets)
	if err != nil {
		log.Warn("code-survival: ts reachability subprocess failed; degrading", "err", err)
		res.partial = true
		return res
	}

	report, perr := parseTSReport(out)
	if perr != nil {
		log.Warn("code-survival: ts reachability produced unparseable output; degrading", "err", perr)
		res.partial = true
		return res
	}
	if report.Status != "ok" {
		res.partial = true
	}
	for _, s := range report.Symbols {
		res.spans = append(res.spans, symbolSpan{
			file:      s.File,
			symbol:    s.Symbol,
			startLine: s.StartLine,
			endLine:   s.EndLine,
			reachable: normalizeReachable(s.Reachable),
		})
	}
	return res
}

// parseTSReport decodes the node script's stdout. It tolerates leading/trailing
// whitespace and a trailing newline but rejects anything that is not a single
// JSON object.
func parseTSReport(out []byte) (tsReachabilityReport, error) {
	trimmed := strings.TrimSpace(string(out))
	if trimmed == "" {
		return tsReachabilityReport{}, errors.New("codesurvival: empty ts reachability output")
	}
	var r tsReachabilityReport
	if err := json.Unmarshal([]byte(trimmed), &r); err != nil {
		return tsReachabilityReport{}, err
	}
	return r, nil
}

// normalizeReachable maps the script's reachable string onto the contract enum,
// defaulting unknown for any unrecognised value (weighted as hot, no down-weight).
func normalizeReachable(s string) SymbolReachability {
	switch SymbolReachability(s) {
	case ReachableHot:
		return ReachableHot
	case ReachableCold:
		return ReachableCold
	default:
		return ReachableUnknown
	}
}
