package codeintelbridge

import (
	"strings"
	"testing"
)

func TestRuntimeConfigCanonicalAndDeliveryCloned(t *testing.T) {
	config := RuntimeConfigV1{ContractVersion: RuntimeConfigVersion, Tools: []string{"af_code_get_repo_map", "af_code_search_symbols"}}
	raw, digest, err := CanonicalRuntimeConfig(config)
	if err != nil || !strings.Contains(string(raw), RuntimeConfigVersion) || len(digest) != 64 {
		t.Fatalf("runtime config raw=%s digest=%q err=%v", raw, digest, err)
	}
	decoded, gotDigest, err := DecodeRuntimeConfig(raw)
	if err != nil || gotDigest != digest || len(decoded.Tools) != 2 {
		t.Fatalf("decoded=%+v digest=%q err=%v", decoded, gotDigest, err)
	}
	first := Delivery()
	second := Delivery()
	if first.Digest != second.Digest || first.ID != DeliveryID || !first.Required {
		t.Fatalf("delivery drift: %+v %+v", first, second)
	}
	first.Source[0] = 'x'
	if second.Source[0] == 'x' {
		t.Fatal("delivery source aliases caller memory")
	}
}
