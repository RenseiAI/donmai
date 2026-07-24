package credentials

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
)

// BlocklistArtifactPath is the repo-relative path (from the module root) of
// the generated, vendorable rendering of AgentEnvBlocklist. Cross-repo
// consumers vendor this file; see the package doc comment.
const BlocklistArtifactPath = "internal/credentials/blocklist.json"

// blocklistArtifact is the on-disk shape of blocklist.json. Field order is
// the emitted JSON key order (encoding/json writes struct fields in
// declaration order), so the render is deterministic.
type blocklistArtifact struct {
	Comment         string   `json:"_comment"`
	CanonicalSource string   `json:"canonicalSource"`
	Generator       string   `json:"generator"`
	Digest          string   `json:"digest"`
	Names           []string `json:"names"`
}

const (
	blocklistComment   = "GENERATED — do not hand-edit. Regenerate with `make generate`; verified by `make verify-generated` and internal/credentials TestBlocklistJSONParity."
	blocklistCanonical = "internal/credentials/blocklist.go (var AgentEnvBlocklist)"
	blocklistGenerator = "internal/credentials/gen"
)

// BlocklistDigest returns the canonical content digest for names: the
// lowercase-hex sha256 over each name followed by "\n", in order.
//
// This normalization is deliberately identical to rensei-tui's
// scripts/gen-blocklist.go, so donmai's blocklist.json and rensei-tui's
// generated Go baseline share a digest whenever they describe the same
// list — turning "are all repos in sync?" into a single hash comparison.
func BlocklistDigest(names []string) string {
	h := sha256.New()
	for _, n := range names {
		h.Write([]byte(n))
		h.Write([]byte("\n"))
	}
	return hex.EncodeToString(h.Sum(nil))
}

// RenderBlocklistJSON returns the deterministic bytes of blocklist.json for
// the given names, plus the content digest. Output has no timestamps and a
// stable key/element order, so identical input yields byte-identical output.
func RenderBlocklistJSON(names []string) (out []byte, digest string, err error) {
	digest = BlocklistDigest(names)
	doc := blocklistArtifact{
		Comment:         blocklistComment,
		CanonicalSource: blocklistCanonical,
		Generator:       blocklistGenerator,
		Digest:          digest,
		Names:           names,
	}
	b, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return nil, "", err
	}
	b = append(b, '\n') // trailing newline for POSIX-clean diffs
	return b, digest, nil
}
