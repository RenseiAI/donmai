package afcli

import (
	"reflect"
	"testing"

	providerpi "github.com/RenseiAI/donmai/provider/harness/pi"
)

func TestPiTrustedExtensionsThreadFromCompiledConfig(t *testing.T) {
	t.Parallel()
	want := []providerpi.TrustedExtensionIdentity{{ID: "reviewed-pack", Digest: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}}
	opts := agentRunOptions(Config{PiTrustedExtensions: want}, "embedder")
	if !reflect.DeepEqual(opts.piTrustedExtensions, want) {
		t.Fatalf("agent run options trusted extensions = %+v, want %+v", opts.piTrustedExtensions, want)
	}
	hints := agentRunCtorHints{PiTrustedExtensions: opts.piTrustedExtensions}
	if got := piCtorOptions(hints).TrustedExtensions; !reflect.DeepEqual(got, want) {
		t.Fatalf("pi constructor trusted extensions = %+v, want %+v", got, want)
	}
	// Both seams clone the slice so later caller mutation cannot alter the
	// provider trust root after construction.
	want[0].ID = "mutated"
	if opts.piTrustedExtensions[0].ID != "reviewed-pack" || piCtorOptions(hints).TrustedExtensions[0].ID != "reviewed-pack" {
		t.Fatal("compiled trusted extension identities alias caller-owned memory")
	}
}
