package executioncell

import (
	"bytes"
	"encoding/json"
	"os"
	"strings"
	"testing"
)

const capabilityRealizationSelectionVectorsPath = "testdata/capability-realization-selection-v1.json"

type capabilityRealizationSelectionVectorFile struct {
	Version     int                                    `json:"version"`
	Description string                                 `json:"description"`
	Vectors     []capabilityRealizationSelectionVector `json:"vectors"`
}

type capabilityRealizationSelectionVector struct {
	Name                  string `json:"name"`
	InputKind             string `json:"inputKind"`
	JSON                  string `json:"json"`
	Valid                 bool   `json:"valid"`
	ExpectedAbsent        bool   `json:"expectedAbsent,omitempty"`
	ExpectedCanonicalJSON string `json:"expectedCanonicalJson,omitempty"`
	SelectionBytes        int    `json:"selectionBytes,omitempty"`
}

func loadCapabilityRealizationSelectionVectors(t *testing.T) capabilityRealizationSelectionVectorFile {
	t.Helper()
	raw, err := os.ReadFile(capabilityRealizationSelectionVectorsPath)
	if err != nil {
		t.Fatalf("read capability realization selection vectors: %v", err)
	}
	var vectors capabilityRealizationSelectionVectorFile
	if err := json.Unmarshal(raw, &vectors); err != nil {
		t.Fatalf("decode capability realization selection vectors: %v", err)
	}
	if vectors.Version != 1 || vectors.Description == "" || len(vectors.Vectors) == 0 {
		t.Fatalf("capability realization selection vector envelope is incomplete: %+v", vectors)
	}
	return vectors
}

func capabilityRealizationSelectionVectorRaw(t *testing.T, vector capabilityRealizationSelectionVector) []byte {
	t.Helper()
	raw := []byte(vector.JSON)
	if vector.SelectionBytes == 0 {
		return raw
	}
	if len(raw) > vector.SelectionBytes {
		t.Fatalf("vector %q base bytes = %d, exceeds requested %d", vector.Name, len(raw), vector.SelectionBytes)
	}
	return append(raw, bytes.Repeat([]byte(" "), vector.SelectionBytes-len(raw))...)
}

func decodeCapabilityRealizationSelectionVector(t *testing.T, vector capabilityRealizationSelectionVector) (*CapabilityRealizationSelectionV1, error) {
	t.Helper()
	raw := capabilityRealizationSelectionVectorRaw(t, vector)
	switch vector.InputKind {
	case "selection", "padded-selection":
		value, err := DecodeCapabilityRealizationSelectionV1(raw)
		if err != nil {
			return nil, err
		}
		return &value, nil
	case "operational-payload":
		return ExtractCapabilityRealizationSelectionV1(raw)
	default:
		t.Fatalf("vector %q has unknown input kind %q", vector.Name, vector.InputKind)
		return nil, nil
	}
}

func assertCapabilityRealizationSelectionVector(t *testing.T, vector capabilityRealizationSelectionVector) {
	t.Helper()
	value, err := decodeCapabilityRealizationSelectionVector(t, vector)
	if !vector.Valid {
		if err == nil {
			t.Fatalf("invalid vector decoded successfully: %+v", value)
		}
		return
	}
	if err != nil {
		t.Fatalf("valid vector rejected: %v", err)
	}
	if vector.ExpectedAbsent {
		if value != nil {
			t.Fatalf("absent vector returned selection: %+v", value)
		}
		return
	}
	if value == nil {
		t.Fatal("present vector returned no selection")
	}
	canonical, err := CanonicalCapabilityRealizationSelectionV1(*value)
	if err != nil {
		t.Fatalf("canonicalize decoded selection: %v", err)
	}
	if got := string(canonical); got != vector.ExpectedCanonicalJSON {
		t.Fatalf("canonical JSON = %s, want %s", got, vector.ExpectedCanonicalJSON)
	}
	roundTrip, err := DecodeCapabilityRealizationSelectionV1(canonical)
	if err != nil {
		t.Fatalf("decode canonical selection: %v", err)
	}
	canonicalRoundTrip, err := CanonicalCapabilityRealizationSelectionV1(roundTrip)
	if err != nil {
		t.Fatalf("canonicalize round trip: %v", err)
	}
	if !bytes.Equal(canonical, canonicalRoundTrip) {
		t.Fatalf("canonical round trip = %s, want %s", canonicalRoundTrip, canonical)
	}
}

func TestCapabilityRealizationSelectionV1GoldenVectors(t *testing.T) {
	t.Parallel()
	vectors := loadCapabilityRealizationSelectionVectors(t)
	for _, vector := range vectors.Vectors {
		vector := vector
		t.Run(vector.Name, func(t *testing.T) {
			assertCapabilityRealizationSelectionVector(t, vector)
		})
	}
}

func TestCapabilityRealizationSelectionV1Controls(t *testing.T) {
	t.Parallel()
	vectors := loadCapabilityRealizationSelectionVectors(t)
	required := map[string]bool{
		"case-alias-field":       false,
		"duplicate-outer-key":    false,
		"present-null":           false,
		"byte-limit-over-bound":  false,
		"byte-limit-exact-bound": false,
	}
	for _, vector := range vectors.Vectors {
		if _, ok := required[vector.Name]; !ok {
			continue
		}
		required[vector.Name] = true
		assertCapabilityRealizationSelectionVector(t, vector)
	}
	for name, found := range required {
		if !found {
			t.Fatalf("required control vector %q is missing", name)
		}
	}
}

func TestExtractCapabilityRealizationSelectionV1PreservesInputAndIndependence(t *testing.T) {
	t.Parallel()
	vectors := loadCapabilityRealizationSelectionVectors(t)
	selected := make(map[string]capabilityRealizationSelectionVector)
	for _, vector := range vectors.Vectors {
		if vector.Name == "operational-v1" || vector.Name == "operational-v2" {
			selected[vector.Name] = vector
		}
	}
	if len(selected) != 2 {
		t.Fatalf("operational independence vectors = %d, want 2", len(selected))
	}
	values := make(map[string]*CapabilityRealizationSelectionV1, len(selected))
	for name, vector := range selected {
		raw := capabilityRealizationSelectionVectorRaw(t, vector)
		before := bytes.Clone(raw)
		value, err := ExtractCapabilityRealizationSelectionV1(raw)
		if err != nil {
			t.Fatalf("extract %s: %v", name, err)
		}
		if value == nil {
			t.Fatalf("extract %s returned absent selection", name)
		}
		if !bytes.Equal(raw, before) {
			t.Fatalf("extract %s mutated operational bytes", name)
		}
		values[name] = value
	}
	if values["operational-v1"].AdapterVersion == values["operational-v2"].AdapterVersion {
		t.Fatalf("independent payloads collapsed to adapter %q", values["operational-v1"].AdapterVersion)
	}
}

func TestCapabilityRealizationSelectionV1RejectsLossyStringsAndKeepsCanonicalDomainClosed(t *testing.T) {
	t.Parallel()
	vectors := loadCapabilityRealizationSelectionVectors(t)
	var baseline capabilityRealizationSelectionVector
	for _, vector := range vectors.Vectors {
		if vector.Name == "explicit-v1" {
			baseline = vector
			break
		}
	}
	if baseline.Name == "" {
		t.Fatal("explicit-v1 vector is missing")
	}
	value, err := DecodeCapabilityRealizationSelectionV1([]byte(baseline.JSON))
	if err != nil {
		t.Fatalf("decode typed baseline: %v", err)
	}
	canonical, err := CanonicalCapabilityRealizationSelectionV1(value)
	if err != nil {
		t.Fatalf("canonicalize typed baseline: %v", err)
	}
	if _, err := DecodeCapabilityRealizationSelectionV1(canonical); err != nil {
		t.Fatalf("decode successful typed canonical bytes: %v", err)
	}

	for name, candidate := range map[string]CapabilityRealizationSelectionV1{
		"invalid Go UTF-8": func() CapabilityRealizationSelectionV1 {
			mutated := value
			mutated.HarnessID = string([]byte{0xff})
			return mutated
		}(),
		"oversized canonical value": func() CapabilityRealizationSelectionV1 {
			mutated := value
			mutated.HarnessID = strings.Repeat("h", maxCapabilityRealizationSelectionBytes)
			return mutated
		}(),
	} {
		t.Run(name, func(t *testing.T) {
			if err := ValidateCapabilityRealizationSelectionV1(candidate); err == nil {
				t.Fatal("invalid typed selection passed validation")
			}
			if _, err := CanonicalCapabilityRealizationSelectionV1(candidate); err == nil {
				t.Fatal("invalid typed selection produced canonical bytes")
			}
		})
	}

	for name, raw := range map[string][]byte{
		"invalid raw UTF-8":         bytes.Replace([]byte(baseline.JSON), []byte(`"harnessId":"codex"`), append([]byte(`"harnessId":"`), append([]byte{0xff}, []byte(`"`)...)...), 1),
		"unpaired surrogate escape": bytes.Replace([]byte(baseline.JSON), []byte(`"harnessId":"codex"`), []byte(`"harnessId":"\ud800"`), 1),
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := DecodeCapabilityRealizationSelectionV1(raw); err == nil {
				t.Fatal("lossy raw selection decoded successfully")
			}
		})
	}

	validPair := bytes.Replace([]byte(baseline.JSON), []byte(`"harnessId":"codex"`), []byte(`"harnessId":"codex-\ud83d\ude80"`), 1)
	paired, err := DecodeCapabilityRealizationSelectionV1(validPair)
	if err != nil {
		t.Fatalf("decode valid surrogate pair: %v", err)
	}
	if paired.HarnessID != "codex-🚀" {
		t.Fatalf("decoded surrogate pair harness = %q", paired.HarnessID)
	}
}
