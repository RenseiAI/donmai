package credentials

import (
	"os"
	"testing"
)

// TestBlocklistJSONParity is the in-repo drift canary: it fails whenever
// AgentEnvBlocklist is edited without regenerating blocklist.json. It reads
// the committed artifact and compares it byte-for-byte against a fresh
// render of the Go source of truth.
//
// It runs under `make test` (go test -race ./...) and `make verify-generated`,
// so hand-editing the Go var and forgetting `make generate` turns those
// lanes red.
//
// Demo of the red→green loop: add or remove an entry in AgentEnvBlocklist
// without running `make generate` → this test fails (byte mismatch, and the
// error names the new digest). Run `make generate` and commit → green.
func TestBlocklistJSONParity(t *testing.T) {
	const path = "blocklist.json"
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v (run `make generate`)", path, err)
	}
	want, digest, err := RenderBlocklistJSON(AgentEnvBlocklist)
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	if string(got) != string(want) {
		t.Errorf("%s is stale (does not match a fresh render of AgentEnvBlocklist, digest %s).\n"+
			"The Go var was changed without regenerating. Run `make generate` and commit the result.", path, digest)
	}
}

// TestBlocklistDigestMatchesRenseiTUI pins the shared cross-repo digest so a
// change here cannot silently break the "one hash proves one list" tie with
// rensei-tui's generated baseline (daemon/credentials/generated_blocklist.go,
// GeneratedOSSBlocklistDigest). If AgentEnvBlocklist legitimately changes,
// this constant is updated deliberately in the same commit — and rensei-tui's
// blocklist-freshness CI (which regenerates from this repo) will require the
// matching update there.
func TestBlocklistDigestMatchesRenseiTUI(t *testing.T) {
	const sharedDigest = "43a6ca3658b7ded046e4e50b8b79cd4bb55c20d6d395afe50181a6c9328abf44"
	if got := BlocklistDigest(AgentEnvBlocklist); got != sharedDigest {
		t.Errorf("canonical digest = %q; the pinned cross-repo shared digest = %q.\n"+
			"AgentEnvBlocklist changed: update this constant AND expect rensei-tui/platform freshness checks to require their regenerated copies.",
			got, sharedDigest)
	}
}
