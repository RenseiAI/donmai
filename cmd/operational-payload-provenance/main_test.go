package main

import (
	"encoding/json"
	"strings"
	"testing"
)

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
