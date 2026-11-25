//go:build darwin || linux

package afcli

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"

	"github.com/RenseiAI/donmai/afclient"
	"github.com/RenseiAI/donmai/tempown"
)

// doctorTempFixture builds a temp folder with a shared tool cache, a donmai
// cache and an unrelated entry. Nothing in it is a recorded daemon artefact,
// so --fix's janitor has nothing to remove and only the tool-cache step can
// change it.
func doctorTempFixture(t *testing.T) (root string, toolCaches, kept []string) {
	t.Helper()
	root = t.TempDir()
	mk := func(rel string) string {
		p := filepath.Join(root, rel)
		if err := os.MkdirAll(p, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(p, "entry"), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	toolCaches = []string{mk("tsx-501"), mk("node-compile-cache"), mk("go-build77")}
	kept = []string{mk("donmai-pi-ext-cache"), mk("tmp.keepme")}
	return root, toolCaches, kept
}

func runDoctorTemp(t *testing.T, mock *mockDaemon, tty bool, stdin string, args ...string) (string, error) {
	t.Helper()
	factory := func(afclient.DaemonConfig) daemonDoer { return mock }
	cmd := newDaemonDoctorTempCmdWithIO(factory, "donmai", doctorTempIO{
		isTTY: func() bool { return tty },
		in:    strings.NewReader(stdin),
	})
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs(args)
	err := cmd.Execute()
	return out.String(), err
}

func requirePresent(t *testing.T, paths []string, present bool) {
	t.Helper()
	for _, p := range paths {
		_, err := os.Stat(p)
		if present && err != nil {
			t.Errorf("%s was removed: %v", filepath.Base(p), err)
		}
		if !present && !os.IsNotExist(err) {
			t.Errorf("%s survived: %v", filepath.Base(p), err)
		}
	}
}

func TestDoctorTempIsReachableUnderHostAndDaemonDoctor(t *testing.T) {
	root := &cobra.Command{Use: "donmai"}
	RegisterCommands(root, Config{})
	for _, path := range [][]string{{"host", "doctor", "temp"}, {"daemon", "doctor", "temp"}} {
		cmd, _, err := root.Find(path)
		if err != nil || cmd.Name() != "temp" || cmd.Parent().Name() != "doctor" {
			t.Errorf("%v resolved to %v (err %v)", path, cmd, err)
		}
	}
	if NewDoctorTempCmd(Config{}).Name() != "temp" {
		t.Error("NewDoctorTempCmd does not build the temp leaf")
	}
}

func TestDoctorTempReportsByProducer(t *testing.T) {
	root, _, _ := doctorTempFixture(t)
	out, err := runDoctorTemp(t, &mockDaemon{}, false, "", "--root", root)
	if err != nil {
		t.Fatalf("doctor temp: %v\n%s", err, out)
	}
	for _, needle := range []string{"tool-cache", "tsx-*", "daemon-cache", "Boot cleanup:"} {
		if !strings.Contains(out, needle) {
			t.Errorf("report lacks %q:\n%s", needle, out)
		}
	}
}

func TestDoctorTempJSON(t *testing.T) {
	root, _, _ := doctorTempFixture(t)
	out, err := runDoctorTemp(t, &mockDaemon{}, false, "", "--root", root, "--json")
	if err != nil {
		t.Fatalf("doctor temp --json: %v", err)
	}
	var doc struct {
		Report struct {
			Root   string `json:"root"`
			Groups []struct {
				Class string `json:"class"`
			} `json:"groups"`
		} `json:"report"`
	}
	if err := json.Unmarshal([]byte(out), &doc); err != nil {
		t.Fatalf("decode: %v\n%s", err, out)
	}
	if doc.Report.Root != filepath.Clean(root) || len(doc.Report.Groups) == 0 {
		t.Fatalf("json report = %+v", doc)
	}
}

func TestDoctorTempIncludeToolCachesRequiresFix(t *testing.T) {
	root, toolCaches, _ := doctorTempFixture(t)
	if _, err := runDoctorTemp(t, &mockDaemon{}, true, "y\n", "--root", root, "--include-tool-caches"); err == nil {
		t.Fatal("--include-tool-caches without --fix was accepted")
	}
	requirePresent(t, toolCaches, true)
}

func TestDoctorTempToolCacheRemovalRules(t *testing.T) {
	idle := &afclient.DaemonStatusResponse{ActiveSessions: 0}
	busy := &afclient.DaemonStatusResponse{ActiveSessions: 3}
	cases := map[string]struct {
		mock    *mockDaemon
		tty     bool
		stdin   string
		yes     bool
		removed bool
	}{
		"no terminal and no --yes refuses":   {mock: &mockDaemon{statusResp: idle}, tty: false},
		"terminal and the operator declines": {mock: &mockDaemon{statusResp: idle}, tty: true, stdin: "n\n"},
		"seats running refuses even with --yes": {
			mock: &mockDaemon{statusResp: busy}, yes: true,
		},
		"daemon unreachable refuses": {
			mock: &mockDaemon{statusErr: errors.New("connection refused")}, yes: true,
		},
		"terminal and the operator accepts": {mock: &mockDaemon{statusResp: idle}, tty: true, stdin: "y\n", removed: true},
		"--yes without a terminal":          {mock: &mockDaemon{statusResp: idle}, yes: true, removed: true},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			root, toolCaches, kept := doctorTempFixture(t)
			args := []string{"--root", root, "--fix", "--include-tool-caches"}
			if tc.yes {
				args = append(args, "--yes")
			}
			out, err := runDoctorTemp(t, tc.mock, tc.tty, tc.stdin, args...)
			if tc.removed {
				if err != nil {
					t.Fatalf("doctor temp: %v\n%s", err, out)
				}
				requirePresent(t, toolCaches, false)
			} else {
				if err == nil || !strings.Contains(out, "Tool caches:      not removed") {
					t.Fatalf("want a refusal, got err=%v\n%s", err, out)
				}
				requirePresent(t, toolCaches, true)
			}
			requirePresent(t, kept, true)
		})
	}
}

func TestDoctorTempFixRemovesOnlyADeadOwnersRecordedArtefact(t *testing.T) {
	root, toolCaches, kept := doctorTempFixture(t)
	reaped := exec.Command("true")
	if err := reaped.Run(); err != nil {
		t.Fatal(err)
	}
	live := exec.Command("sleep", "60")
	if err := live.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = live.Process.Kill(); _ = live.Wait() })
	mk := func(name string) string {
		p := filepath.Join(root, name)
		if err := os.Mkdir(p, 0o700); err != nil {
			t.Fatal(err)
		}
		return p
	}
	dead := mk(tempown.PrefixClaudeUsage + "p" + strconv.Itoa(reaped.Process.Pid) + "-1")
	liveOwned := mk(tempown.PrefixSignalGuard + "p" + strconv.Itoa(live.Process.Pid) + "-2")
	legacy := mk(tempown.PrefixClaudeLogin + "3920481723")
	time.Sleep(1100 * time.Millisecond)

	out, err := runDoctorTemp(t, &mockDaemon{}, false, "", "--root", root, "--fix", "--grace", "1s")
	if err != nil {
		t.Fatalf("doctor temp --fix: %v\n%s", err, out)
	}
	if !strings.Contains(out, "Daemon artefacts: removed 1") {
		t.Errorf("fix summary missing:\n%s", out)
	}
	requirePresent(t, []string{dead}, false)
	requirePresent(t, []string{liveOwned, legacy}, true)
	requirePresent(t, toolCaches, true)
	requirePresent(t, kept, true)
}
