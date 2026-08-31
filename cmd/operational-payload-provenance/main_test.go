package main

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCleanupPinnedProbeTemporary_RemovesReadOnlyModuleCacheWithoutFollowingSymlink(t *testing.T) {
	temporary, err := os.MkdirTemp("", "operational-payload-cleanup-")
	if err != nil {
		t.Fatal(err)
	}
	moduleCache := filepath.Join(temporary, "gomodcache")
	nested := filepath.Join(moduleCache, "gopkg.in", "yaml.v3@v3.0.1")
	if err := os.MkdirAll(nested, 0o700); err != nil {
		t.Fatal(err)
	}
	moduleFile := filepath.Join(nested, "module.go")
	if err := os.WriteFile(moduleFile, []byte("read-only module content"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(moduleFile, 0o444); err != nil { // #nosec G302 -- cleanup control must reproduce Go's read-only module files.
		t.Fatal(err)
	}
	escapeTarget := t.TempDir()
	escapeMarker := filepath.Join(escapeTarget, "must-survive")
	if err := os.WriteFile(escapeMarker, []byte("outside owned cache"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(escapeTarget, filepath.Join(moduleCache, "escape")); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{nested, filepath.Dir(nested), moduleCache} {
		if err := os.Chmod(path, 0o555); err != nil { // #nosec G302 -- cleanup control must reproduce Go's read-only module directories.
			t.Fatal(err)
		}
	}

	if err := cleanupPinnedProbeTemporary(temporary, moduleCache, []string{"GOWORK=off", "GOTOOLCHAIN=local", "GOMODCACHE=" + moduleCache}, runCommand, os.RemoveAll); err != nil {
		t.Fatalf("cleanupPinnedProbeTemporary: %v", err)
	}
	if _, err := os.Stat(temporary); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("temporary tree remains after cleanup: %v", err)
	}
	if contents, err := os.ReadFile(escapeMarker); err != nil || string(contents) != "outside owned cache" {
		t.Fatalf("symlink escape target changed: contents=%q err=%v", contents, err)
	}
}

func TestCleanupPinnedProbeTemporary_ReportsCleanupWithoutMaskingPrimary(t *testing.T) {
	temporary := t.TempDir()
	cleanupFailure := errors.New("simulated remove failure")
	cleanupErr := cleanupPinnedProbeTemporary(temporary, "", nil, nil, func(string) error { return cleanupFailure })
	if !errors.Is(cleanupErr, cleanupFailure) {
		t.Fatalf("cleanup error = %v, want observable %v", cleanupErr, cleanupFailure)
	}
	primary := errors.New("simulated replay failure")
	combined := joinReplayAndCleanupError(primary, cleanupErr)
	if !errors.Is(combined, primary) || !errors.Is(combined, cleanupFailure) {
		t.Fatalf("combined error = %v, want both primary and cleanup errors", combined)
	}
}

func TestCleanupPinnedProbeTemporary_RefusesModuleCacheOutsideOwnedChild(t *testing.T) {
	temporary := t.TempDir()
	outside := t.TempDir()
	if err := cleanupPinnedProbeTemporary(temporary, outside, nil, runCommand, os.RemoveAll); err == nil {
		t.Fatal("outside module cache unexpectedly accepted")
	}
	if _, err := os.Stat(outside); err != nil {
		t.Fatalf("outside module cache was touched: %v", err)
	}
}

func TestPathAndCommandRefusals(t *testing.T) {
	root, err := repositoryRoot()
	if err != nil {
		t.Fatal(err)
	}
	expected, err := artifactFile(root, artifactPath)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := artifactFile(root, expected); err != nil || got != expected {
		t.Fatalf("absolute expected artifact path = %q, %v; want %q, nil", got, err, expected)
	}
	for _, path := range []string{"../outside.json", filepath.Join(root, "..", "outside.json"), "/tmp/outside.json"} {
		if _, err := artifactFile(root, path); err == nil {
			t.Fatalf("malicious artifact path %q unexpectedly passed", path)
		}
	}
	if _, err := fixedRepositoryFile(root, "../go.mod"); err == nil {
		t.Fatal("escaping generator source path unexpectedly passed")
	}
	for _, tc := range []struct {
		name string
		kind commandKind
		args []string
	}{
		{name: "unknown binary kind", kind: commandKind(99), args: []string{"sh", "-c", "echo unsafe"}},
		{name: "git configuration", kind: commandGit, args: []string{"config", "--global", "core.hooksPath", "/tmp"}},
		{name: "git option injection", kind: commandGit, args: []string{"rev-parse", "--upload-pack=sh^{commit}"}},
		{name: "tar escape", kind: commandTar, args: []string{"-x", "-C", "/tmp"}},
		{name: "go arbitrary run", kind: commandGo, args: []string{"run", "./cmd/evil", "one", "two"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := validateCommand(tc.kind, root, tc.args); err == nil {
				t.Fatalf("unsafe command %s %q unexpectedly passed", commandName(tc.kind), tc.args)
			}
		})
	}
}

func TestArtifactIntegrityControls(t *testing.T) {
	root, err := repositoryRoot()
	if err != nil {
		t.Fatal(err)
	}
	generated, err := generate(root, replayOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if err := validateArtifact(generated); err != nil {
		t.Fatal(err)
	}
	var decoded artifact
	if err := json.Unmarshal(generated, &decoded); err != nil {
		t.Fatal(err)
	}
	decoded.CanonicalPayload.Base64 = "A" + decoded.CanonicalPayload.Base64[1:]
	corruptCanonical := signedArtifact(t, decoded)
	if err := validateArtifact(corruptCanonical); err == nil {
		t.Fatal("corrupt canonical payload control unexpectedly passed")
	}
	decoded = mustArtifact(t, generated)
	decoded.OperationalDigest = "0" + decoded.OperationalDigest[1:]
	corruptDigest := signedArtifact(t, decoded)
	if err := validateArtifact(corruptDigest); err == nil {
		t.Fatal("corrupt operational digest control unexpectedly passed")
	}
	decoded = mustArtifact(t, generated)
	decoded.ForgedSidecarError = "forged operational payload accepted"
	if err := validateArtifact(signedArtifact(t, decoded)); err == nil {
		t.Fatal("forged sidecar/output control unexpectedly passed")
	}
}

func TestPinnedSourceControls(t *testing.T) {
	root, err := repositoryRoot()
	if err != nil {
		t.Fatal(err)
	}
	if err := verifyPinnedSource(root); err != nil {
		t.Fatal(err)
	}
	if err := verifyPinnedSourceWith(root, "v0.72.999", pinnedCommit, pinnedBlobs); err == nil {
		t.Fatal("wrong tag control unexpectedly passed")
	}
	if err := verifyPinnedSourceWith(root, pinnedTag, strings.Repeat("0", 40), pinnedBlobs); err == nil {
		t.Fatal("wrong commit control unexpectedly passed")
	}
	wrongBlobs := append([]sourceBlob(nil), pinnedBlobs...)
	wrongBlobs[0].SHA1 = strings.Repeat("0", 40)
	if err := verifyPinnedSourceWith(root, pinnedTag, pinnedCommit, wrongBlobs); err == nil {
		t.Fatal("wrong blob control unexpectedly passed")
	}
	for _, path := range []string{"go.mod", "go.sum"} {
		mutant := append([]sourceBlob(nil), pinnedBlobs...)
		for index := range mutant {
			if mutant[index].Path == path {
				mutant[index].SHA1 = strings.Repeat("0", 40)
			}
		}
		if err := verifyPinnedSourceWith(root, pinnedTag, pinnedCommit, mutant); err == nil {
			t.Fatalf("%s drift control unexpectedly passed", path)
		}
	}
	if err := verifyTagCommit(pinnedTag, strings.Repeat("0", 40), pinnedCommit); err == nil {
		t.Fatal("tag ref swap control unexpectedly passed")
	}
}

func TestProbeFunctionPathControl(t *testing.T) {
	if err := validateProbeSource(probeProgram); err != nil {
		t.Fatal(err)
	}
	mutant := strings.Replace(probeProgram, "runner.CanonicalOperationalPayload", "copiedCanonicalOperationalPayload", 1)
	if err := validateProbeStructure(mutant); err == nil {
		t.Fatal("function-path bypass mutant unexpectedly passed")
	}
	deadCode := strings.Replace(probeProgram, "CanonicalPayload: base64.StdEncoding.EncodeToString(canonical)", "CanonicalPayload: base64.StdEncoding.EncodeToString(item.OperationalPayload)", 1)
	if err := validateProbeStructure(deadCode); err == nil {
		t.Fatal("dead-code canonical output mutant unexpectedly passed")
	}
	deadCall := strings.Replace(probeProgram, "canonical, err := runner.CanonicalOperationalPayload", "discarded, err := runner.CanonicalOperationalPayload", 1)
	if err := validateProbeStructure(deadCall); err == nil {
		t.Fatal("dead-call canonical path mutant unexpectedly passed")
	}
}

func mustArtifact(t *testing.T, raw []byte) artifact {
	t.Helper()
	var decoded artifact
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatal(err)
	}
	return decoded
}

func signedArtifact(t *testing.T, value artifact) []byte {
	t.Helper()
	value.ArtifactDigest = ""
	unsigned, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	value.ArtifactDigest = sha256Hex(unsigned)
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return encoded
}
