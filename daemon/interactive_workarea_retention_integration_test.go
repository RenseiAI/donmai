package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/RenseiAI/donmai/afclient"
	"github.com/RenseiAI/donmai/agent"
	"github.com/RenseiAI/donmai/prompt"
	providershell "github.com/RenseiAI/donmai/provider/harness/shell"
	"github.com/RenseiAI/donmai/result"
	"github.com/RenseiAI/donmai/runner"
	"github.com/RenseiAI/donmai/runtime/worktree"
)

const interactiveRetentionSessionID = "11111111-1111-4111-8111-111111111111"

func TestCompletedInteractiveDirtyWorkareaArchivesThroughRestartAndCleanPublishedRemoves(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git unavailable")
	}

	t.Run("dirty tracked and untracked source survives and restores", func(t *testing.T) {
		remote := interactiveRetentionBareRemote(t)
		parent := t.TempDir()
		archiveRoot := filepath.Join(t.TempDir(), "archives")
		registry := NewWorkareaArchiveRegistry(WorkareaArchiveOptions{Root: archiveRoot})
		current := time.Date(2026, 9, 13, 6, 30, 0, 0, time.UTC)
		manager := interactiveRetentionManager(t, parent, registry, func() time.Time { return current })

		tracked := []byte("retained tracked bytes\n")
		untracked := []byte("retained untracked postgres bytes\n")
		script := interactiveRetentionShell(t, fmt.Sprintf(
			"printf %%s %s > tracked.txt\nprintf %%s %s > dispatch-workflow-site.pg.test.ts\nrm deleted.txt\n",
			shellQuote(string(tracked)), shellQuote(string(untracked)),
		))
		t.Setenv("SHELL", script)

		terminalBody := interactiveRetentionPlatform(t)
		run := interactiveRetentionRunner(t, manager, terminalBody.server)
		res, err := run.Run(t.Context(), interactiveRetentionWork(remote, interactiveRetentionSessionID, terminalBody.server.URL))
		if err != nil || res.Status != "completed" {
			t.Fatalf("Run result=%+v err=%v", res, err)
		}
		if res.TerminalWorkareaLease == nil {
			t.Fatal("completed dirty interactive result omitted durable retention lease")
		}
		if _, err := os.Stat(res.WorktreePath); err != nil {
			t.Fatalf("dirty source root was removed before archive disposition: %v", err)
		}
		assertInteractiveRetentionBytes(t, res.WorktreePath, tracked, untracked)
		assertInteractiveRetentionDeletion(t, res.WorktreePath)

		lease, err := manager.TerminalLease(res.TerminalWorkareaLease.LeaseID)
		if err != nil {
			t.Fatal(err)
		}
		if lease.ReleaseDisposition != "archive" || lease.ReleaseMetadata["retentionReason"] != "interactive-unpublished" {
			t.Fatalf("lease disposition/metadata = %q/%v", lease.ReleaseDisposition, lease.ReleaseMetadata)
		}
		terminalBody.assertFourFieldLease(t)

		current = lease.ExpiresAt.Add(time.Millisecond)
		recovered := interactiveRetentionManager(t, parent, registry, func() time.Time { return current })
		considered, err := recovered.ReapExpiredTerminalLeases(t.Context(), 1, time.Second)
		if err != nil || considered != 1 {
			t.Fatalf("restart reaper considered=%d err=%v", considered, err)
		}
		if _, err := os.Stat(res.WorktreePath); !os.IsNotExist(err) {
			t.Fatalf("archived source generation still exists: %v", err)
		}
		manifest, err := registry.readManifest(lease.WorkareaID)
		if err != nil || manifest.TreeDigest == "" || manifest.ID != lease.WorkareaID || manifest.SessionID != interactiveRetentionSessionID || manifest.SourceProvider != "local" || manifest.Disposition != "archive" {
			t.Fatalf("archive manifest=%+v err=%v", manifest, err)
		}
		if info, err := os.Stat(archiveRoot); err != nil || info.Mode().Perm() != 0o700 {
			t.Fatalf("local archive root mode=%v err=%v", info, err)
		}
		if considered, err := recovered.ReapExpiredTerminalLeases(t.Context(), 1, time.Second); err != nil || considered != 0 {
			t.Fatalf("released lease reaped twice: considered=%d err=%v", considered, err)
		}
		_, archived, err := registry.List()
		if err != nil || len(archived) != 1 || archived[0].ID != lease.WorkareaID {
			t.Fatalf("archive inventory=%+v err=%v", archived, err)
		}
		restored, _, err := registry.RestoreV1(lease.WorkareaID, afclient.WorkareaRestoreRequest{
			IntoSessionID: "22222222-2222-4222-8222-222222222222",
			Reason:        "loss-prevention-control",
		})
		if err != nil {
			t.Fatal(err)
		}
		assertInteractiveRetentionBytes(t, restored.Path, tracked, untracked)
		assertInteractiveRetentionDeletion(t, restored.Path)
	})

	t.Run("clean current head with local stash retains", func(t *testing.T) {
		remote := interactiveRetentionBareRemote(t)
		parent := t.TempDir()
		registry := NewWorkareaArchiveRegistry(WorkareaArchiveOptions{Root: filepath.Join(t.TempDir(), "archives")})
		current := time.Date(2026, 9, 13, 6, 30, 0, 0, time.UTC)
		manager := interactiveRetentionManager(t, parent, registry, func() time.Time { return current })
		script := interactiveRetentionShell(t, strings.Join([]string{
			"printf 'stashed-only bytes\\n' > tracked.txt",
			"git -c user.name=test -c user.email=test@example.invalid stash push --quiet -m local-unpublished",
			"test -n \"$(git stash list)\"",
		}, "\n")+"\n")
		t.Setenv("SHELL", script)
		platform := interactiveRetentionPlatform(t)
		res, err := interactiveRetentionRunner(t, manager, platform.server).Run(
			t.Context(),
			interactiveRetentionWork(remote, "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb", platform.server.URL),
		)
		assertInteractiveRetentionLease(t, manager, res, err, worktree.PublicationReasonUnpublished)
		lease, err := manager.TerminalLease(res.TerminalWorkareaLease.LeaseID)
		if err != nil {
			t.Fatal(err)
		}
		current = lease.ExpiresAt.Add(time.Millisecond)
		recovered := interactiveRetentionManager(t, parent, registry, func() time.Time { return current })
		if considered, err := recovered.ReapExpiredTerminalLeases(t.Context(), 1, time.Second); err != nil || considered != 1 {
			t.Fatalf("stash restart reaper considered=%d err=%v", considered, err)
		}
		restored, _, err := registry.RestoreV1(lease.WorkareaID, afclient.WorkareaRestoreRequest{
			IntoSessionID: "eeeeeeee-eeee-4eee-8eee-eeeeeeeeeeee",
			Reason:        "stash-loss-prevention-control",
		})
		if err != nil {
			t.Fatal(err)
		}
		stash := interactiveRetentionGitOutput(t, restored.Path, "stash", "show", "--format=fuller", "--patch", "refs/stash")
		if !strings.Contains(stash, "stashed-only bytes") {
			t.Fatalf("restored archive lost local stash:\n%s", stash)
		}
	})

	t.Run("clean current head with another local branch retains", func(t *testing.T) {
		remote := interactiveRetentionBareRemote(t)
		manager, err := worktree.NewManager(worktree.Options{ParentDir: t.TempDir()})
		if err != nil {
			t.Fatal(err)
		}
		script := interactiveRetentionShell(t, strings.Join([]string{
			"current=$(git symbolic-ref --short HEAD)",
			"git checkout --quiet -b local-only",
			"printf 'local-branch-only bytes\\n' > tracked.txt",
			"git add tracked.txt",
			"git -c user.name=test -c user.email=test@example.invalid commit --quiet -m local-only",
			"git checkout --quiet \"$current\"",
		}, "\n")+"\n")
		t.Setenv("SHELL", script)
		platform := interactiveRetentionPlatform(t)
		res, err := interactiveRetentionRunner(t, manager, platform.server).Run(
			t.Context(),
			interactiveRetentionWork(remote, "cccccccc-cccc-4ccc-8ccc-cccccccccccc", platform.server.URL),
		)
		assertInteractiveRetentionLease(t, manager, res, err, worktree.PublicationReasonUnpublished)
	})

	t.Run("ignored source bytes retain", func(t *testing.T) {
		remote := interactiveRetentionBareRemote(t)
		manager, err := worktree.NewManager(worktree.Options{ParentDir: t.TempDir()})
		if err != nil {
			t.Fatal(err)
		}
		t.Setenv("SHELL", interactiveRetentionShell(t,
			"printf 'ignored-only.txt\\n' >> .git/info/exclude\n"+
				"printf 'ignored-only bytes\\n' > ignored-only.txt\n"+
				"test -f ignored-only.txt\n"))
		platform := interactiveRetentionPlatform(t)
		res, err := interactiveRetentionRunner(t, manager, platform.server).Run(
			t.Context(),
			interactiveRetentionWork(remote, "dddddddd-dddd-4ddd-8ddd-dddddddddddd", platform.server.URL),
		)
		assertInteractiveRetentionLease(t, manager, res, err, worktree.PublicationReasonDirty)
	})

	t.Run("ignored regular file named like harness state retains", func(t *testing.T) {
		remote := interactiveRetentionBareRemote(t)
		manager, err := worktree.NewManager(worktree.Options{ParentDir: t.TempDir()})
		if err != nil {
			t.Fatal(err)
		}
		t.Setenv("SHELL", interactiveRetentionShell(t,
			"test ! -e .pi\n"+
				"printf '.pi\\n' >> .git/info/exclude\n"+
				"printf 'same-name regular source bytes\\n' > .pi\n"+
				"test -f .pi\n"))
		platform := interactiveRetentionPlatform(t)
		res, err := interactiveRetentionRunner(t, manager, platform.server).Run(
			t.Context(),
			interactiveRetentionWork(remote, "ffffffff-ffff-4fff-8fff-ffffffffffff", platform.server.URL),
		)
		assertInteractiveRetentionLease(t, manager, res, err, worktree.PublicationReasonDirty)
	})

	t.Run("clean exact published source keeps ordinary teardown", func(t *testing.T) {
		remote := interactiveRetentionBareRemote(t)
		manager, err := worktree.NewManager(worktree.Options{ParentDir: t.TempDir()})
		if err != nil {
			t.Fatal(err)
		}
		t.Setenv("SHELL", interactiveRetentionShell(t, ""))
		platform := interactiveRetentionPlatform(t)
		res, err := interactiveRetentionRunner(t, manager, platform.server).Run(
			t.Context(),
			interactiveRetentionWork(remote, "33333333-3333-4333-8333-333333333333", platform.server.URL),
		)
		if err != nil || res.Status != "completed" {
			t.Fatalf("Run result=%+v err=%v", res, err)
		}
		if res.TerminalWorkareaLease != nil {
			t.Fatalf("clean published workarea unexpectedly retained: %+v", res.TerminalWorkareaLease)
		}
		if _, err := os.Stat(res.WorktreePath); !os.IsNotExist(err) {
			t.Fatalf("clean published workarea survived ordinary teardown: %v", err)
		}
	})

	t.Run("clean unpublished commit retains", func(t *testing.T) {
		remote := interactiveRetentionBareRemote(t)
		manager, err := worktree.NewManager(worktree.Options{ParentDir: t.TempDir()})
		if err != nil {
			t.Fatal(err)
		}
		script := interactiveRetentionShell(t, strings.Join([]string{
			"printf 'local commit bytes\\n' > tracked.txt",
			"git add tracked.txt",
			"git -c user.name=test -c user.email=test@example.invalid commit --quiet -m local",
		}, "\n")+"\n")
		t.Setenv("SHELL", script)
		platform := interactiveRetentionPlatform(t)
		res, err := interactiveRetentionRunner(t, manager, platform.server).Run(
			t.Context(),
			interactiveRetentionWork(remote, "44444444-4444-4444-8444-444444444444", platform.server.URL),
		)
		if err != nil || res.Status != "completed" || res.TerminalWorkareaLease == nil {
			t.Fatalf("Run result=%+v err=%v", res, err)
		}
		lease, err := manager.TerminalLease(res.TerminalWorkareaLease.LeaseID)
		if err != nil || lease.ReleaseMetadata["publicationAssessment"] != "unpublished" {
			t.Fatalf("lease=%+v err=%v", lease, err)
		}
		if _, err := os.Stat(res.WorktreePath); err != nil {
			t.Fatalf("unpublished commit workarea was removed: %v", err)
		}
	})

	t.Run("clean exact pushed commit keeps ordinary teardown", func(t *testing.T) {
		remote := interactiveRetentionBareRemote(t)
		manager, err := worktree.NewManager(worktree.Options{ParentDir: t.TempDir()})
		if err != nil {
			t.Fatal(err)
		}
		const sessionID = "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"
		branch := "agent/" + sessionID
		script := interactiveRetentionShell(t, strings.Join([]string{
			"printf 'published interactive bytes\\n' > tracked.txt",
			"git add tracked.txt",
			"git -c user.name=test -c user.email=test@example.invalid commit --quiet -m published",
			"git push --quiet origin HEAD:refs/heads/" + branch,
		}, "\n")+"\n")
		t.Setenv("SHELL", script)
		platform := interactiveRetentionPlatform(t)
		res, err := interactiveRetentionRunner(t, manager, platform.server).Run(
			t.Context(),
			interactiveRetentionWork(remote, sessionID, platform.server.URL),
		)
		if err != nil || res.Status != "completed" {
			t.Fatalf("Run result=%+v err=%v", res, err)
		}
		if res.TerminalWorkareaLease != nil {
			t.Fatalf("exact pushed commit unexpectedly retained: %+v", res.TerminalWorkareaLease)
		}
		if _, err := os.Stat(res.WorktreePath); !os.IsNotExist(err) {
			t.Fatalf("exact pushed workarea survived ordinary teardown: %v", err)
		}
		out := interactiveRetentionGitOutput(t, "", "ls-remote", "--exit-code", "--heads", remote, "refs/heads/"+branch)
		if !strings.Contains(out, "refs/heads/"+branch) {
			t.Fatalf("published branch missing: %s", out)
		}
	})

	t.Run("remote uncertainty retains", func(t *testing.T) {
		remote := interactiveRetentionBareRemote(t)
		manager, err := worktree.NewManager(worktree.Options{ParentDir: t.TempDir()})
		if err != nil {
			t.Fatal(err)
		}
		script := interactiveRetentionShell(t, fmt.Sprintf("mv %s %s\n", shellQuote(remote), shellQuote(remote+".offline")))
		t.Setenv("SHELL", script)
		platform := interactiveRetentionPlatform(t)
		res, err := interactiveRetentionRunner(t, manager, platform.server).Run(
			t.Context(),
			interactiveRetentionWork(remote, "55555555-5555-4555-8555-555555555555", platform.server.URL),
		)
		if err != nil || res.Status != "completed" || res.TerminalWorkareaLease == nil {
			t.Fatalf("Run result=%+v err=%v", res, err)
		}
		lease, err := manager.TerminalLease(res.TerminalWorkareaLease.LeaseID)
		if err != nil || lease.ReleaseMetadata["publicationAssessment"] != "uncertain" {
			t.Fatalf("lease=%+v err=%v", lease, err)
		}
		if _, err := os.Stat(res.WorktreePath); err != nil {
			t.Fatalf("remote-uncertain workarea was removed: %v", err)
		}
	})

	t.Run("archive failure after restart retains the only source", func(t *testing.T) {
		remote := interactiveRetentionBareRemote(t)
		parent := t.TempDir()
		registry := NewWorkareaArchiveRegistry(WorkareaArchiveOptions{
			Root: filepath.Join(t.TempDir(), "archives"),
			ArchiveHook: func(stage string) error {
				if stage == "after-authorize" {
					return fmt.Errorf("archive unavailable")
				}
				return nil
			},
		})
		current := time.Date(2026, 9, 13, 6, 30, 0, 0, time.UTC)
		manager := interactiveRetentionManager(t, parent, registry, func() time.Time { return current })
		t.Setenv("SHELL", interactiveRetentionShell(t, "printf 'must survive\\n' > tracked.txt\n"))
		platform := interactiveRetentionPlatform(t)
		res, err := interactiveRetentionRunner(t, manager, platform.server).Run(
			t.Context(),
			interactiveRetentionWork(remote, "66666666-6666-4666-8666-666666666666", platform.server.URL),
		)
		if err != nil || res.TerminalWorkareaLease == nil {
			t.Fatalf("Run result=%+v err=%v", res, err)
		}
		lease, err := manager.TerminalLease(res.TerminalWorkareaLease.LeaseID)
		if err != nil {
			t.Fatal(err)
		}
		current = lease.ExpiresAt.Add(time.Millisecond)
		recovered := interactiveRetentionManager(t, parent, registry, func() time.Time { return current })
		if _, err := recovered.ReapExpiredTerminalLeases(t.Context(), 1, time.Second); err == nil || !strings.Contains(err.Error(), "archive unavailable") {
			t.Fatalf("archive failure = %v", err)
		}
		if _, err := os.Stat(res.WorktreePath); err != nil {
			t.Fatalf("archive failure removed the only source: %v", err)
		}
		retained, err := recovered.TerminalLease(lease.LeaseID)
		if err != nil || retained.State != "release-pending" {
			t.Fatalf("retained lease=%+v err=%v", retained, err)
		}
	})
}

type interactiveRetentionRecorder struct {
	server *httptest.Server
	body   []byte
}

func interactiveRetentionPlatform(t *testing.T) *interactiveRetentionRecorder {
	t.Helper()
	recorder := &interactiveRetentionRecorder{}
	recorder.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if strings.HasSuffix(req.URL.Path, "/status") {
			body, err := io.ReadAll(req.Body)
			if err != nil {
				t.Error(err)
			} else {
				var envelope map[string]any
				if json.Unmarshal(body, &envelope) == nil && envelope["status"] == "completed" {
					recorder.body = append([]byte(nil), body...)
				}
			}
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"refreshed":true,"ok":true}`))
	}))
	t.Cleanup(recorder.server.Close)
	return recorder
}

func (r *interactiveRetentionRecorder) assertFourFieldLease(t *testing.T) {
	t.Helper()
	var envelope map[string]any
	if err := json.Unmarshal(r.body, &envelope); err != nil {
		t.Fatal(err)
	}
	projection, ok := envelope["terminalWorkareaLease"].(map[string]any)
	if !ok || len(projection) != 4 || projection["leaseId"] == nil || projection["workareaId"] == nil || projection["terminalResultId"] == nil || projection["expiresAt"] == nil {
		t.Fatalf("terminal lease projection changed: %#v", envelope["terminalWorkareaLease"])
	}
}

func interactiveRetentionManager(
	t *testing.T,
	parent string,
	registry *WorkareaArchiveRegistry,
	now func() time.Time,
) *worktree.Manager {
	t.Helper()
	manager, err := worktree.NewManager(worktree.Options{
		ParentDir: parent,
		Now:       now,
		ArchiveRoot: func(ctx context.Context, spec worktree.ArchiveRootSpec) error {
			return registry.ArchiveRoot(ctx, WorkareaRootArchiveSpec{
				AcquisitionID: spec.AcquisitionID,
				WorkareaID:    spec.WorkareaID,
				SessionID:     spec.SessionID,
				WorkareaRoot:  spec.WorkareaRoot,
				SelectedPath:  spec.SelectedPath,
			})
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	return manager
}

func interactiveRetentionRunner(t *testing.T, manager *worktree.Manager, server *httptest.Server) *runner.Runner {
	t.Helper()
	registry := runner.NewRegistry()
	provider, err := providershell.New()
	if err != nil {
		t.Fatal(err)
	}
	if err := registry.Register(provider); err != nil {
		t.Fatal(err)
	}
	poster, err := result.NewPoster(result.Options{
		PlatformURL: server.URL,
		WorkerID:    "worker",
		HTTPClient:  server.Client(),
		BaseDelay:   time.Nanosecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	run, err := runner.New(runner.Options{
		Registry:           registry,
		WorktreeManager:    manager,
		Poster:             poster,
		HTTPClient:         server.Client(),
		MaxSessionDuration: -1,
		SkipBackstop:       true,
		SkipSteering:       true,
		SkipPostSession:    true,
	})
	if err != nil {
		t.Fatal(err)
	}
	return run
}

func interactiveRetentionWork(repository, sessionID, platformURL string) runner.QueuedWork {
	return runner.QueuedWork{
		QueuedWork: prompt.QueuedWork{
			SessionID:       sessionID,
			IssueID:         "interactive-retention-issue",
			IssueIdentifier: "WORK-RETENTION",
			WorkType:        "development",
			Body:            "preserve unpublished interactive source",
			Mode:            prompt.InteractiveRunMode,
			InitialPrompt:   "start",
			Repository:      repository,
		},
		WorkerID:        "worker",
		PlatformURL:     platformURL,
		ResolvedProfile: runner.ResolvedProfile{Provider: agent.ProviderShell},
	}
}

func interactiveRetentionBareRemote(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	remote := filepath.Join(root, "remote.git")
	seed := filepath.Join(root, "seed")
	interactiveRetentionGit(t, "", "init", "--bare", "--quiet", remote)
	interactiveRetentionGit(t, "", "init", "--quiet", "-b", "main", seed)
	if err := os.WriteFile(filepath.Join(seed, "tracked.txt"), []byte("published bytes\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(seed, "deleted.txt"), []byte("published deletion target\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	interactiveRetentionGit(t, seed, "-c", "user.name=test", "-c", "user.email=test@example.invalid", "add", "tracked.txt", "deleted.txt")
	interactiveRetentionGit(t, seed, "-c", "user.name=test", "-c", "user.email=test@example.invalid", "commit", "--quiet", "-m", "initial")
	interactiveRetentionGit(t, seed, "remote", "add", "origin", remote)
	interactiveRetentionGit(t, seed, "push", "--quiet", "-u", "origin", "main")
	interactiveRetentionGit(t, remote, "symbolic-ref", "HEAD", "refs/heads/main")
	return remote
}

func interactiveRetentionShell(t *testing.T, afterRead string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "interactive-shell")
	body := "#!/bin/sh\nIFS= read -r _\n" + afterRead + "exit 0\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o700); err != nil { //nolint:gosec // test fixture must execute
		t.Fatal(err)
	}
	return path
}

func interactiveRetentionGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	_ = interactiveRetentionGitOutput(t, dir, args...)
}

func interactiveRetentionGitOutput(t *testing.T, dir string, args ...string) string {
	t.Helper()
	if dir != "" {
		args = append([]string{"-C", dir}, args...)
	}
	cmd := exec.Command("git", args...) //nolint:gosec // fixed test binary and temp-dir arguments
	cmd.Env = append(os.Environ(), "GIT_CONFIG_NOSYSTEM=1", "GIT_TERMINAL_PROMPT=0")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	} else {
		return string(out)
	}
	return ""
}

func assertInteractiveRetentionBytes(t *testing.T, root string, tracked, untracked []byte) {
	t.Helper()
	for name, want := range map[string][]byte{
		"tracked.txt":                       tracked,
		"dispatch-workflow-site.pg.test.ts": untracked,
	} {
		got, err := os.ReadFile(filepath.Join(root, name))
		if err != nil || string(got) != string(want) {
			t.Fatalf("%s bytes=%q err=%v, want %q", name, got, err, want)
		}
	}
}

func assertInteractiveRetentionDeletion(t *testing.T, root string) {
	t.Helper()
	if _, err := os.Stat(filepath.Join(root, "deleted.txt")); !os.IsNotExist(err) {
		t.Fatalf("deleted tracked file was restored: %v", err)
	}
}

func assertInteractiveRetentionLease(
	t *testing.T,
	manager *worktree.Manager,
	res *runner.Result,
	runErr error,
	wantReason string,
) {
	t.Helper()
	if runErr != nil || res.Status != "completed" || res.TerminalWorkareaLease == nil {
		t.Fatalf("Run result=%+v err=%v", res, runErr)
	}
	lease, err := manager.TerminalLease(res.TerminalWorkareaLease.LeaseID)
	if err != nil || lease.ReleaseDisposition != "archive" || lease.ReleaseMetadata["publicationAssessment"] != wantReason {
		t.Fatalf("retention lease=%+v err=%v, want archive/%s", lease, err, wantReason)
	}
	if _, err := os.Stat(res.WorktreePath); err != nil {
		t.Fatalf("retained source root was removed: %v", err)
	}
}

func shellQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\\''") + "'"
}
