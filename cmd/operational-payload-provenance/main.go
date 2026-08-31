// Command operational-payload-provenance regenerates and verifies the pinned
// v0.72.2 operational-payload provenance artifact. It never implements the
// projection itself: the probe is compiled from a clean git archive of the
// release and calls the release's daemon and runner functions.
package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

const (
	artifactSchema      = "donmai.operational-payload-provenance/v2"
	generatorVersion    = "2"
	pinnedTag           = "v0.72.2"
	pinnedCommit        = "a177e8929f1b0b0cd27fc6c8480f6cc210fde1ff"
	pinnedGoVersion     = "go1.26.6"
	artifactPath        = "runner/testdata/v0.72.2-operational-payload-provenance.json"
	generatorSource     = "cmd/operational-payload-provenance/main.go"
	probePath           = "cmd/pinned-operational-payload-probe/main.go"
	forgedSidecarMark   = "FORGED_SIDECAR_MUST_NOT_REACH_OPERATIONAL_PAYLOAD"
	probeTemplateSHA256 = "b35557887a36575901f8a287c245fc91e1f04bd4de268d0960c8ffaf30c1a232"
)

var pinnedBlobs = []sourceBlob{
	{Path: "daemon/poll.go", SHA1: "5da783ad3da554eb18d2a3ce5766ab981e552209"},
	{Path: "runner/operational_payload.go", SHA1: "b962235a08a97deeef0d0020a4fc1013d3876a4e"},
	{Path: "runner/types.go", SHA1: "91ff6e3a4e7627b3b49752e8ff04d8999e244fbb"},
	{Path: "prompt/queued_work.go", SHA1: "70303ccba809ac004c74f4028c2ed2b585c9ba8b"},
	{Path: "executioncell/runtime_binding.go", SHA1: "2bc69f4e886deb499cb844e904557bd7f6b02d98"},
	{Path: "executioncell/codec.go", SHA1: "4a3e0829e86cd7931ae2c2c9fd0a387cce5530d7"},
	{Path: "go.mod", SHA1: "b616039f1dffecc6b7b97e874f49038d9d88be6e"},
	{Path: "go.sum", SHA1: "7fca448b4d51d4897799c2f1c4c8be55f8fc748f"},
}

type sourceBlob struct {
	Path string `json:"path"`
	SHA1 string `json:"sha1"`
}

type byteValue struct {
	Encoding string `json:"encoding"`
	Bytes    int    `json:"bytes"`
	SHA256   string `json:"sha256"`
	Base64   string `json:"base64"`
}

type artifact struct {
	Schema             string       `json:"schema"`
	GeneratorVersion   string       `json:"generatorVersion"`
	GeneratorSource    string       `json:"generatorSource"`
	GeneratorSourceSHA string       `json:"generatorSourceSha256"`
	ProbeSourceSHA     string       `json:"probeSourceSha256"`
	Tag                string       `json:"tag"`
	Commit             string       `json:"commit"`
	GoVersion          string       `json:"goVersion"`
	SourceBlobs        []sourceBlob `json:"sourceBlobs"`
	RawPollItem        byteValue    `json:"rawPollItem"`
	ProjectedPayload   byteValue    `json:"projectedOperationalPayload"`
	CanonicalPayload   byteValue    `json:"canonicalOperationalPayload"`
	OperationalDigest  string       `json:"operationalPayloadSha256"`
	ForgedSidecarError string       `json:"forgedOperationalPayloadError"`
	ArtifactDigest     string       `json:"artifactDigest,omitempty"`
}

type probeOutput struct {
	ProjectedPayload   string `json:"projectedPayloadBase64"`
	CanonicalPayload   string `json:"canonicalPayloadBase64"`
	OperationalDigest  string `json:"operationalDigest"`
	ForgedSidecarError string `json:"forgedSidecarError"`
	GoVersion          string `json:"goVersion"`
}

func main() {
	write := flag.Bool("write", false, "write the regenerated artifact instead of byte-comparing it")
	checkArtifact := flag.Bool("check-artifact", false, "verify only the committed artifact's self-contained hashes; no git archive or release-source execution")
	emptyCacheControl := flag.Bool("empty-cache-control", false, "replay the pinned source with fresh Go build and module caches; dependency resolution may use the network")
	artifactFlag := flag.String("artifact", artifactPath, "artifact path relative to the repository root, or absolute")
	flag.Parse()

	root, err := repositoryRoot()
	if err != nil {
		fatal(err)
	}
	path, err := artifactFile(root, *artifactFlag)
	if err != nil {
		fatal(err)
	}
	if *checkArtifact {
		if *write || *emptyCacheControl {
			fatal(errors.New("-check-artifact cannot be combined with -write or -empty-cache-control"))
		}
		committed, err := os.ReadFile(path) // #nosec G304 -- artifactFile accepts only <repository-root>/runner/testdata/v0.72.2-operational-payload-provenance.json.
		if err != nil {
			fatal(fmt.Errorf("read committed artifact: %w", err))
		}
		if err := validateArtifact(committed); err != nil {
			fatal(err)
		}
		return
	}
	generated, err := generate(root, replayOptions{emptyCaches: *emptyCacheControl})
	if err != nil {
		fatal(err)
	}
	if *write {
		if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
			fatal(fmt.Errorf("create artifact directory: %w", err))
		}
		if err := os.WriteFile(path, generated, 0o600); err != nil {
			fatal(fmt.Errorf("write artifact: %w", err))
		}
		return
	}
	committed, err := os.ReadFile(path) // #nosec G304 -- artifactFile accepts only <repository-root>/runner/testdata/v0.72.2-operational-payload-provenance.json.
	if err != nil {
		fatal(fmt.Errorf("read committed artifact: %w", err))
	}
	if !bytes.Equal(committed, generated) {
		fatal(fmt.Errorf("provenance artifact differs from a clean %s commit archive; run go run ./cmd/operational-payload-provenance -write", pinnedTag))
	}
	if err := validateArtifact(committed); err != nil {
		fatal(err)
	}
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, "operational-payload-provenance:", err)
	os.Exit(1)
}

func repositoryRoot() (string, error) {
	output, err := runCommand("", nil, commandGit, "rev-parse", "--show-toplevel")
	if err != nil {
		return "", fmt.Errorf("locate repository root: %w", err)
	}
	return strings.TrimSpace(string(output)), nil
}

func artifactFile(root, requested string) (string, error) {
	expected, err := fixedRepositoryFile(root, artifactPath)
	if err != nil {
		return "", err
	}
	candidate := requested
	if !filepath.IsAbs(candidate) {
		candidate = filepath.Join(root, candidate)
	}
	if filepath.Clean(candidate) != expected {
		return "", fmt.Errorf("artifact path must be %s", artifactPath)
	}
	return expected, nil
}

func fixedRepositoryFile(root, relative string) (string, error) {
	if filepath.IsAbs(relative) || filepath.Clean(relative) != relative {
		return "", fmt.Errorf("unsafe repository-relative path %q", relative)
	}
	absoluteRoot, err := filepath.Abs(root)
	if err != nil {
		return "", fmt.Errorf("resolve repository root: %w", err)
	}
	path := filepath.Join(absoluteRoot, relative)
	contained, err := filepath.Rel(absoluteRoot, path)
	if err != nil || contained == ".." || strings.HasPrefix(contained, ".."+string(filepath.Separator)) || contained != relative {
		return "", fmt.Errorf("repository-relative path escapes root: %q", relative)
	}
	return path, nil
}

type replayOptions struct {
	emptyCaches bool
}

func generate(root string, options replayOptions) ([]byte, error) {
	if err := verifyPinnedSource(root); err != nil {
		return nil, err
	}
	raw := adversarialPollItem()
	probe, err := runPinnedProbe(root, raw, options)
	if err != nil {
		return nil, err
	}
	projected, err := base64.StdEncoding.DecodeString(probe.ProjectedPayload)
	if err != nil {
		return nil, fmt.Errorf("decode pinned projected payload: %w", err)
	}
	canonical, err := base64.StdEncoding.DecodeString(probe.CanonicalPayload)
	if err != nil {
		return nil, fmt.Errorf("decode pinned canonical payload: %w", err)
	}
	if !bytes.Equal(projected, canonical) {
		return nil, errors.New("pinned runner canonical payload differs from daemon projection")
	}
	if strings.Contains(string(canonical), forgedSidecarMark) {
		return nil, errors.New("pinned projection retained a forged execution sidecar")
	}
	if probe.ForgedSidecarError != "daemon poll: operationalPayload does not match raw poll item" {
		return nil, fmt.Errorf("pinned forged operationalPayload rejection = %q", probe.ForgedSidecarError)
	}
	if probe.GoVersion != pinnedGoVersion {
		return nil, fmt.Errorf("pinned archive Go version = %q, want %q", probe.GoVersion, pinnedGoVersion)
	}

	sourcePath, err := fixedRepositoryFile(root, generatorSource)
	if err != nil {
		return nil, err
	}
	source, err := os.ReadFile(sourcePath) // #nosec G304 -- fixedRepositoryFile accepts only the generator's fixed repository-relative path.
	if err != nil {
		return nil, fmt.Errorf("read generator source: %w", err)
	}
	result := artifact{
		Schema:             artifactSchema,
		GeneratorVersion:   generatorVersion,
		GeneratorSource:    generatorSource,
		GeneratorSourceSHA: sha256Hex(source),
		ProbeSourceSHA:     probeTemplateSHA256,
		Tag:                pinnedTag,
		Commit:             pinnedCommit,
		GoVersion:          probe.GoVersion,
		SourceBlobs:        append([]sourceBlob(nil), pinnedBlobs...),
		RawPollItem:        bytesValue(raw),
		ProjectedPayload:   bytesValue(projected),
		CanonicalPayload:   bytesValue(canonical),
		OperationalDigest:  probe.OperationalDigest,
		ForgedSidecarError: probe.ForgedSidecarError,
	}
	withoutDigest, err := json.Marshal(result)
	if err != nil {
		return nil, fmt.Errorf("marshal unsigned artifact: %w", err)
	}
	result.ArtifactDigest = sha256Hex(withoutDigest)
	encoded, err := json.MarshalIndent(result, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("marshal artifact: %w", err)
	}
	return append(encoded, '\n'), nil
}

func verifyPinnedSource(root string) error {
	return verifyPinnedSourceWith(root, pinnedTag, pinnedCommit, pinnedBlobs)
}

func verifyPinnedSourceWith(root, tag, wantCommit string, blobs []sourceBlob) error {
	commit, err := git(root, "rev-parse", tag+"^{commit}")
	if err != nil {
		return fmt.Errorf("resolve %s: %w", tag, err)
	}
	if err := verifyTagCommit(tag, commit, wantCommit); err != nil {
		return err
	}
	for _, expected := range blobs {
		line, err := git(root, "ls-tree", wantCommit, "--", expected.Path)
		if err != nil {
			return fmt.Errorf("read %s blob: %w", expected.Path, err)
		}
		parts := strings.Fields(line)
		if len(parts) < 3 || parts[1] != "blob" {
			return fmt.Errorf("%s is not a source blob in pinned commit %s", expected.Path, wantCommit)
		}
		if parts[2] != expected.SHA1 {
			return fmt.Errorf("%s blob = %s, want %s", expected.Path, parts[2], expected.SHA1)
		}
	}
	return nil
}

func verifyTagCommit(tag, got, want string) error {
	if got != want {
		return fmt.Errorf("%s commit = %s, want %s", tag, got, want)
	}
	return nil
}

func runPinnedProbe(root string, raw []byte, options replayOptions) (result probeOutput, resultErr error) {
	if err := validateProbeSource(probeProgram); err != nil {
		return probeOutput{}, err
	}
	temporary, err := os.MkdirTemp("", "donmai-operational-payload-provenance-")
	if err != nil {
		return probeOutput{}, fmt.Errorf("create temporary archive tree: %w", err)
	}
	var moduleCache string
	var cleanupGoEnv []string
	defer func() {
		resultErr = joinReplayAndCleanupError(resultErr, cleanupPinnedProbeTemporary(temporary, moduleCache, cleanupGoEnv, runCommand, os.RemoveAll))
	}()

	archiveCommand, err := commandFor(root, nil, commandGit, "archive", "--format=tar", pinnedCommit)
	if err != nil {
		return probeOutput{}, err
	}
	// A pipe is used so archive bytes never pass through a shell or a file that
	// could be mistaken for release source.
	tarCommand, err := commandFor(temporary, nil, commandTar, "-x", "-C", temporary)
	if err != nil {
		return probeOutput{}, err
	}
	archiveInput, err := archiveCommand.StdoutPipe()
	if err != nil {
		return probeOutput{}, fmt.Errorf("open archive stream: %w", err)
	}
	tarCommand.Stdin = archiveInput
	if err := tarCommand.Start(); err != nil {
		return probeOutput{}, fmt.Errorf("start archive extraction: %w", err)
	}
	if err := archiveCommand.Start(); err != nil {
		return probeOutput{}, fmt.Errorf("start clean git archive: %w", err)
	}
	archiveErr := archiveCommand.Wait()
	tarErr := tarCommand.Wait()
	if archiveErr != nil {
		return probeOutput{}, fmt.Errorf("create clean git archive: %w", archiveErr)
	}
	if tarErr != nil {
		return probeOutput{}, fmt.Errorf("extract clean git archive: %w", tarErr)
	}

	inputPath := filepath.Join(temporary, "pinned-poll-item.json")
	if err := os.WriteFile(inputPath, raw, 0o600); err != nil {
		return probeOutput{}, fmt.Errorf("write probe input: %w", err)
	}
	forgedPath := filepath.Join(temporary, "pinned-forged-poll-item.json")
	if err := os.WriteFile(forgedPath, forgedOperationalPayload(raw), 0o600); err != nil {
		return probeOutput{}, fmt.Errorf("write forged probe input: %w", err)
	}
	probeSourcePath := filepath.Join(temporary, probePath)
	if err := os.MkdirAll(filepath.Dir(probeSourcePath), 0o750); err != nil {
		return probeOutput{}, fmt.Errorf("create probe directory: %w", err)
	}
	if err := os.WriteFile(probeSourcePath, []byte(probeProgram), 0o600); err != nil {
		return probeOutput{}, fmt.Errorf("write probe source: %w", err)
	}

	goEnv := []string{"GOWORK=off", "GOTOOLCHAIN=local"}
	if options.emptyCaches {
		moduleCache = filepath.Join(temporary, "gomodcache")
		buildCache := filepath.Join(temporary, "gocache")
		if err := os.MkdirAll(moduleCache, 0o700); err != nil {
			return probeOutput{}, fmt.Errorf("create empty module cache: %w", err)
		}
		goEnv = append(goEnv, "GOMODCACHE="+moduleCache, "GOCACHE="+buildCache)
		cleanupGoEnv = append([]string(nil), goEnv...)
	}
	// v0.72.2 has no vendor directory. The exact archived go.mod/go.sum (both
	// blob-pinned above) therefore resolve dependencies through normal Go module
	// verification. A warm cache makes this local; an empty cache may fetch.
	if _, err := runCommand(temporary, goEnv, commandGo, "mod", "download"); err != nil {
		return probeOutput{}, fmt.Errorf("resolve pinned archive modules: %w", err)
	}
	if _, err := runCommand(temporary, goEnv, commandGo, "mod", "verify"); err != nil {
		return probeOutput{}, fmt.Errorf("verify pinned archive modules against go.sum: %w", err)
	}
	goVersion, err := runCommand(temporary, goEnv, commandGo, "env", "GOVERSION")
	if err != nil {
		return probeOutput{}, fmt.Errorf("read pinned archive Go version: %w", err)
	}
	output, err := runCommand(temporary, goEnv, commandGo, "run", "./cmd/pinned-operational-payload-probe", inputPath, forgedPath)
	if err != nil {
		return probeOutput{}, fmt.Errorf("run pinned Go probe: %w", err)
	}
	var decoded probeOutput
	if err := json.Unmarshal(output, &decoded); err != nil {
		return probeOutput{}, fmt.Errorf("decode pinned Go probe output: %w", err)
	}
	decoded.GoVersion = strings.TrimSpace(string(goVersion))
	return decoded, nil
}

type commandRunner func(dir string, extraEnv []string, kind commandKind, args ...string) ([]byte, error)

type removeTree func(path string) error

// cleanupPinnedProbeTemporary cleans only a module cache we created as the
// exact gomodcache child of temporary. Go deliberately makes downloaded module
// content read-only; asking the same Go toolchain to clean that cache avoids a
// host-specific chmod walk while leaving symlinks and paths outside temporary
// outside this cleanup authority.
func cleanupPinnedProbeTemporary(temporary, moduleCache string, goEnv []string, run commandRunner, remove removeTree) error {
	if moduleCache != "" {
		if err := validateOwnedModuleCache(temporary, moduleCache); err != nil {
			return err
		}
		if run == nil {
			return errors.New("cleanup pinned probe temporary: Go command runner is required for module cache cleanup")
		}
		if _, err := run(temporary, goEnv, commandGo, "clean", "-modcache"); err != nil {
			return fmt.Errorf("clean owned temporary Go module cache: %w", err)
		}
	}
	if err := remove(temporary); err != nil {
		return fmt.Errorf("remove temporary archive tree: %w", err)
	}
	return nil
}

func validateOwnedModuleCache(temporary, moduleCache string) error {
	temporaryRoot, err := filepath.Abs(temporary)
	if err != nil {
		return fmt.Errorf("resolve temporary archive tree: %w", err)
	}
	moduleRoot, err := filepath.Abs(moduleCache)
	if err != nil {
		return fmt.Errorf("resolve temporary module cache: %w", err)
	}
	relative, err := filepath.Rel(temporaryRoot, moduleRoot)
	if err != nil || relative != "gomodcache" {
		return fmt.Errorf("temporary module cache must be the owned gomodcache child, got %q", moduleCache)
	}
	info, err := os.Lstat(moduleRoot)
	if err != nil {
		return fmt.Errorf("lstat temporary module cache: %w", err)
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("temporary module cache is not an owned directory: %q", moduleCache)
	}
	return nil
}

func joinReplayAndCleanupError(primary, cleanup error) error {
	if cleanup == nil {
		return primary
	}
	if primary == nil {
		return cleanup
	}
	return errors.Join(primary, cleanup)
}

func git(root string, args ...string) (string, error) {
	output, err := runCommand(root, nil, commandGit, args...)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(output)), nil
}

type commandKind uint8

const (
	commandGit commandKind = iota + 1
	commandTar
	commandGo
)

func runCommand(dir string, extraEnv []string, kind commandKind, args ...string) ([]byte, error) {
	command, err := commandFor(dir, extraEnv, kind, args...)
	if err != nil {
		return nil, err
	}
	output, err := command.CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("%s %s: %w\n%s", command.Path, strings.Join(args, " "), err, output)
	}
	return output, nil
}

func commandFor(dir string, extraEnv []string, kind commandKind, args ...string) (*exec.Cmd, error) {
	if err := validateCommand(kind, dir, args); err != nil {
		return nil, err
	}
	var command *exec.Cmd
	switch kind {
	case commandGit:
		command = exec.Command("git", args...) // #nosec G204 -- command kind and every argument shape are closed by validateCommand.
	case commandTar:
		command = exec.Command("tar", args...) // #nosec G204 -- command kind and every argument shape are closed by validateCommand.
	case commandGo:
		command = exec.Command("go", args...) // #nosec G204 -- command kind and every argument shape are closed by validateCommand.
	default:
		return nil, fmt.Errorf("unsupported command kind %d", kind)
	}
	command.Dir = dir
	command.Env = append(os.Environ(), extraEnv...)
	return command, nil
}

func validateCommand(kind commandKind, dir string, args []string) error {
	if err := validateCommandDirectory(dir); err != nil {
		return err
	}
	switch kind {
	case commandGit:
		if matches(args, "rev-parse", "--show-toplevel") ||
			(len(args) == 2 && args[0] == "rev-parse" && safeGitReference(args[1])) ||
			(len(args) == 4 && args[0] == "ls-tree" && safeCommit(args[1]) && args[2] == "--" && safePinnedBlobPath(args[3])) ||
			matches(args, "archive", "--format=tar", pinnedCommit) {
			return nil
		}
	case commandTar:
		if len(args) == 3 && args[0] == "-x" && args[1] == "-C" && args[2] == dir {
			return nil
		}
	case commandGo:
		if matches(args, "mod", "download") || matches(args, "mod", "verify") || matches(args, "env", "GOVERSION") || matches(args, "clean", "-modcache") {
			return nil
		}
		if len(args) == 4 && args[0] == "run" && args[1] == "./cmd/pinned-operational-payload-probe" && args[2] == filepath.Join(dir, "pinned-poll-item.json") && args[3] == filepath.Join(dir, "pinned-forged-poll-item.json") {
			return nil
		}
	}
	return fmt.Errorf("refused unsafe %s arguments %q", commandName(kind), args)
}

func validateCommandDirectory(dir string) error {
	if dir == "" {
		return nil
	}
	info, err := os.Stat(dir)
	if err != nil {
		return fmt.Errorf("stat command directory: %w", err)
	}
	if !info.IsDir() {
		return fmt.Errorf("command directory is not a directory: %q", dir)
	}
	return nil
}

func commandName(kind commandKind) string {
	switch kind {
	case commandGit:
		return "git"
	case commandTar:
		return "tar"
	case commandGo:
		return "go"
	default:
		return "unknown"
	}
}

func matches(got []string, want ...string) bool {
	return len(got) == len(want) && strings.Join(got, "\x00") == strings.Join(want, "\x00")
}

func safeGitReference(value string) bool {
	return value == pinnedTag+"^{commit}" || (strings.HasPrefix(value, "v") && strings.HasSuffix(value, "^{commit}") && !strings.ContainsAny(value, "\\/:; \t\n"))
}

func safeCommit(value string) bool {
	if len(value) != 40 {
		return false
	}
	for _, character := range value {
		if !strings.ContainsRune("0123456789abcdef", character) {
			return false
		}
	}
	return true
}

func safePinnedBlobPath(value string) bool {
	for _, blob := range pinnedBlobs {
		if value == blob.Path {
			return true
		}
	}
	return false
}

func bytesValue(raw []byte) byteValue {
	return byteValue{Encoding: "base64", Bytes: len(raw), SHA256: sha256Hex(raw), Base64: base64.StdEncoding.EncodeToString(raw)}
}

func sha256Hex(raw []byte) string {
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

func validateProbeSource(source string) error {
	if sha256Hex([]byte(source)) != probeTemplateSHA256 {
		return errors.New("probe source hash does not match the pinned template")
	}
	return validateProbeStructure(source)
}

func validateProbeStructure(source string) error {
	file, err := parser.ParseFile(token.NewFileSet(), probePath, source, 0)
	if err != nil {
		return fmt.Errorf("parse probe source: %w", err)
	}
	imports := map[string]string{}
	for _, spec := range file.Imports {
		path := strings.Trim(spec.Path.Value, "\"")
		name := filepath.Base(path)
		if spec.Name != nil {
			name = spec.Name.Name
		}
		imports[name] = path
	}
	if imports["daemon"] != "github.com/RenseiAI/donmai/daemon" || imports["runner"] != "github.com/RenseiAI/donmai/runner" {
		return errors.New("probe imports are not the pinned daemon and runner packages")
	}
	var declaredItem, unmarshaledItem, canonicalFromItem, digestFromItem, emittedOutput bool
	ast.Inspect(file, func(node ast.Node) bool {
		switch typed := node.(type) {
		case *ast.ValueSpec:
			if len(typed.Names) == 1 && typed.Names[0].Name == "item" && isSelector(typed.Type, "daemon", "PollWorkItem") {
				declaredItem = true
			}
		case *ast.AssignStmt:
			if len(typed.Lhs) > 0 && len(typed.Rhs) > 0 {
				call, ok := typed.Rhs[0].(*ast.CallExpr)
				if ok && isIdentifier(typed.Lhs[0], "canonical") && isSelector(call.Fun, "runner", "CanonicalOperationalPayload") && len(call.Args) == 1 && isQueuedWorkFromItem(call.Args[0]) {
					canonicalFromItem = true
				}
				if ok && isIdentifier(typed.Lhs[0], "digest") && isSelector(call.Fun, "runner", "DigestOperationalPayload") && len(call.Args) == 1 && isQueuedWorkFromItem(call.Args[0]) {
					digestFromItem = true
				}
			}
		case *ast.CallExpr:
			switch {
			case isSelector(typed.Fun, "json", "Unmarshal") && len(typed.Args) == 2 && isIdentifier(typed.Args[0], "raw") && isAddressOfIdentifier(typed.Args[1], "item"):
				unmarshaledItem = true
			case isEncoderOutputCall(typed):
				emittedOutput = true
			}
		}
		return true
	})
	if !declaredItem || !unmarshaledItem || !canonicalFromItem || !digestFromItem || !emittedOutput {
		return errors.New("probe does not preserve the required direct PollWorkItem to CanonicalOperationalPayload/Digest output dataflow")
	}
	if !probeOutputFieldsAreDirect(file) {
		return errors.New("probe output is not populated directly from the pinned PollWorkItem, canonical, and digest values")
	}
	return nil
}

func isSelector(expression ast.Expr, packageName, selector string) bool {
	value, ok := expression.(*ast.SelectorExpr)
	return ok && isIdentifier(value.X, packageName) && value.Sel.Name == selector
}

func isIdentifier(expression ast.Expr, want string) bool {
	value, ok := expression.(*ast.Ident)
	return ok && value.Name == want
}

func isAddressOfIdentifier(expression ast.Expr, want string) bool {
	value, ok := expression.(*ast.UnaryExpr)
	return ok && value.Op == token.AND && isIdentifier(value.X, want)
}

func isQueuedWorkFromItem(expression ast.Expr) bool {
	value, ok := expression.(*ast.CompositeLit)
	if !ok || !isSelector(value.Type, "runner", "QueuedWork") || len(value.Elts) != 1 {
		return false
	}
	field, ok := value.Elts[0].(*ast.KeyValueExpr)
	return ok && isIdentifier(field.Key, "OperationalPayload") && isItemOperationalPayload(field.Value)
}

func isItemOperationalPayload(expression ast.Expr) bool {
	return isSelector(expression, "item", "OperationalPayload")
}

func isEncoderOutputCall(call *ast.CallExpr) bool {
	if len(call.Args) != 1 || !isIdentifier(call.Args[0], "output") {
		return false
	}
	encode, ok := call.Fun.(*ast.SelectorExpr)
	if !ok || encode.Sel.Name != "Encode" {
		return false
	}
	newEncoder, ok := encode.X.(*ast.CallExpr)
	return ok && isSelector(newEncoder.Fun, "json", "NewEncoder") && len(newEncoder.Args) == 1 && isSelector(newEncoder.Args[0], "os", "Stdout")
}

func probeOutputFieldsAreDirect(file *ast.File) bool {
	valid := false
	ast.Inspect(file, func(node ast.Node) bool {
		assignment, ok := node.(*ast.AssignStmt)
		if !ok || len(assignment.Lhs) != 1 || !isIdentifier(assignment.Lhs[0], "output") || len(assignment.Rhs) != 1 {
			return true
		}
		literal, ok := assignment.Rhs[0].(*ast.CompositeLit)
		if !ok {
			return true
		}
		fields := map[string]ast.Expr{}
		for _, element := range literal.Elts {
			field, ok := element.(*ast.KeyValueExpr)
			if !ok {
				return true
			}
			name, ok := field.Key.(*ast.Ident)
			if !ok {
				return true
			}
			fields[name.Name] = field.Value
		}
		valid = isBase64EncodingOf(fields["ProjectedPayload"], "item") &&
			isBase64EncodingOf(fields["CanonicalPayload"], "canonical") &&
			isIdentifier(fields["OperationalDigest"], "digest") && isErrorString(fields["ForgedSidecarError"])
		return true
	})
	return valid
}

func isBase64EncodingOf(expression ast.Expr, name string) bool {
	call, ok := expression.(*ast.CallExpr)
	return ok && isBase64EncodeCall(call, name)
}

func isBase64EncodeCall(call *ast.CallExpr, name string) bool {
	if !isStdEncodingEncodeToString(call.Fun) {
		return false
	}
	return len(call.Args) == 1 && ((name == "item" && isItemOperationalPayload(call.Args[0])) || isIdentifier(call.Args[0], name))
}

func isStdEncodingEncodeToString(expression ast.Expr) bool {
	call, ok := expression.(*ast.SelectorExpr)
	if !ok || call.Sel.Name != "EncodeToString" {
		return false
	}
	return isSelector(call.X, "base64", "StdEncoding")
}

func isErrorString(expression ast.Expr) bool {
	call, ok := expression.(*ast.CallExpr)
	return ok && isSelector(call.Fun, "err", "Error") && len(call.Args) == 0
}

func validateArtifact(raw []byte) error {
	var decoded artifact
	if err := json.Unmarshal(raw, &decoded); err != nil {
		return fmt.Errorf("decode committed artifact: %w", err)
	}
	if decoded.Schema != artifactSchema || decoded.Tag != pinnedTag || decoded.Commit != pinnedCommit {
		return errors.New("committed artifact has the wrong schema, tag, or commit")
	}
	if decoded.GoVersion != pinnedGoVersion || decoded.ProbeSourceSHA != probeTemplateSHA256 {
		return errors.New("committed artifact has the wrong Go version or probe template hash")
	}
	if !sameSourceBlobs(decoded.SourceBlobs, pinnedBlobs) {
		return errors.New("committed artifact source blobs do not match the pinned release")
	}
	if decoded.ArtifactDigest == "" {
		return errors.New("committed artifact has no artifactDigest")
	}
	digest := decoded.ArtifactDigest
	decoded.ArtifactDigest = ""
	unsigned, err := json.Marshal(decoded)
	if err != nil {
		return fmt.Errorf("marshal unsigned committed artifact: %w", err)
	}
	if sha256Hex(unsigned) != digest {
		return errors.New("committed artifactDigest does not match its unsigned content")
	}
	for _, value := range []byteValue{decoded.RawPollItem, decoded.ProjectedPayload, decoded.CanonicalPayload} {
		contents, err := base64.StdEncoding.DecodeString(value.Base64)
		if err != nil || len(contents) != value.Bytes || sha256Hex(contents) != value.SHA256 {
			return errors.New("committed artifact contains corrupt bytes or byte digest")
		}
	}
	if decoded.OperationalDigest != decoded.CanonicalPayload.SHA256 {
		return errors.New("committed artifact operational digest does not match canonical payload")
	}
	if decoded.ForgedSidecarError != "daemon poll: operationalPayload does not match raw poll item" {
		return errors.New("committed artifact does not record the pinned forged-sidecar rejection")
	}
	return nil
}

func sameSourceBlobs(got, want []sourceBlob) bool {
	if len(got) != len(want) {
		return false
	}
	for index := range want {
		if got[index] != want[index] {
			return false
		}
	}
	return true
}

// The input is intentionally raw JSON, not a Go struct. The daemon's release
// decoder must retain empty containers and RFC-8785 canonicalize -0, 1e21,
// Unicode, and non-ASCII keys before the runner digests it. Every forged
// execution sidecar is outside the operational projection.
func adversarialPollItem() []byte {
	return []byte(`{"sessionId":"session-π","projectName":"prøject","repository":"acme/naïve","allowedTools":[],"mcpServers":[{"name":"mcp-Ω","type":"stdio","args":[],"env":{}}],"stageLifecycle":{"é":-0,"Ω":1e21,"a":[]},"nonAsciiOrder":{"Ω":"omega","é":"accent","a":"ascii"},"emptyMap":{},"emptyList":[],"priority":-0,"admissionReceipt":{"forged":"FORGED_SIDECAR_MUST_NOT_REACH_OPERATIONAL_PAYLOAD"},"claimReceipt":{"forged":"FORGED_SIDECAR_MUST_NOT_REACH_OPERATIONAL_PAYLOAD"},"effectiveCell":{"forged":"FORGED_SIDECAR_MUST_NOT_REACH_OPERATIONAL_PAYLOAD"},"executionRuntimeBinding":{"forged":"FORGED_SIDECAR_MUST_NOT_REACH_OPERATIONAL_PAYLOAD"},"hostAdaptationReceipt":{"forged":"FORGED_SIDECAR_MUST_NOT_REACH_OPERATIONAL_PAYLOAD"}}`)
}

func forgedOperationalPayload(raw []byte) []byte {
	return append(append(append([]byte(nil), raw[:len(raw)-1]...), []byte(`,"operationalPayload":{"forged":"FORGED_RAW_OPERATIONAL_PAYLOAD"}`)...), '}')
}

const probeProgram = `package main

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"os"

	"github.com/RenseiAI/donmai/daemon"
	"github.com/RenseiAI/donmai/runner"
)

func main() {
	raw, err := os.ReadFile(os.Args[1])
	if err != nil { panic(err) }
	var item daemon.PollWorkItem
	if err := json.Unmarshal(raw, &item); err != nil { panic(err) }
	if bytes.Contains(item.OperationalPayload, []byte("FORGED_SIDECAR_MUST_NOT_REACH_OPERATIONAL_PAYLOAD")) { panic("forged sidecar reached operational payload") }
	canonical, err := runner.CanonicalOperationalPayload(runner.QueuedWork{OperationalPayload: item.OperationalPayload})
	if err != nil { panic(err) }
	digest, err := runner.DigestOperationalPayload(runner.QueuedWork{OperationalPayload: item.OperationalPayload})
	if err != nil { panic(err) }
	forged, err := os.ReadFile(os.Args[2])
	if err != nil { panic(err) }
	var rejected daemon.PollWorkItem
	if err := json.Unmarshal(forged, &rejected); err == nil { panic("forged operationalPayload was accepted") } else if err.Error() != "daemon poll: operationalPayload does not match raw poll item" { panic(err) } else {
		output := struct {
			ProjectedPayload string ` + "`json:\"projectedPayloadBase64\"`" + `
			CanonicalPayload string ` + "`json:\"canonicalPayloadBase64\"`" + `
			OperationalDigest string ` + "`json:\"operationalDigest\"`" + `
			ForgedSidecarError string ` + "`json:\"forgedSidecarError\"`" + `
		}{
			ProjectedPayload: base64.StdEncoding.EncodeToString(item.OperationalPayload),
			CanonicalPayload: base64.StdEncoding.EncodeToString(canonical),
			OperationalDigest: digest,
			ForgedSidecarError: err.Error(),
		}
		if err := json.NewEncoder(os.Stdout).Encode(output); err != nil { panic(err) }
	}
}
`
