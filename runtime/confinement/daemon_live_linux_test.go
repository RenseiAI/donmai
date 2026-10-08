//go:build linux

package confinement

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/RenseiAI/donmai/agent"
)

// TestLinux_DaemonPrivateDeniedEveryReadScope is the live proof, in every
// read scope, that a confined seat laid out the way a host lays out its
// state home — the control token (a sentinel here) directly in the state
// home, the session's work area beneath it — cannot read the token, cannot
// read a token minted there after the seat was prepared, cannot see either
// in a listing of the state home, and cannot read or write a token nested
// inside its own writable set; while ordinary seat work succeeds: reading
// and writing the work area, reading system files, and git where the host
// has it.
func TestLinux_DaemonPrivateDeniedEveryReadScope(t *testing.T) {
	if !linuxConfinementAvailable(t) {
		return
	}
	for _, scope := range []agent.ExecutionSecurityLevel{"", agent.FileReadWorkarea} {
		name := string(scope)
		if name == "" {
			name = "open"
		}
		t.Run(name, func(t *testing.T) {
			c, stateHome := newLinuxLiveConfiner(t, DefaultBackend(), nil)
			current, err := c.fingerprint()
			if err != nil {
				t.Fatalf("fingerprint: %v", err)
			}
			// The self-test is proven by TestLinux_SelfTestPassesBothModes;
			// this test drives one seat through Prepare.
			c.store(SelfTestRecord{
				Backend: current.backend, BackendVersion: current.backendVersion, ProbeSetVersion: ProbeSetVersion,
				ExecutableDigest: current.executableDigest, Passed: true,
				SessionModes: []agent.PromptSessionMode{agent.PromptModeAutonomous, agent.PromptModeHumanControlled},
				Probes:       []ProbeOutcome{{ID: "p", Pass: true}},
			})
			token := filepath.Join(stateHome, "control-token")
			if err := os.WriteFile(token, []byte("sentinel-control-token\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			ws := filepath.Join(stateHome, "worktrees", "s1")
			mut := filepath.Join(ws, "repo")
			state := filepath.Join(stateHome, "state", "s1")
			tmp := filepath.Join(stateHome, "tmp", "s1")
			nestedDir := filepath.Join(mut, "daemon-state")
			nested := filepath.Join(nestedDir, "control-token")
			for _, dir := range []string{filepath.Join(mut, ".git"), state, tmp, nestedDir} {
				if err := os.MkdirAll(dir, 0o700); err != nil {
					t.Fatal(err)
				}
			}
			if err := os.WriteFile(nested, []byte("sentinel-nested-token\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			plan, err := c.Prepare(Spec{
				SessionID: "live-daemon-" + name, HarnessID: "live", SessionMode: agent.PromptModeAutonomous,
				WorkareaRoot: ws, MutableLeaves: []string{mut}, HarnessState: []string{state}, SessionTmp: tmp,
				DeniedPaths:    []string{token, nested, nestedDir},
				DeniedListings: []string{stateHome},
				ReadScope:      scope,
			})
			if err != nil {
				t.Fatalf("Prepare: %v", err)
			}
			t.Cleanup(func() { _ = plan.Release() })
			// A token minted after the seat was prepared: a placeholder
			// cannot cover what did not exist, so only the emptied
			// directory keeps it out.
			late := filepath.Join(stateHome, "late-token")
			if err := os.WriteFile(late, []byte("sentinel-late-token\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			// Unconfined control: everything the seat is refused reads here.
			for _, file := range []string{token, late, nested} {
				if _, err := os.ReadFile(file); err != nil { //nolint:gosec // G304: the test's own sentinel.
					t.Fatalf("unconfined control: %s does not read: %v", filepath.Base(file), err)
				}
			}

			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
			defer cancel()
			launch := func(argv []string, env ...string) error {
				wrapped, err := plan.Command(argv)
				if err != nil {
					return err
				}
				code, err := HeadlessLauncher()(ctx, wrapped, append(plan.Environment(), env...), mut)
				if err == nil && code != 0 {
					err = fmt.Errorf("the seat exited %d", code)
				}
				return err
			}
			results := runSeatCheck(t, tmp, []seatCheckStep{
				{ID: "read_token", Op: "read", Target: token},
				{ID: "read_late_token", Op: "read", Target: late},
				{ID: "read_nested", Op: "read", Target: nested},
				{ID: "write_token", Op: "write", Target: token},
				{ID: "write_nested", Op: "write", Target: nested},
				{ID: "create_beside_token", Op: "write", Target: filepath.Join(stateHome, "planted")},
				{ID: "list_state_home", Op: "list", Target: stateHome},
				{ID: "write_work", Op: "write", Target: filepath.Join(mut, "work.txt")},
				{ID: "read_work", Op: "read", Target: filepath.Join(mut, "work.txt")},
				{ID: "read_system", Op: "read", Target: "/etc/hosts"},
			}, func(self, planPath string) (string, error) {
				return "", launch([]string{self, "-test.run=^$"}, seatCheckEnv+"="+planPath)
			})
			for _, id := range []string{"read_token", "read_late_token", "read_nested", "write_token", "write_nested", "create_beside_token"} {
				if res := results[id]; res.Err == "" {
					t.Errorf("%s: the seat got through the daemon-private deny", id)
				} else {
					t.Logf("%s refused: %s", id, res.Err)
				}
			}
			listing := results["list_state_home"]
			for _, leaf := range []string{"control-token", "late-token"} {
				if strings.Contains("\n"+listing.Output, "\n"+leaf+"\n") {
					t.Errorf("a listing of the state home shows %s: %q", leaf, listing.Output)
				}
			}
			t.Logf("listing of the state home from the seat: err=%q entries=%q", listing.Err, listing.Output)
			for _, id := range []string{"write_work", "read_work", "read_system"} {
				if res := results[id]; res.Err != "" {
					t.Errorf("%s refused: %s; seat work beneath the state home must succeed", id, res.Err)
				}
			}
			for file, want := range map[string]string{token: "sentinel-control-token\n", nested: "sentinel-nested-token\n"} {
				if raw, err := os.ReadFile(file); err != nil || string(raw) != want { //nolint:gosec // G304: the test's own sentinel.
					t.Errorf("%s changed on the host: %q %v", filepath.Base(file), raw, err)
				}
			}
			if exists(filepath.Join(stateHome, "planted")) {
				t.Error("a file landed beside the token")
			}

			// Ordinary seat work through a shell: the working directory
			// resolves, the work area lists, system files read, and git
			// commits where this host has it.
			marker := filepath.Join(tmp, "seat-work-ok")
			script := []string{
				`set -e`,
				`cd "` + mut + `"`,
				`test "$(pwd -P)" = "` + mut + `"`,
				`echo shell > shell.txt`,
				`grep -q shell shell.txt`,
				`ls . >/dev/null`,
				`cat /etc/os-release >/dev/null 2>&1 || cat /etc/hosts >/dev/null`,
			}
			if git, err := exec.LookPath("git"); err == nil {
				script = append(script,
					`mkdir -p repo2 && cd repo2`,
					`"`+git+`" init -q .`,
					`echo a > a.txt`,
					`"`+git+`" add a.txt`,
					`"`+git+`" -c user.name=probe -c user.email=probe@example.invalid commit -q -m first`,
					`"`+git+`" log --oneline | grep -q first`,
				)
			} else {
				t.Log("git is not installed here; not exercised")
			}
			script = append(script, `echo ok > "`+marker+`"`)
			if err := launch([]string{"/bin/sh", "-c", strings.Join(script, "\n")}, "HOME="+tmp, "GIT_CONFIG_NOSYSTEM=1"); err != nil {
				t.Fatalf("seat work beneath the state home: %v", err)
			}
			if !exists(marker) {
				t.Fatal("seat work beneath the state home left no marker")
			}
		})
	}
}
