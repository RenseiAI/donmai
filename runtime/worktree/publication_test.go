package worktree_test

import (
	"context"
	"encoding/pem"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"

	"github.com/RenseiAI/donmai/runtime/workarea"
	"github.com/RenseiAI/donmai/runtime/worktree"
)

func TestInteractivePublicationIgnoresAmbientURLRewrite(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git unavailable")
	}
	remote := publicationBareRemote(t)
	other := filepath.Join(t.TempDir(), "other.git")
	publicationGit(t, "", "clone", "--bare", "--quiet", remote, other)
	const sessionID = "11111111-1111-4111-8111-111111111111"
	manager, err := worktree.NewManager(worktree.Options{ParentDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	path, err := manager.Provision(t.Context(), worktree.ProvisionSpec{SessionID: sessionID, RepoURL: remote, Strategy: worktree.StrategyClone})
	if err != nil {
		t.Fatal(err)
	}
	branch := "agent/" + sessionID
	publicationGit(t, path, "checkout", "-b", branch)
	publicationGit(t, path, "push", "--quiet", other, "HEAD:refs/heads/"+branch)
	config := filepath.Join(t.TempDir(), "global.gitconfig")
	if err := os.WriteFile(config, []byte(fmt.Sprintf("[url %q]\n\tinsteadOf = %s\n", other, remote)), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GIT_CONFIG_GLOBAL", config)
	assessment := publicationAssessment(t, manager, sessionID, remote, branch)
	if !assessment.Retain || assessment.Reason != worktree.PublicationReasonUnpublished {
		t.Fatalf("rewrite redirected publication proof: %+v", assessment)
	}
}

func TestInteractivePublicationIgnoresCheckoutURLRewrite(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git unavailable")
	}
	remote := publicationBareRemote(t)
	other := filepath.Join(t.TempDir(), "other.git")
	publicationGit(t, "", "clone", "--bare", "--quiet", remote, other)
	const sessionID = "44444444-4444-4444-8444-444444444444"
	manager, err := worktree.NewManager(worktree.Options{ParentDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	path, err := manager.Provision(t.Context(), worktree.ProvisionSpec{SessionID: sessionID, RepoURL: remote, Strategy: worktree.StrategyClone})
	if err != nil {
		t.Fatal(err)
	}
	branch := "agent/" + sessionID
	publicationGit(t, path, "checkout", "-b", branch)
	publicationGit(t, path, "push", "--quiet", other, "HEAD:refs/heads/"+branch)
	publicationGit(t, path, "config", "url."+other+".insteadOf", remote)
	t.Chdir(path)
	assessment := publicationAssessment(t, manager, sessionID, remote, branch)
	if !assessment.Retain || assessment.Reason != worktree.PublicationReasonUnpublished {
		t.Fatalf("checkout config redirected publication proof: %+v", assessment)
	}
}

func TestInteractivePublicationInspectsOwnedWorktreeDespiteLocalConfig(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git unavailable")
	}
	remote := publicationBareRemote(t)
	const sessionID = "55555555-5555-4555-8555-555555555555"
	manager, err := worktree.NewManager(worktree.Options{ParentDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	path, err := manager.Provision(t.Context(), worktree.ProvisionSpec{SessionID: sessionID, RepoURL: remote, Strategy: worktree.StrategyClone})
	if err != nil {
		t.Fatal(err)
	}
	branch := "agent/" + sessionID
	publicationGit(t, path, "checkout", "-b", branch)
	publicationGit(t, path, "push", "--quiet", "origin", "HEAD:refs/heads/"+branch)
	clean := filepath.Join(t.TempDir(), "clean")
	publicationGit(t, "", "clone", "--quiet", remote, clean)
	if err := os.WriteFile(filepath.Join(path, "unpublished.txt"), []byte("unpublished\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	publicationGit(t, path, "config", "core.worktree", clean)
	assessment := publicationAssessment(t, manager, sessionID, remote, branch)
	if !assessment.Retain || assessment.Reason != worktree.PublicationReasonDirty {
		t.Fatalf("local core.worktree hid owned bytes: %+v", assessment)
	}
}

func TestInteractivePublicationRefusesAmbientGitExecutables(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git unavailable")
	}
	for _, tc := range []struct {
		name string
		arm  func(*testing.T, string) string
	}{
		{name: "unknown transport helper", arm: func(t *testing.T, canary string) string {
			bin := t.TempDir()
			writePublicationCanaryScript(t, filepath.Join(bin, "git-remote-fixture"), canary)
			t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
			return "fixture://example.invalid/repo"
		}},
		{name: "configured SSH command", arm: func(t *testing.T, canary string) string {
			script := filepath.Join(t.TempDir(), "ssh-command")
			writePublicationCanaryScript(t, script, canary)
			config := filepath.Join(t.TempDir(), "global.gitconfig")
			publicationGit(t, "", "config", "--file", config, "core.sshCommand", script)
			t.Setenv("GIT_CONFIG_GLOBAL", config)
			return "ssh://git@127.0.0.1:1/repo.git"
		}},
		{name: "inherited SSH command", arm: func(t *testing.T, canary string) string {
			script := filepath.Join(t.TempDir(), "ssh-command")
			writePublicationCanaryScript(t, script, canary)
			t.Setenv("GIT_SSH_COMMAND", script)
			return "ssh://git@127.0.0.1:1/repo.git"
		}},
		{name: "configured askpass", arm: func(t *testing.T, canary string) string {
			script := filepath.Join(t.TempDir(), "askpass")
			writePublicationCanaryScript(t, script, canary)
			t.Setenv("GIT_ASKPASS", script)
			t.Setenv("GIT_TERMINAL_PROMPT", "0")
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("WWW-Authenticate", `Basic realm="fixture"`)
				w.WriteHeader(http.StatusUnauthorized)
			}))
			t.Cleanup(server.Close)
			return server.URL + "/repo.git"
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			remote := publicationBareRemote(t)
			const sessionID = "22222222-2222-4222-8222-222222222222"
			manager, err := worktree.NewManager(worktree.Options{ParentDir: t.TempDir()})
			if err != nil {
				t.Fatal(err)
			}
			path, err := manager.Provision(t.Context(), worktree.ProvisionSpec{SessionID: sessionID, RepoURL: remote, Strategy: worktree.StrategyClone})
			if err != nil {
				t.Fatal(err)
			}
			branch := "agent/" + sessionID
			publicationGit(t, path, "checkout", "-b", branch)
			canary := filepath.Join(t.TempDir(), "executed")
			admitted := tc.arm(t, canary)
			assessment := publicationAssessment(t, manager, sessionID, admitted, branch)
			if !assessment.Retain || assessment.Reason != worktree.PublicationReasonUncertain {
				t.Fatalf("assessment=%+v", assessment)
			}
			if _, err := os.Stat(canary); !os.IsNotExist(err) {
				t.Fatalf("ambient command executed; canary stat err=%v", err)
			}
		})
	}
}

func TestInteractivePublicationUsesOnlyAdmittedHTTPSAuthorization(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git unavailable")
	}
	remote := publicationBareRemote(t)
	const sessionID = "33333333-3333-4333-8333-333333333333"
	manager, err := worktree.NewManager(worktree.Options{ParentDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	path, err := manager.Provision(t.Context(), worktree.ProvisionSpec{SessionID: sessionID, RepoURL: remote, Strategy: worktree.StrategyClone})
	if err != nil {
		t.Fatal(err)
	}
	branch := "agent/" + sessionID
	publicationGit(t, path, "checkout", "-b", branch)
	publicationGit(t, path, "push", "--quiet", "origin", "HEAD:refs/heads/"+branch)
	publicationGit(t, remote, "update-server-info")
	const auth = "Basic cHVibGljYXRpb246Zml4dHVyZQ=="
	var authorized atomic.Int32
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != auth {
			w.Header().Set("WWW-Authenticate", `Basic realm="fixture"`)
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		authorized.Add(1)
		http.StripPrefix("/repo.git/", http.FileServer(http.Dir(remote))).ServeHTTP(w, r)
	}))
	defer server.Close()
	caFile := filepath.Join(t.TempDir(), "ca.pem")
	if err := os.WriteFile(caFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw}), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GIT_SSL_CAINFO", caFile)
	admitted := server.URL + "/repo.git"
	const explicitSessionID = "66666666-6666-4666-8666-666666666666"
	explicitManager, err := worktree.NewManager(worktree.Options{
		ParentDir: t.TempDir(),
		GitAuth: func(_ context.Context, repository string) (string, bool, error) {
			if repository == admitted {
				return "Authorization: " + auth, true, nil
			}
			return "", false, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	explicitPath, err := explicitManager.Provision(t.Context(), worktree.ProvisionSpec{SessionID: explicitSessionID, RepoURL: remote, Strategy: worktree.StrategyClone})
	if err != nil {
		t.Fatal(err)
	}
	explicitBranch := "agent/" + explicitSessionID
	publicationGit(t, explicitPath, "checkout", "-b", explicitBranch)
	publicationGit(t, explicitPath, "push", "--quiet", "origin", "HEAD:refs/heads/"+explicitBranch)
	publicationGit(t, remote, "update-server-info")
	t.Setenv("GIT_CONFIG_COUNT", "2")
	t.Setenv("GIT_CONFIG_KEY_0", "url.file:///unexpected/.insteadOf")
	t.Setenv("GIT_CONFIG_VALUE_0", admitted)
	t.Setenv("GIT_CONFIG_KEY_1", "http."+admitted+".extraHeader")
	t.Setenv("GIT_CONFIG_VALUE_1", "Authorization: "+auth)
	assessment := publicationAssessment(t, manager, sessionID, admitted, branch)
	if assessment.Retain || assessment.Reason != worktree.PublicationReasonPublished {
		t.Fatalf("authenticated HTTPS publication not proved: %+v", assessment)
	}
	if authorized.Load() == 0 {
		t.Fatal("Git did not send the admitted URL authorization to the TLS fixture")
	}
	t.Setenv("GIT_CONFIG_VALUE_1", "Authorization: Basic d3Jvbmc6d3Jvbmc=")
	previous := authorized.Load()
	explicitAssessment := publicationAssessment(t, explicitManager, explicitSessionID, admitted, explicitBranch)
	if explicitAssessment.Retain || explicitAssessment.Reason != worktree.PublicationReasonPublished || authorized.Load() <= previous {
		t.Fatalf("explicit GitAuth did not take precedence: assessment=%+v authorized=%d before=%d", explicitAssessment, authorized.Load(), previous)
	}
}

func publicationAssessment(t *testing.T, manager *worktree.Manager, sessionID, remote, branch string) worktree.PublicationAssessment {
	t.Helper()
	_, assessment, err := manager.AcquireInteractiveTerminalLease(t.Context(),
		worktree.InteractivePublicationSpec{SessionID: sessionID, Repositories: []worktree.PublicationTarget{{Name: "remote", Repository: remote, Branch: branch}}},
		workarea.AcquireSpec{SessionID: sessionID, TerminalResultID: "tr_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", Policy: workarea.DefaultLeasePolicy(), ReleaseRequested: true},
		false)
	if err != nil {
		t.Fatal(err)
	}
	return assessment
}

func writePublicationCanaryScript(t *testing.T, path, canary string) {
	t.Helper()
	content := "#!/bin/sh\nprintf touched > " + fmt.Sprintf("%q", canary) + "\nexit 1\n"
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Chmod(path, 0o700); err != nil {
		t.Fatal(err)
	}
}

func TestInteractivePublicationInspectionSerializesLeaseAheadOfTeardown(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git unavailable")
	}
	remote := publicationBareRemote(t)
	refsStarted := make(chan struct{})
	allowRefs := make(chan struct{})
	var once sync.Once
	var observedStatusArgs []string
	var observedRefArgs []string
	runner := func(ctx context.Context, name string, args ...string) ([]byte, error) {
		for _, arg := range args {
			if arg == "status" {
				observedStatusArgs = append([]string(nil), args...)
			}
			if arg == "for-each-ref" {
				once.Do(func() {
					observedRefArgs = append([]string(nil), args...)
					close(refsStarted)
				})
				select {
				case <-allowRefs:
				case <-ctx.Done():
					return nil, ctx.Err()
				}
				break
			}
		}
		cmd := exec.CommandContext(ctx, name, args...) //nolint:gosec // fixed git binary and temp inputs
		cmd.Env = append(os.Environ(), "GIT_CONFIG_NOSYSTEM=1", "GIT_TERMINAL_PROMPT=0")
		return cmd.CombinedOutput()
	}
	parent := t.TempDir()
	manager, err := worktree.NewManager(worktree.Options{ParentDir: parent, CommandRunner: runner})
	if err != nil {
		t.Fatal(err)
	}
	const sessionID = "77777777-7777-4777-8777-777777777777"
	path, err := manager.Provision(t.Context(), worktree.ProvisionSpec{
		SessionID: sessionID,
		RepoURL:   remote,
		Strategy:  worktree.StrategyClone,
	})
	if err != nil {
		t.Fatal(err)
	}
	branch := "agent/" + sessionID
	publicationGit(t, path, "checkout", "-b", branch)
	if err := os.WriteFile(filepath.Join(path, "local-only.txt"), []byte("retained\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	publicationGit(t, path, "-c", "user.name=test", "-c", "user.email=test@example.invalid", "stash", "push", "--quiet", "--include-untracked", "-m", "local-only")

	type leaseResult struct {
		lease      *workarea.TerminalLease
		assessment worktree.PublicationAssessment
		err        error
	}
	leaseDone := make(chan leaseResult, 1)
	go func() {
		lease, assessment, err := manager.AcquireInteractiveTerminalLease(
			context.Background(),
			worktree.InteractivePublicationSpec{
				SessionID: sessionID,
				Repositories: []worktree.PublicationTarget{{
					Name: "remote", Repository: remote, Branch: branch, AllowRemoteHEAD: true,
				}},
			},
			workarea.AcquireSpec{
				SessionID: sessionID, TerminalResultID: "tr_77777777777777777777777777777777",
				Policy: workarea.DefaultLeasePolicy(), ReleaseRequested: true,
			},
			false,
		)
		leaseDone <- leaseResult{lease: lease, assessment: assessment, err: err}
	}()
	<-refsStarted
	teardownDone := make(chan error, 1)
	go func() { teardownDone <- manager.Teardown(context.Background(), sessionID) }()
	close(allowRefs)

	retained := <-leaseDone
	if retained.err != nil || retained.lease == nil || !retained.assessment.Retain || retained.assessment.Reason != worktree.PublicationReasonUnpublished {
		t.Fatalf("lease result=%+v", retained)
	}
	if err := <-teardownDone; err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("teardown crossed committed retention lease: %v", err)
	}
	if retained.lease.ReleaseDisposition != "archive" {
		t.Fatalf("release disposition=%q", retained.lease.ReleaseDisposition)
	}
	statusCommand := strings.Join(observedStatusArgs, " ")
	for _, required := range []string{"core.hooksPath=/dev/null", "core.fsmonitor=false", "protocol.allow=never", "protocol.https.allow=always", "--untracked-files=all"} {
		if !strings.Contains(statusCommand, required) {
			t.Errorf("publication status command omitted %q: %s", required, statusCommand)
		}
	}
	if refCommand := strings.Join(observedRefArgs, " "); !strings.Contains(refCommand, "for-each-ref") || !strings.Contains(refCommand, "%(objectname)%09%(refname)%09%(symref)") {
		t.Fatalf("publication local-ref command was not the bounded literal inventory: %s", refCommand)
	}
}

func TestInteractivePublicationRefusesAdmittedRemoteHelperWithoutExecutingIt(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git unavailable")
	}
	remote := publicationBareRemote(t)
	var unsafeCalls int
	runner := func(ctx context.Context, name string, args ...string) ([]byte, error) {
		if strings.Contains(strings.Join(args, " "), "ext::") {
			unsafeCalls++
		}
		cmd := exec.CommandContext(ctx, name, args...) //nolint:gosec // fixed git binary and temp inputs
		cmd.Env = append(os.Environ(), "GIT_CONFIG_NOSYSTEM=1", "GIT_TERMINAL_PROMPT=0")
		return cmd.CombinedOutput()
	}
	manager, err := worktree.NewManager(worktree.Options{ParentDir: t.TempDir(), CommandRunner: runner})
	if err != nil {
		t.Fatal(err)
	}
	const sessionID = "88888888-8888-4888-8888-888888888888"
	path, err := manager.Provision(t.Context(), worktree.ProvisionSpec{SessionID: sessionID, RepoURL: remote, Strategy: worktree.StrategyClone})
	if err != nil {
		t.Fatal(err)
	}
	branch := "agent/" + sessionID
	publicationGit(t, path, "checkout", "-b", branch)
	lease, assessment, err := manager.AcquireInteractiveTerminalLease(
		t.Context(),
		worktree.InteractivePublicationSpec{
			SessionID: sessionID,
			Repositories: []worktree.PublicationTarget{{
				Name: "remote", Repository: "ext::sh -c 'exit 0'", Branch: branch,
			}},
		},
		workarea.AcquireSpec{
			SessionID: sessionID, TerminalResultID: "tr_88888888888888888888888888888888",
			Policy: workarea.DefaultLeasePolicy(), ReleaseRequested: true,
		},
		false,
	)
	if err != nil || lease == nil || !assessment.Retain || assessment.Reason != worktree.PublicationReasonUncertain {
		t.Fatalf("lease=%+v assessment=%+v err=%v", lease, assessment, err)
	}
	if unsafeCalls != 0 {
		t.Fatalf("unsafe remote helper reached git %d time(s)", unsafeCalls)
	}
}

func publicationBareRemote(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	remote := filepath.Join(root, "remote.git")
	seed := filepath.Join(root, "seed")
	publicationGit(t, "", "init", "--bare", "--quiet", remote)
	publicationGit(t, "", "init", "--quiet", "-b", "main", seed)
	if err := os.WriteFile(filepath.Join(seed, "tracked.txt"), []byte("published\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	publicationGit(t, seed, "-c", "user.name=test", "-c", "user.email=test@example.invalid", "add", "tracked.txt")
	publicationGit(t, seed, "-c", "user.name=test", "-c", "user.email=test@example.invalid", "commit", "--quiet", "-m", "initial")
	publicationGit(t, seed, "remote", "add", "origin", remote)
	publicationGit(t, seed, "push", "--quiet", "-u", "origin", "main")
	publicationGit(t, remote, "symbolic-ref", "HEAD", "refs/heads/main")
	return remote
}

func publicationGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	if dir != "" {
		args = append([]string{"-C", dir}, args...)
	}
	cmd := exec.Command("git", args...) //nolint:gosec // fixed test binary and temp inputs
	cmd.Env = append(os.Environ(), "GIT_CONFIG_NOSYSTEM=1", "GIT_TERMINAL_PROMPT=0")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
}
