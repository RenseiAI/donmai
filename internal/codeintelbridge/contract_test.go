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

func TestNativeFactsCanonicalAndOldConfigUntouched(t *testing.T) {
	facts := NativeFactsV1{ContractVersion: NativeFactsVersion, Root: "/abs/worktree", RepoPath: "pkg/sub", Tools: []string{"af_code_get_repo_map", "af_code_search_symbols"}}
	raw, digest, err := CanonicalNativeFacts(facts)
	if err != nil || len(digest) != 64 || !strings.Contains(string(raw), NativeFactsVersion) || !strings.Contains(string(raw), "/abs/worktree") {
		t.Fatalf("native facts raw=%s digest=%q err=%v", raw, digest, err)
	}
	decoded, gotDigest, err := DecodeNativeFacts(raw)
	if err != nil || gotDigest != digest || decoded.Root != facts.Root || decoded.RepoPath != facts.RepoPath || len(decoded.Tools) != 2 {
		t.Fatalf("decoded=%+v digest=%q err=%v", decoded, gotDigest, err)
	}
	again, againDigest, err := CanonicalNativeFacts(facts)
	if err != nil || string(again) != string(raw) || againDigest != digest {
		t.Fatal("native facts are not deterministic")
	}
	for _, bad := range []NativeFactsV1{{ContractVersion: NativeFactsVersion, Root: "relative/path", RepoPath: "", Tools: []string{"a"}}, {ContractVersion: NativeFactsVersion, Root: "/abs//worktree", RepoPath: "", Tools: []string{"a"}}, {ContractVersion: NativeFactsVersion, Root: "/abs/worktree", RepoPath: "../outside", Tools: []string{"a"}}, {ContractVersion: NativeFactsVersion, Root: "/abs/worktree", RepoPath: "", Tools: []string{"b", "a"}}, {ContractVersion: NativeFactsVersion, Root: "/abs/worktree", RepoPath: "pkg/sub", Tools: nil}} {
		if _, _, err := CanonicalNativeFacts(bad); err == nil {
			t.Fatalf("malformed native facts compiled: %+v", bad)
		}
	}
	if NativeFactsVersion == RuntimeConfigVersion {
		t.Fatal("native facts must carry a separate version")
	}
	old := RuntimeConfigV1{ContractVersion: RuntimeConfigVersion, RepoPath: "pkg/sub", Tools: []string{"af_code_get_repo_map"}}
	oldRaw, _, err := CanonicalRuntimeConfig(old)
	if err != nil || strings.Contains(string(oldRaw), "root") {
		t.Fatalf("historical runtime config drifted: %s err=%v", oldRaw, err)
	}
	if _, _, err := DecodeRuntimeConfig([]byte(`{"contractVersion":"donmai.code-intelligence-runtime-config/v1","repoPath":"","tools":["a"],"root":"/abs"}`)); err == nil {
		t.Fatal("historical runtime config accepted a root field")
	}
}
