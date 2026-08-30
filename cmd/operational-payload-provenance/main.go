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
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

const (
	artifactSchema    = "donmai.operational-payload-provenance/v1"
	generatorVersion  = "1"
	pinnedTag         = "v0.72.2"
	pinnedCommit      = "a177e8929f1b0b0cd27fc6c8480f6cc210fde1ff"
	artifactPath      = "runner/testdata/v0.72.2-operational-payload-provenance.json"
	generatorSource   = "cmd/operational-payload-provenance/main.go"
	probePath         = "cmd/pinned-operational-payload-probe/main.go"
	forgedSidecarMark = "FORGED_SIDECAR_MUST_NOT_REACH_OPERATIONAL_PAYLOAD"
)

var pinnedBlobs = []sourceBlob{
	{Path: "daemon/poll.go", SHA1: "5da783ad3da554eb18d2a3ce5766ab981e552209"},
	{Path: "runner/operational_payload.go", SHA1: "b962235a08a97deeef0d0020a4fc1013d3876a4e"},
	{Path: "runner/types.go", SHA1: "91ff6e3a4e7627b3b49752e8ff04d8999e244fbb"},
	{Path: "prompt/queued_work.go", SHA1: "70303ccba809ac004c74f4028c2ed2b585c9ba8b"},
	{Path: "executioncell/runtime_binding.go", SHA1: "2bc69f4e886deb499cb844e904557bd7f6b02d98"},
	{Path: "executioncell/codec.go", SHA1: "4a3e0829e86cd7931ae2c2c9fd0a387cce5530d7"},
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
	Tag                string       `json:"tag"`
	Commit             string       `json:"commit"`
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
}

func main() {
	write := flag.Bool("write", false, "write the regenerated artifact instead of byte-comparing it")
	artifactFlag := flag.String("artifact", artifactPath, "artifact path relative to the repository root, or absolute")
	flag.Parse()

	root, err := repositoryRoot()
	if err != nil {
		fatal(err)
	}
	path := *artifactFlag
	if !filepath.IsAbs(path) {
		path = filepath.Join(root, path)
	}
	generated, err := generate(root)
	if err != nil {
		fatal(err)
	}
	if *write {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			fatal(fmt.Errorf("create artifact directory: %w", err))
		}
		if err := os.WriteFile(path, generated, 0o644); err != nil {
			fatal(fmt.Errorf("write artifact: %w", err))
		}
		return
	}
	committed, err := os.ReadFile(path)
	if err != nil {
		fatal(fmt.Errorf("read committed artifact: %w", err))
	}
	if !bytes.Equal(committed, generated) {
		fatal(fmt.Errorf("provenance artifact differs from a clean %s archive; run go run ./cmd/operational-payload-provenance -write", pinnedTag))
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
	output, err := run("", nil, "git", "rev-parse", "--show-toplevel")
	if err != nil {
		return "", fmt.Errorf("locate repository root: %w", err)
	}
	return strings.TrimSpace(string(output)), nil
}

func generate(root string) ([]byte, error) {
	if err := verifyPinnedSource(root); err != nil {
		return nil, err
	}
	raw := adversarialPollItem()
	probe, err := runPinnedProbe(root, raw)
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

	source, err := os.ReadFile(filepath.Join(root, generatorSource))
	if err != nil {
		return nil, fmt.Errorf("read generator source: %w", err)
	}
	result := artifact{
		Schema:             artifactSchema,
		GeneratorVersion:   generatorVersion,
		GeneratorSource:    generatorSource,
		GeneratorSourceSHA: sha256Hex(source),
		Tag:                pinnedTag,
		Commit:             pinnedCommit,
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
	if commit != wantCommit {
		return fmt.Errorf("%s commit = %s, want %s", tag, commit, wantCommit)
	}
	for _, expected := range blobs {
		line, err := git(root, "ls-tree", tag, "--", expected.Path)
		if err != nil {
			return fmt.Errorf("read %s blob: %w", expected.Path, err)
		}
		parts := strings.Fields(line)
		if len(parts) < 3 || parts[1] != "blob" {
			return fmt.Errorf("%s is not a source blob in %s", expected.Path, tag)
		}
		if parts[2] != expected.SHA1 {
			return fmt.Errorf("%s blob = %s, want %s", expected.Path, parts[2], expected.SHA1)
		}
	}
	return nil
}

func runPinnedProbe(root string, raw []byte) (probeOutput, error) {
	if err := assertProbeUsesPinnedFunctions(probeProgram); err != nil {
		return probeOutput{}, err
	}
	temporary, err := os.MkdirTemp("", "donmai-operational-payload-provenance-")
	if err != nil {
		return probeOutput{}, fmt.Errorf("create temporary archive tree: %w", err)
	}
	defer os.RemoveAll(temporary)

	archiveCommand := exec.Command("git", "-C", root, "archive", "--format=tar", pinnedTag)
	// A pipe is used so archive bytes never pass through a shell or a file that
	// could be mistaken for release source.
	tarCommand := exec.Command("tar", "-x", "-C", temporary)
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
	if err := os.MkdirAll(filepath.Dir(probeSourcePath), 0o755); err != nil {
		return probeOutput{}, fmt.Errorf("create probe directory: %w", err)
	}
	if err := os.WriteFile(probeSourcePath, []byte(probeProgram), 0o600); err != nil {
		return probeOutput{}, fmt.Errorf("write probe source: %w", err)
	}

	output, err := run(temporary, []string{"GOWORK=off", "GOPROXY=off", "GOSUMDB=off", "GOTOOLCHAIN=local"}, "go", "run", "./cmd/pinned-operational-payload-probe", inputPath, forgedPath)
	if err != nil {
		return probeOutput{}, fmt.Errorf("run pinned Go probe without network: %w", err)
	}
	var decoded probeOutput
	if err := json.Unmarshal(output, &decoded); err != nil {
		return probeOutput{}, fmt.Errorf("decode pinned Go probe output: %w", err)
	}
	return decoded, nil
}

func git(root string, args ...string) (string, error) {
	output, err := run(root, nil, "git", args...)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(output)), nil
}

func run(dir string, extraEnv []string, name string, args ...string) ([]byte, error) {
	command := exec.Command(name, args...)
	command.Dir = dir
	command.Env = append(os.Environ(), extraEnv...)
	output, err := command.CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("%s %s: %w\n%s", name, strings.Join(args, " "), err, output)
	}
	return output, nil
}

func bytesValue(raw []byte) byteValue {
	return byteValue{Encoding: "base64", Bytes: len(raw), SHA256: sha256Hex(raw), Base64: base64.StdEncoding.EncodeToString(raw)}
}

func sha256Hex(raw []byte) string {
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

func assertProbeUsesPinnedFunctions(source string) error {
	for _, call := range []string{
		"daemon.PollWorkItem",
		"runner.CanonicalOperationalPayload",
		"runner.DigestOperationalPayload",
	} {
		if !strings.Contains(source, call) {
			return fmt.Errorf("probe bypasses required pinned function path %q", call)
		}
	}
	return nil
}

func validateArtifact(raw []byte) error {
	var decoded artifact
	if err := json.Unmarshal(raw, &decoded); err != nil {
		return fmt.Errorf("decode committed artifact: %w", err)
	}
	if decoded.Schema != artifactSchema || decoded.Tag != pinnedTag || decoded.Commit != pinnedCommit {
		return errors.New("committed artifact has the wrong schema, tag, or commit")
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
		json.NewEncoder(os.Stdout).Encode(struct {
			ProjectedPayload string ` + "`json:\"projectedPayloadBase64\"`" + `
			CanonicalPayload string ` + "`json:\"canonicalPayloadBase64\"`" + `
			OperationalDigest string ` + "`json:\"operationalDigest\"`" + `
			ForgedSidecarError string ` + "`json:\"forgedSidecarError\"`" + `
		}{base64.StdEncoding.EncodeToString(item.OperationalPayload), base64.StdEncoding.EncodeToString(canonical), digest, err.Error()})
	}
}
`
