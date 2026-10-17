package runner

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/RenseiAI/donmai/runtime/workarea"
)

// This fake models gh choosing the host user's repository unless --repo is
// explicit. Git still clones, commits and pushes through real local remotes.
func stubGhRepositorySelection(t *testing.T, failCreate bool) string {
	t.Helper()
	dir := t.TempDir()
	script := fmt.Sprintf(`#!/bin/sh
if [ "$1" = repo ]; then echo PUBLIC; exit 0; fi
command="$2"
url="$3"
shift 2
repo=""
while [ "$#" -gt 0 ]; do
 if [ "$1" = --repo ]; then repo="$2"; break; fi
 shift
done
if [ -z "$repo" ]; then
 printf '%%s host-user/project\n' "$command" >> "${0%%/*}/calls"
 echo 'repository lookup used host-user/project' >&2
 exit 1
fi
printf '%%s %%s\n' "$command" "$repo" >> "${0%%/*}/calls"
if [ "$command" = create ]; then
 if [ %[1]t = true ]; then echo 'pull request permission denied' >&2; exit 1; fi
 printf 'https://github.com/%%s/pull/17\n' "$repo"
 exit 0
fi
if [ "$command" = view ] && [ "$url" = "https://github.com/$repo/pull/17" ]; then
 printf '{"number":17,"url":"%%s","baseRefName":"main","headRefName":"session-branch","isDraft":false}\n' "$url"
 exit 0
fi
exit 1
`, failCreate)
	path := filepath.Join(dir, "gh")
	if err := os.WriteFile(path, []byte(script), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Chmod(path, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return filepath.Join(dir, "calls")
}

func TestRunBackstopConfiguredRepository(t *testing.T) {
	for _, declared := range []bool{false, true} {
		for _, failCreate := range []bool{false, true} {
			t.Run(fmt.Sprintf("declared=%t/failure=%t", declared, failCreate), func(t *testing.T) {
				calls := stubGhRepositorySelection(t, failCreate)
				cfg := scriptedSession{
					workType: "development", repository: "git@github.com:example/project.git", backstop: true, skipSteering: true,
					turns: []verdictScriptTurn{{
						text:  "Implementation complete.\nWORK_RESULT:passed",
						files: map[string]string{"feature.txt": "finished work\n"},
						during: func(t *testing.T, cwd string) {
							gitRun(t, cwd, "config", "user.name", "host-user")
							gitRun(t, cwd, "config", "github.user", "host-user")
						},
					}},
				}
				if declared {
					const primary = "https://github.com/example/project.git"
					const secondary = "https://github.com/another-org/docs.git"
					githubRepositoryFixture(t, primary)
					githubRepositoryFixture(t, secondary)
					cfg.declaration = &workarea.RepositoryDeclarationV1{
						Protocol: workarea.ProtocolSessionRootV1,
						Repositories: []workarea.DeclaredRepositoryV1{
							{Name: "primary", Role: workarea.RepositoryRolePrimary, Authority: workarea.RepositoryMutable, Source: workarea.RepositorySource{Repository: primary}},
							{Name: "docs", Role: workarea.RepositoryRoleSecondary, Authority: workarea.RepositoryMutable, Source: workarea.RepositorySource{Repository: secondary}},
						},
					}
				}
				res, _ := runScriptedSession(t, cfg)
				if res.BackstopReport == nil || !res.BackstopReport.Pushed {
					t.Fatalf("backstop did not push: %+v", res)
				}
				if res.WorkResult != "passed" {
					t.Fatalf("agent's successful work verdict changed: %q", res.WorkResult)
				}
				gotCalls := readFile(t, calls)
				if !strings.Contains(gotCalls, "create example/project\n") {
					t.Fatalf("missing configured repository: %s", gotCalls)
				}
				if declared && !strings.Contains(gotCalls, "create another-org/docs\n") {
					t.Fatalf("missing secondary repository: %s", gotCalls)
				}
				if failCreate {
					if !strings.Contains(res.BackstopReport.Diagnostics, "delivery failed") || !strings.Contains(res.BackstopReport.Diagnostics, "pull request permission denied") {
						t.Fatalf("missing delivery failure diagnostics: %+v", res.BackstopReport)
					}
					if res.Status != "failed" || res.FailureMode != FailureBackstop || res.PullRequestURL != "" {
						t.Fatalf("missing PR must still fail completion contract: %+v", res)
					}
					return
				}
				if res.Status != "completed" || !res.BackstopReport.PRCreated || res.PullRequestURL != "https://github.com/example/project/pull/17" {
					t.Fatalf("configured repository delivery failed: %+v", res)
				}
				if declared {
					for _, repository := range res.BackstopReport.Repositories {
						if repository.Report.PullRequest == nil {
							t.Errorf("missing PR projection for %s", repository.Name)
						}
					}
				}
			})
		}
	}
}
