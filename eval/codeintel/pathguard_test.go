package codeintel

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// writeFakeBinary drops an executable file named `name` into a fresh temp dir
// and returns the dir. The file is a tiny shell script so an actual exec of it
// would succeed on the host — proving the guard is about reachability, not a
// broken file.
func writeFakeBinary(t *testing.T, name string) string {
	t.Helper()
	dir := t.TempDir()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte("#!/bin/sh\necho fake\n"), 0o755); err != nil {
		t.Fatalf("write fake binary: %v", err)
	}
	return dir
}

func TestBinaryOnPath_FindsAndMisses(t *testing.T) {
	donmaiDir := writeFakeBinary(t, "donmai")
	other := t.TempDir() // no donmai here
	path := strings.Join([]string{other, donmaiDir}, pathListSep)

	got, found := BinaryOnPath("donmai", path)
	if !found {
		t.Fatal("expected to find donmai on the composed PATH")
	}
	if filepath.Dir(got) != donmaiDir {
		t.Errorf("resolved %s, want a file under %s", got, donmaiDir)
	}
	if _, found := BinaryOnPath("donmai", other); found {
		t.Error("must not find donmai on a PATH that lacks it")
	}
}

// TestScrubBinaryFromEnv_StripsControlArm is the core contamination-guard proof:
// after scrubbing, donmai is unreachable, a sibling tool in a different dir
// survives, and — critically — the WITHOUT-arm env genuinely cannot exec
// `donmai` (exec.LookPath under the scrubbed PATH fails).
func TestScrubBinaryFromEnv_StripsControlArm(t *testing.T) {
	donmaiDir := writeFakeBinary(t, "donmai")
	toolDir := writeFakeBinary(t, "som-other-tool")
	basePATH := strings.Join([]string{toolDir, donmaiDir}, pathListSep)
	env := []string{"HOME=/home/x", "PATH=" + basePATH}

	scrubbed, dropped := ScrubBinaryFromEnv(env, "donmai")

	if len(dropped) != 1 || dropped[0] != donmaiDir {
		t.Fatalf("dropped = %v, want exactly [%s]", dropped, donmaiDir)
	}
	scrubbedPATH := envPath(scrubbed)
	if _, found := BinaryOnPath("donmai", scrubbedPATH); found {
		t.Error("donmai still reachable after scrub — control is contaminated")
	}
	// The sibling tool in a different directory must survive the scrub.
	if _, found := BinaryOnPath("som-other-tool", scrubbedPATH); !found {
		t.Error("scrub over-stripped: an unrelated tool in a separate dir was removed")
	}

	// VerifyControlClean: passes on scrubbed, fails on the original.
	if err := VerifyControlClean(scrubbed, "donmai"); err != nil {
		t.Errorf("VerifyControlClean(scrubbed) = %v, want nil", err)
	}
	if err := VerifyControlClean(env, "donmai"); err == nil {
		t.Error("VerifyControlClean(original) must report contamination")
	}

	// Prove the agent literally cannot exec `donmai` under the scrubbed PATH.
	if runtime.GOOS != "windows" {
		lp := exec.LookPath
		t.Setenv("PATH", scrubbedPATH)
		if _, err := lp("donmai"); err == nil {
			t.Error("exec.LookPath resolved donmai under the scrubbed PATH — the control could still run it")
		}
	}
}

// TestPrependPath_MakesWithArmReachable proves the WITH arm can reach a
// freshly-built donmai in a dedicated dir via PATH prepend, without the base
// PATH needing it.
func TestPrependPath_MakesWithArmReachable(t *testing.T) {
	donmaiDir := writeFakeBinary(t, "donmai")
	env := []string{"PATH=" + t.TempDir()} // base PATH lacks donmai
	withEnv := PrependPath(env, donmaiDir)
	if _, found := BinaryOnPath("donmai", envPath(withEnv)); !found {
		t.Error("WITH arm must resolve donmai after PrependPath")
	}
}
