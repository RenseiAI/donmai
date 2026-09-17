package executioncell

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"strings"
	"testing"
)

const (
	protectedRuntimeMCPVectorsPath = "testdata/protected-runtime-mcp-v2-vectors.json"
	protectedRuntimeMCPSHAPath     = "testdata/protected-runtime-mcp-v2-vectors.json.sha256"
)

type protectedRuntimeMCPVectorFile struct {
	Version     int                         `json:"version"`
	Description string                      `json:"description"`
	Vectors     []protectedRuntimeMCPVector `json:"vectors"`
}

type protectedRuntimeMCPVector struct {
	Name                    string `json:"name"`
	Decoder                 string `json:"decoder"`
	JSON                    string `json:"json"`
	Valid                   bool   `json:"valid"`
	ExpectedCanonicalJSON   string `json:"expectedCanonicalJson,omitempty"`
	ExpectedReferenceDigest string `json:"expectedReferenceDigest,omitempty"`
}

func loadProtectedRuntimeMCPVectors(t *testing.T) protectedRuntimeMCPVectorFile {
	t.Helper()
	raw, err := os.ReadFile(protectedRuntimeMCPVectorsPath)
	if err != nil {
		t.Fatalf("read protected runtime MCP vectors: %v", err)
	}
	pin, err := os.ReadFile(protectedRuntimeMCPSHAPath)
	if err != nil {
		t.Fatalf("read protected runtime MCP vector checksum: %v", err)
	}
	want := strings.Fields(string(pin))
	if len(want) != 2 || want[1] != "protected-runtime-mcp-v2-vectors.json" {
		t.Fatalf("protected runtime MCP checksum has invalid format: %q", pin)
	}
	sum := sha256.Sum256(raw)
	if got := hex.EncodeToString(sum[:]); got != want[0] {
		t.Fatalf("protected runtime MCP vector checksum = %s, want %s", got, want[0])
	}
	var vectors protectedRuntimeMCPVectorFile
	if err := json.Unmarshal(raw, &vectors); err != nil {
		t.Fatalf("decode protected runtime MCP vectors: %v", err)
	}
	if vectors.Version != 1 || vectors.Description == "" || len(vectors.Vectors) == 0 {
		t.Fatalf("protected runtime MCP vector envelope is incomplete: %+v", vectors)
	}
	return vectors
}

func decodeProtectedRuntimeMCPVector(vector protectedRuntimeMCPVector) ([]byte, string, error) {
	raw := []byte(vector.JSON)
	switch vector.Decoder {
	case "requirement-v1":
		value, err := DecodeProtectedRuntimeMCPConfigRequirementV1(raw)
		if err != nil {
			return nil, "", err
		}
		canonical, err := CanonicalJSON(value)
		return canonical, "", err
	case "materialization-v1":
		value, err := DecodeProtectedRuntimeMCPConfigMaterializationV1(raw)
		if err != nil {
			return nil, "", err
		}
		canonical, err := CanonicalJSON(value)
		if err != nil {
			return nil, "", err
		}
		digest, err := DigestProtectedRuntimeMCPConfigReference(value)
		return canonical, digest, err
	case "requirement-v2":
		value, err := DecodeProtectedRuntimeMCPConfigRequirementV2(raw)
		if err != nil {
			return nil, "", err
		}
		canonical, err := CanonicalJSON(value)
		return canonical, "", err
	case "materialization-v2":
		value, err := DecodeProtectedRuntimeMCPConfigMaterializationV2(raw)
		if err != nil {
			return nil, "", err
		}
		canonical, err := CanonicalJSON(value)
		if err != nil {
			return nil, "", err
		}
		digest, err := DigestProtectedRuntimeMCPConfigReferenceV2(value)
		return canonical, digest, err
	default:
		return nil, "", &unknownProtectedRuntimeMCPVectorDecoderError{decoder: vector.Decoder}
	}
}

type unknownProtectedRuntimeMCPVectorDecoderError struct{ decoder string }

func (e *unknownProtectedRuntimeMCPVectorDecoderError) Error() string {
	return "unknown protected runtime MCP vector decoder " + e.decoder
}

func assertProtectedRuntimeMCPVector(t *testing.T, vector protectedRuntimeMCPVector) {
	t.Helper()
	canonical, digest, err := decodeProtectedRuntimeMCPVector(vector)
	if !vector.Valid {
		if err == nil {
			t.Fatalf("invalid vector decoded successfully: %s", canonical)
		}
		return
	}
	if err != nil {
		t.Fatalf("valid vector rejected: %v", err)
	}
	if vector.ExpectedCanonicalJSON == "" || string(canonical) != vector.ExpectedCanonicalJSON {
		t.Fatalf("canonical JSON = %s, want %s", canonical, vector.ExpectedCanonicalJSON)
	}
	if digest != vector.ExpectedReferenceDigest {
		t.Fatalf("reference digest = %q, want %q", digest, vector.ExpectedReferenceDigest)
	}
}

func TestProtectedRuntimeMCPConfigV1GoldenCompatibility(t *testing.T) {
	t.Parallel()
	vectors := loadProtectedRuntimeMCPVectors(t)
	count := 0
	for _, vector := range vectors.Vectors {
		if vector.Decoder != "requirement-v1" && vector.Decoder != "materialization-v1" {
			continue
		}
		if !vector.Valid {
			continue
		}
		count++
		t.Run(vector.Name, func(t *testing.T) { assertProtectedRuntimeMCPVector(t, vector) })
	}
	if count != 2 {
		t.Fatalf("valid v1 baseline vector count = %d, want 2", count)
	}
}

func TestProtectedRuntimeMCPConfigV1RejectsV2Fields(t *testing.T) {
	t.Parallel()
	vectors := loadProtectedRuntimeMCPVectors(t)
	count := 0
	for _, vector := range vectors.Vectors {
		if vector.Name != "v1-requirement-with-authorization-source" {
			continue
		}
		count++
		assertProtectedRuntimeMCPVector(t, vector)
	}
	if count != 1 {
		t.Fatalf("v1 closure vector count = %d, want 1", count)
	}
}

func TestProtectedRuntimeMCPConfigV2GoldenVectors(t *testing.T) {
	t.Parallel()
	vectors := loadProtectedRuntimeMCPVectors(t)
	count := 0
	for _, vector := range vectors.Vectors {
		if vector.Decoder != "requirement-v2" && vector.Decoder != "materialization-v2" {
			continue
		}
		count++
		t.Run(vector.Name, func(t *testing.T) { assertProtectedRuntimeMCPVector(t, vector) })
	}
	if count < 12 {
		t.Fatalf("v2 vector count = %d, want at least 12", count)
	}
}

func TestProtectedRuntimeMCPConfigV2RejectsClosedShapeMutations(t *testing.T) {
	t.Parallel()
	vectors := loadProtectedRuntimeMCPVectors(t)
	count := 0
	for _, vector := range vectors.Vectors {
		if vector.Valid || (vector.Decoder != "requirement-v2" && vector.Decoder != "materialization-v2") {
			continue
		}
		count++
		t.Run(vector.Name, func(t *testing.T) { assertProtectedRuntimeMCPVector(t, vector) })
	}
	if count < 10 {
		t.Fatalf("invalid v2 vector count = %d, want at least 10", count)
	}
}

func TestProtectedRuntimeMCPConfigV2DigestBindsAuthorizationSource(t *testing.T) {
	t.Parallel()
	vectors := loadProtectedRuntimeMCPVectors(t)
	for _, vector := range vectors.Vectors {
		if vector.Name != "v2-materialization" {
			continue
		}
		value, err := DecodeProtectedRuntimeMCPConfigMaterializationV2([]byte(vector.JSON))
		if err != nil {
			t.Fatal(err)
		}
		for name, mutate := range map[string]func(*ProtectedRuntimeMCPConfigMaterializationV2){
			"config requirement": func(v *ProtectedRuntimeMCPConfigMaterializationV2) {
				v.AuthorizationSource.ConfigRequirementID = "example.session-config/other"
			},
			"config reference": func(v *ProtectedRuntimeMCPConfigMaterializationV2) {
				v.AuthorizationSource.ConfigReferenceDigest = strings.Repeat("1", 64)
			},
			"file reference": func(v *ProtectedRuntimeMCPConfigMaterializationV2) {
				v.AuthorizationSource.FileReferenceDigest = strings.Repeat("2", 64)
			},
			"helper command": func(v *ProtectedRuntimeMCPConfigMaterializationV2) {
				v.AuthorizationSource.HelperCommandDigest = strings.Repeat("3", 64)
			},
		} {
			t.Run(name, func(t *testing.T) {
				candidate := value
				mutate(&candidate)
				if err := ValidateProtectedRuntimeMCPConfigMaterializationV2(candidate); err == nil {
					t.Fatal("authorization source mutation accepted with stale outer digest")
				}
			})
		}
		return
	}
	t.Fatal("v2 materialization vector is missing")
}

func TestProtectedRuntimeMCPConfigV2ValidationIsStructuralOnly(t *testing.T) {
	t.Parallel()
	vectors := loadProtectedRuntimeMCPVectors(t)
	for _, vector := range vectors.Vectors {
		if vector.Name != "v2-materialization-fabricated-references-structurally-valid" {
			continue
		}
		assertProtectedRuntimeMCPVector(t, vector)
		return
	}
	t.Fatal("structural-only materialization vector is missing")
}
