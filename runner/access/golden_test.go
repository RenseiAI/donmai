package access

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/RenseiAI/donmai/agent"
)

// goldenVectorsPath is the vendored copy of the canonical
// donmai-architecture/golden/narrow-only-vectors.json. The sibling .sha256 file
// pins its digest; TestGoldenVectors_VendoredChecksum guards drift.
const (
	goldenVectorsPath = "testdata/narrow-only-vectors.json"
	goldenSHAPath     = "testdata/narrow-only-vectors.json.sha256"
)

// goldenFile is the on-disk schema of narrow-only-vectors.json (P3-SPEC §3.2).
type goldenFile struct {
	Version     int            `json:"version"`
	Description string         `json:"description"`
	Vectors     []goldenVector `json:"vectors"`
}

type goldenVector struct {
	Name                 string             `json:"name"`
	PlatformAllowed      []string           `json:"platformAllowed"`
	Machine              *ModelAccessConfig `json:"machine"` // nil for the nil-block identity vectors
	Workload             string             `json:"workload"`
	Company              string             `json:"company"`
	Model                string             `json:"model"`
	Requested            string             `json:"requested"`
	Expected             *expectedCell      `json:"expected"`      // nil ⇒ expect a deny
	ExpectedError        string             `json:"expectedError"` // "AccessDenied" when Expected is nil
	ExpectedEffectiveSet []string           `json:"expectedEffectiveSet"`
	ExpectedDropped      []string           `json:"expectedDropped"`
}

type expectedCell struct {
	AuthMode string `json:"authMode"`
	Host     string `json:"host"`
	Model    string `json:"model"`
}

func loadGolden(t *testing.T) goldenFile {
	t.Helper()
	raw, err := os.ReadFile(goldenVectorsPath)
	if err != nil {
		t.Fatalf("read vendored golden vectors: %v", err)
	}
	var gf goldenFile
	if err := json.Unmarshal(raw, &gf); err != nil {
		t.Fatalf("decode golden vectors: %v", err)
	}
	if len(gf.Vectors) == 0 {
		t.Fatal("golden vectors file has no vectors")
	}
	return gf
}

func toAllowedSet(modes []string) map[AuthMode]bool {
	set := make(map[AuthMode]bool, len(modes))
	for _, m := range modes {
		set[AuthMode(m)] = true
	}
	return set
}

// TestGoldenVectors_VendoredChecksum is the anti-drift parity gate (P3-SPEC §3.3):
// the vendored bytes must match the SHA-256 pinned alongside them, which in turn
// equals the canonical donmai-architecture original. A stale or hand-edited
// vendor reds CI. Mirrors the matrix/ provider-matrix.json discipline.
func TestGoldenVectors_VendoredChecksum(t *testing.T) {
	raw, err := os.ReadFile(goldenVectorsPath)
	if err != nil {
		t.Fatalf("read vendored vectors: %v", err)
	}
	sum := sha256.Sum256(raw)
	got := hex.EncodeToString(sum[:])

	pinRaw, err := os.ReadFile(goldenSHAPath)
	if err != nil {
		t.Fatalf("read pinned checksum: %v", err)
	}
	// Format: "<hex>  <filename>" (shasum -a 256 style); take field 0.
	want := strings.Fields(string(pinRaw))
	if len(want) == 0 {
		t.Fatalf("checksum file %s is empty", goldenSHAPath)
	}
	if got != want[0] {
		t.Errorf("vendored golden vectors are STALE: sha256(%s)=%s, pinned=%s.\n"+
			"Re-vendor from donmai-architecture/golden/narrow-only-vectors.json and update the .sha256 file.",
			filepath.Base(goldenVectorsPath), got, want[0])
	}
}

// TestGoldenVectors_ResolveMachineCell runs every committed vector through
// ResolveMachineCell + EffectiveSet and asserts the expected NarrowedCell (or
// deny) and the expected effective set. This is the Go half of the cross-language
// parity gate; the platform TS test honors the same file's expectedEffectiveSet /
// expectedDropped.
func TestGoldenVectors_ResolveMachineCell(t *testing.T) {
	gf := loadGolden(t)
	for _, v := range gf.Vectors {
		v := v
		t.Run(v.Name, func(t *testing.T) {
			ceiling := toAllowedSet(v.PlatformAllowed)

			// Effective-set parity (the surface shared with the TS reader).
			eff := EffectiveSet(v.Machine, v.Workload, v.Company, v.Model, ceiling)
			assertSameSet(t, "effective set", eff, v.ExpectedEffectiveSet)
			assertSubset(t, eff, ceiling) // belt-and-suspenders: never exceeds the ceiling.

			got, err := ResolveMachineCell(
				v.Machine, v.Workload, v.Company, v.Model,
				AuthMode(v.Requested), ceiling,
			)

			if v.Expected == nil {
				// Expect a deny.
				var denied *AccessDeniedError
				if !errors.As(err, &denied) {
					t.Fatalf("expected AccessDeniedError, got cell=%+v err=%v", got, err)
				}
				if v.ExpectedError != "" && v.ExpectedError != "AccessDenied" {
					t.Errorf("expectedError=%q not understood (only AccessDenied supported)", v.ExpectedError)
				}
				return
			}

			if err != nil {
				t.Fatalf("expected NarrowedCell %+v, got error: %v", *v.Expected, err)
			}
			if string(got.AuthMode) != v.Expected.AuthMode {
				t.Errorf("authMode: got %q, want %q", got.AuthMode, v.Expected.AuthMode)
			}
			if got.Host != v.Expected.Host {
				t.Errorf("host: got %q, want %q", got.Host, v.Expected.Host)
			}
			if got.Model != v.Expected.Model {
				t.Errorf("model: got %q, want %q", got.Model, v.Expected.Model)
			}
			// The picked mode must live inside the ceiling — the core invariant.
			if !ceiling[got.AuthMode] {
				t.Errorf("INVARIANT VIOLATION: picked authMode %q ∉ platformAllowed %v",
					got.AuthMode, v.PlatformAllowed)
			}
		})
	}
}

// assertSameSet compares a resolved auth-mode set against an expected slice.
func assertSameSet(t *testing.T, label string, got map[AuthMode]bool, want []string) {
	t.Helper()
	gotList := make([]string, 0, len(got))
	for m := range got {
		gotList = append(gotList, string(m))
	}
	wantList := append([]string(nil), want...)
	sort.Strings(gotList)
	sort.Strings(wantList)
	if strings.Join(gotList, ",") != strings.Join(wantList, ",") {
		t.Errorf("%s mismatch: got %v, want %v", label, gotList, wantList)
	}
}

// assertSubset fails if `sub` contains any key not present in `super`.
func assertSubset(t *testing.T, sub, super map[AuthMode]bool) {
	t.Helper()
	for m := range sub {
		if !super[m] {
			t.Errorf("INVARIANT VIOLATION: %q ∈ effective set but ∉ platformAllowed", m)
		}
	}
}

// TestGoldenVectors_AuthModesAreCanonical guards that every authMode referenced
// by the golden corpus is in the canonical 5-mode set, so a typo'd mode can't
// silently pass by being absent from every ceiling.
func TestGoldenVectors_AuthModesAreCanonical(t *testing.T) {
	canonical := map[AuthMode]bool{
		agent.AuthBYOK: true, agent.AuthMetered: true, agent.AuthShared: true,
		agent.AuthHostSession: true, agent.AuthLocal: true,
	}
	gf := loadGolden(t)
	for _, v := range gf.Vectors {
		for _, m := range v.PlatformAllowed {
			if !canonical[AuthMode(m)] {
				t.Errorf("vector %q: platformAllowed mode %q is not canonical", v.Name, m)
			}
		}
		if v.Requested != "" && !canonical[AuthMode(v.Requested)] {
			t.Errorf("vector %q: requested mode %q is not canonical", v.Name, v.Requested)
		}
	}
}
