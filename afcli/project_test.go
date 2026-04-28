package afcli

import (
	"bytes"
	"errors"
	"strings"
	"testing"

	"github.com/RenseiAI/agentfactory-tui/afclient"
)

// ── mock configReaderWriter ───────────────────────────────────────────────────

// mockConfigRW is an in-memory configReaderWriter for tests.
// A non-nil readErr causes ReadConfig to fail.
// A non-nil writeErr causes WriteConfig to fail.
type mockConfigRW struct {
	cfg      *afclient.DaemonYAML
	readErr  error
	writeErr error
	written  *afclient.DaemonYAML
}

func (m *mockConfigRW) ReadConfig() (*afclient.DaemonYAML, error) {
	if m.readErr != nil {
		return nil, m.readErr
	}
	if m.cfg == nil {
		return &afclient.DaemonYAML{}, nil
	}
	// Return a shallow copy so mutations don't bleed between Read calls.
	cp := *m.cfg
	cp.Projects = make([]afclient.ProjectEntry, len(m.cfg.Projects))
	copy(cp.Projects, m.cfg.Projects)
	return &cp, nil
}

func (m *mockConfigRW) WriteConfig(cfg *afclient.DaemonYAML) error {
	if m.writeErr != nil {
		return m.writeErr
	}
	m.written = cfg
	return nil
}

// newTestProjectCmd builds the project command tree with the given mock RW and
// simulated stdin, then executes it with args.  Returns stdout+stderr and any
// execution error.
func newTestProjectCmd(rw configReaderWriter, stdin string, args []string) (*bytes.Buffer, error) {
	cmd := newProjectCmdWithRW(rw)
	buf := &bytes.Buffer{}
	cmd.SetOut(buf)
	cmd.SetErr(buf)
	cmd.SetIn(strings.NewReader(stdin))
	cmd.SetArgs(args)
	err := cmd.Execute()
	return buf, err
}

// ── allow ─────────────────────────────────────────────────────────────────────

func TestProjectAllowNoCredentials(t *testing.T) {
	t.Parallel()

	rw := &mockConfigRW{}
	_, err := newTestProjectCmd(rw, "", []string{
		"allow", "github.com/foo/bar", "--no-credentials",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if rw.written == nil {
		t.Fatal("expected config to be written")
	}
	if len(rw.written.Projects) != 1 {
		t.Fatalf("expected 1 project, got %d", len(rw.written.Projects))
	}
	p := rw.written.Projects[0]
	if p.RepoURL != "github.com/foo/bar" {
		t.Errorf("expected repoURL github.com/foo/bar, got %q", p.RepoURL)
	}
	if p.CredentialHelper != nil {
		t.Errorf("expected nil credential helper, got %+v", p.CredentialHelper)
	}
	if p.CloneStrategy != afclient.CloneShallow {
		t.Errorf("expected shallow clone, got %q", p.CloneStrategy)
	}
}

func TestProjectAllowNonInteractiveNoCredentials(t *testing.T) {
	t.Parallel()

	rw := &mockConfigRW{}
	buf, err := newTestProjectCmd(rw, "", []string{
		"allow", "github.com/foo/bar",
		"--non-interactive", "--no-credentials",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if rw.written == nil {
		t.Fatal("expected config to be written")
	}
	// Output should confirm project allowed.
	if !strings.Contains(buf.String(), "project allowed") {
		t.Errorf("expected 'project allowed' in output, got: %s", buf.String())
	}
}

func TestProjectAllowNonInteractiveWithoutNoCredentials(t *testing.T) {
	t.Parallel()

	rw := &mockConfigRW{}
	buf, err := newTestProjectCmd(rw, "", []string{
		"allow", "github.com/foo/bar", "--non-interactive",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Should warn and write without credentials.
	if rw.written == nil {
		t.Fatal("expected config to be written")
	}
	if rw.written.Projects[0].CredentialHelper != nil {
		t.Error("expected nil credential helper in non-interactive mode")
	}
	// Warning should mention credentials.
	if !strings.Contains(buf.String(), "credentials") {
		t.Errorf("expected warning about credentials in output, got: %s", buf.String())
	}
}

func TestProjectAllowInteractiveOSXKeychain(t *testing.T) {
	t.Parallel()

	rw := &mockConfigRW{}
	// Simulate user selecting "1" (osxkeychain).
	_, err := newTestProjectCmd(rw, "1\n", []string{
		"allow", "github.com/foo/bar",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if rw.written == nil {
		t.Fatal("expected config to be written")
	}
	h := rw.written.Projects[0].CredentialHelper
	if h == nil {
		t.Fatal("expected credential helper to be set")
	}
	if h.Kind != afclient.CredentialHelperOSXKeychain {
		t.Errorf("expected osxkeychain, got %q", h.Kind)
	}
}

func TestProjectAllowInteractiveSSH(t *testing.T) {
	t.Parallel()

	rw := &mockConfigRW{}
	// Simulate "2\n" then custom key path.
	_, err := newTestProjectCmd(rw, "2\n/home/user/.ssh/id_rsa\n", []string{
		"allow", "github.com/foo/bar",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	h := rw.written.Projects[0].CredentialHelper
	if h == nil {
		t.Fatal("expected credential helper")
	}
	if h.Kind != afclient.CredentialHelperSSH {
		t.Errorf("expected ssh, got %q", h.Kind)
	}
	if h.SSHKeyPath != "/home/user/.ssh/id_rsa" {
		t.Errorf("expected /home/user/.ssh/id_rsa, got %q", h.SSHKeyPath)
	}
}

func TestProjectAllowInteractiveSSHDefaultPath(t *testing.T) {
	t.Parallel()

	rw := &mockConfigRW{}
	// "2\n" then empty line → default path.
	_, err := newTestProjectCmd(rw, "2\n\n", []string{
		"allow", "github.com/foo/bar",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	h := rw.written.Projects[0].CredentialHelper
	if h.SSHKeyPath != "~/.ssh/id_ed25519" {
		t.Errorf("expected default key path, got %q", h.SSHKeyPath)
	}
}

func TestProjectAllowInteractivePAT(t *testing.T) {
	t.Parallel()

	rw := &mockConfigRW{}
	// "3\n" then env-var name.
	_, err := newTestProjectCmd(rw, "3\nMY_TOKEN\n", []string{
		"allow", "github.com/foo/bar",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	h := rw.written.Projects[0].CredentialHelper
	if h.Kind != afclient.CredentialHelperPAT {
		t.Errorf("expected pat, got %q", h.Kind)
	}
	if h.EnvVarName != "MY_TOKEN" {
		t.Errorf("expected MY_TOKEN, got %q", h.EnvVarName)
	}
}

func TestProjectAllowInteractivePATDefault(t *testing.T) {
	t.Parallel()

	rw := &mockConfigRW{}
	// "3\n" then empty → default GITHUB_TOKEN.
	_, err := newTestProjectCmd(rw, "3\n\n", []string{
		"allow", "github.com/foo/bar",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	h := rw.written.Projects[0].CredentialHelper
	if h.EnvVarName != "GITHUB_TOKEN" {
		t.Errorf("expected GITHUB_TOKEN, got %q", h.EnvVarName)
	}
}

func TestProjectAllowInteractiveGH(t *testing.T) {
	t.Parallel()

	rw := &mockConfigRW{}
	_, err := newTestProjectCmd(rw, "4\n", []string{
		"allow", "github.com/foo/bar",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	h := rw.written.Projects[0].CredentialHelper
	if h.Kind != afclient.CredentialHelperGH {
		t.Errorf("expected gh, got %q", h.Kind)
	}
}

func TestProjectAllowInteractiveNone(t *testing.T) {
	t.Parallel()

	rw := &mockConfigRW{}
	_, err := newTestProjectCmd(rw, "5\n", []string{
		"allow", "github.com/foo/bar",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if rw.written.Projects[0].CredentialHelper != nil {
		t.Error("expected nil credential helper for choice 5")
	}
}

func TestProjectAllowInteractiveDefaultChoice(t *testing.T) {
	t.Parallel()

	// Empty input → default choice 1 (osxkeychain).
	rw := &mockConfigRW{}
	_, err := newTestProjectCmd(rw, "\n", []string{
		"allow", "github.com/foo/bar",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	h := rw.written.Projects[0].CredentialHelper
	if h == nil || h.Kind != afclient.CredentialHelperOSXKeychain {
		t.Errorf("expected osxkeychain default, got %+v", h)
	}
}

func TestProjectAllowInvalidChoice(t *testing.T) {
	t.Parallel()

	rw := &mockConfigRW{}
	_, err := newTestProjectCmd(rw, "99\n", []string{
		"allow", "github.com/foo/bar",
	})
	if err == nil {
		t.Fatal("expected error for invalid choice")
	}
	if !strings.Contains(err.Error(), "invalid choice") {
		t.Errorf("expected 'invalid choice' in error, got: %v", err)
	}
}

func TestProjectAllowReadError(t *testing.T) {
	t.Parallel()

	rw := &mockConfigRW{readErr: errors.New("disk error")}
	_, err := newTestProjectCmd(rw, "", []string{
		"allow", "github.com/foo/bar", "--no-credentials",
	})
	if err == nil {
		t.Fatal("expected error from read failure")
	}
}

func TestProjectAllowWriteError(t *testing.T) {
	t.Parallel()

	rw := &mockConfigRW{writeErr: errors.New("permission denied")}
	_, err := newTestProjectCmd(rw, "", []string{
		"allow", "github.com/foo/bar", "--no-credentials",
	})
	if err == nil {
		t.Fatal("expected error from write failure")
	}
}

func TestProjectAllowCustomCloneStrategy(t *testing.T) {
	t.Parallel()

	rw := &mockConfigRW{}
	_, err := newTestProjectCmd(rw, "", []string{
		"allow", "github.com/foo/bar",
		"--no-credentials",
		"--clone-strategy", "full",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if rw.written.Projects[0].CloneStrategy != afclient.CloneFull {
		t.Errorf("expected full clone strategy, got %q", rw.written.Projects[0].CloneStrategy)
	}
}

func TestProjectAllowUpsertExisting(t *testing.T) {
	t.Parallel()

	existing := &afclient.DaemonYAML{
		Projects: []afclient.ProjectEntry{
			{
				RepoURL:          "github.com/foo/bar",
				CloneStrategy:    afclient.CloneShallow,
				CredentialHelper: &afclient.CredentialHelper{Kind: afclient.CredentialHelperGH},
			},
		},
	}
	rw := &mockConfigRW{cfg: existing}
	_, err := newTestProjectCmd(rw, "", []string{
		"allow", "github.com/foo/bar",
		"--no-credentials",
		"--clone-strategy", "full",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(rw.written.Projects) != 1 {
		t.Fatalf("expected 1 project after upsert, got %d", len(rw.written.Projects))
	}
	if rw.written.Projects[0].CloneStrategy != afclient.CloneFull {
		t.Error("expected updated clone strategy")
	}
	if rw.written.Projects[0].CredentialHelper != nil {
		t.Error("expected nil credential helper after upsert with --no-credentials")
	}
}

// ── credentials ───────────────────────────────────────────────────────────────

func TestProjectCredentialsNotInAllowlist(t *testing.T) {
	t.Parallel()

	rw := &mockConfigRW{}
	_, err := newTestProjectCmd(rw, "1\n", []string{
		"credentials", "github.com/foo/bar",
	})
	if err == nil {
		t.Fatal("expected error for project not in allowlist")
	}
	if !strings.Contains(err.Error(), "not in the allowlist") {
		t.Errorf("expected 'not in the allowlist' in error, got: %v", err)
	}
}

func TestProjectCredentialsUpdateExisting(t *testing.T) {
	t.Parallel()

	existing := &afclient.DaemonYAML{
		Projects: []afclient.ProjectEntry{
			{RepoURL: "github.com/foo/bar", CloneStrategy: afclient.CloneShallow},
		},
	}
	rw := &mockConfigRW{cfg: existing}
	_, err := newTestProjectCmd(rw, "4\n", []string{
		"credentials", "github.com/foo/bar",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if rw.written == nil {
		t.Fatal("expected config to be written")
	}
	h := rw.written.Projects[0].CredentialHelper
	if h == nil || h.Kind != afclient.CredentialHelperGH {
		t.Errorf("expected gh credential helper, got %+v", h)
	}
}

func TestProjectCredentialsNonInteractiveError(t *testing.T) {
	t.Parallel()

	existing := &afclient.DaemonYAML{
		Projects: []afclient.ProjectEntry{
			{RepoURL: "github.com/foo/bar"},
		},
	}
	rw := &mockConfigRW{cfg: existing}
	_, err := newTestProjectCmd(rw, "", []string{
		"credentials", "github.com/foo/bar", "--non-interactive",
	})
	if err == nil {
		t.Fatal("expected error in non-interactive mode")
	}
}

func TestProjectCredentialsReadError(t *testing.T) {
	t.Parallel()

	rw := &mockConfigRW{readErr: errors.New("io error")}
	_, err := newTestProjectCmd(rw, "1\n", []string{
		"credentials", "github.com/foo/bar",
	})
	if err == nil {
		t.Fatal("expected error from read failure")
	}
}

func TestProjectCredentialsWriteError(t *testing.T) {
	t.Parallel()

	existing := &afclient.DaemonYAML{
		Projects: []afclient.ProjectEntry{
			{RepoURL: "github.com/foo/bar"},
		},
	}
	rw := &mockConfigRW{cfg: existing, writeErr: errors.New("write fail")}
	_, err := newTestProjectCmd(rw, "1\n", []string{
		"credentials", "github.com/foo/bar",
	})
	if err == nil {
		t.Fatal("expected error from write failure")
	}
}

// ── list ──────────────────────────────────────────────────────────────────────

func TestProjectListEmpty(t *testing.T) {
	t.Parallel()

	rw := &mockConfigRW{}
	buf, err := newTestProjectCmd(rw, "", []string{"list"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if !strings.Contains(buf.String(), "No projects") {
		t.Errorf("expected 'No projects' in output, got: %s", buf.String())
	}
}

func TestProjectListWithProjects(t *testing.T) {
	t.Parallel()

	existing := &afclient.DaemonYAML{
		Projects: []afclient.ProjectEntry{
			{
				RepoURL:          "github.com/foo/bar",
				CloneStrategy:    afclient.CloneShallow,
				CredentialHelper: &afclient.CredentialHelper{Kind: afclient.CredentialHelperOSXKeychain},
			},
			{
				RepoURL:          "github.com/baz/qux",
				CloneStrategy:    afclient.CloneFull,
				CredentialHelper: nil,
			},
		},
	}
	rw := &mockConfigRW{cfg: existing}
	buf, err := newTestProjectCmd(rw, "", []string{"list"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	out := buf.String()
	if !strings.Contains(out, "github.com/foo/bar") {
		t.Errorf("expected foo/bar in output, got: %s", out)
	}
	if !strings.Contains(out, "github.com/baz/qux") {
		t.Errorf("expected baz/qux in output, got: %s", out)
	}
	if !strings.Contains(out, "osxkeychain") {
		t.Errorf("expected 'osxkeychain' in output, got: %s", out)
	}
	if !strings.Contains(out, "(none") {
		t.Errorf("expected '(none' for unconfigured credentials, got: %s", out)
	}
	if !strings.Contains(out, "REPO URL") {
		t.Errorf("expected table header in output, got: %s", out)
	}
}

func TestProjectListSSHHelper(t *testing.T) {
	t.Parallel()

	existing := &afclient.DaemonYAML{
		Projects: []afclient.ProjectEntry{
			{
				RepoURL:       "github.com/foo/bar",
				CloneStrategy: afclient.CloneShallow,
				CredentialHelper: &afclient.CredentialHelper{
					Kind:       afclient.CredentialHelperSSH,
					SSHKeyPath: "/home/user/.ssh/id_rsa",
				},
			},
		},
	}
	rw := &mockConfigRW{cfg: existing}
	buf, err := newTestProjectCmd(rw, "", []string{"list"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if !strings.Contains(buf.String(), "/home/user/.ssh/id_rsa") {
		t.Errorf("expected SSH key path in output, got: %s", buf.String())
	}
}

func TestProjectListPATHelper(t *testing.T) {
	t.Parallel()

	existing := &afclient.DaemonYAML{
		Projects: []afclient.ProjectEntry{
			{
				RepoURL:       "github.com/foo/bar",
				CloneStrategy: afclient.CloneShallow,
				CredentialHelper: &afclient.CredentialHelper{
					Kind:       afclient.CredentialHelperPAT,
					EnvVarName: "MY_GITHUB_TOKEN",
				},
			},
		},
	}
	rw := &mockConfigRW{cfg: existing}
	buf, err := newTestProjectCmd(rw, "", []string{"list"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if !strings.Contains(buf.String(), "$MY_GITHUB_TOKEN") {
		t.Errorf("expected env-var name in output, got: %s", buf.String())
	}
}

func TestProjectListReadError(t *testing.T) {
	t.Parallel()

	rw := &mockConfigRW{readErr: errors.New("read error")}
	_, err := newTestProjectCmd(rw, "", []string{"list"})
	if err == nil {
		t.Fatal("expected error from read failure")
	}
}

// ── remove ────────────────────────────────────────────────────────────────────

func TestProjectRemoveConfirmed(t *testing.T) {
	t.Parallel()

	existing := &afclient.DaemonYAML{
		Projects: []afclient.ProjectEntry{
			{RepoURL: "github.com/foo/bar"},
			{RepoURL: "github.com/keep/me"},
		},
	}
	rw := &mockConfigRW{cfg: existing}
	buf, err := newTestProjectCmd(rw, "y\n", []string{
		"remove", "github.com/foo/bar",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if rw.written == nil {
		t.Fatal("expected config to be written")
	}
	if len(rw.written.Projects) != 1 {
		t.Fatalf("expected 1 project remaining, got %d", len(rw.written.Projects))
	}
	if rw.written.Projects[0].RepoURL != "github.com/keep/me" {
		t.Errorf("expected keep/me to remain, got %q", rw.written.Projects[0].RepoURL)
	}
	if !strings.Contains(buf.String(), "project removed") {
		t.Errorf("expected 'project removed' in output, got: %s", buf.String())
	}
}

func TestProjectRemoveAborted(t *testing.T) {
	t.Parallel()

	existing := &afclient.DaemonYAML{
		Projects: []afclient.ProjectEntry{
			{RepoURL: "github.com/foo/bar"},
		},
	}
	rw := &mockConfigRW{cfg: existing}
	buf, err := newTestProjectCmd(rw, "n\n", []string{
		"remove", "github.com/foo/bar",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if rw.written != nil {
		t.Error("expected config NOT to be written on abort")
	}
	if !strings.Contains(buf.String(), "aborted") {
		t.Errorf("expected 'aborted' in output, got: %s", buf.String())
	}
}

func TestProjectRemoveWithYesFlag(t *testing.T) {
	t.Parallel()

	existing := &afclient.DaemonYAML{
		Projects: []afclient.ProjectEntry{
			{RepoURL: "github.com/foo/bar"},
		},
	}
	rw := &mockConfigRW{cfg: existing}
	_, err := newTestProjectCmd(rw, "", []string{
		"remove", "github.com/foo/bar", "--yes",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if rw.written == nil {
		t.Fatal("expected config to be written")
	}
	if len(rw.written.Projects) != 0 {
		t.Errorf("expected empty project list, got %d projects", len(rw.written.Projects))
	}
}

func TestProjectRemoveNonInteractive(t *testing.T) {
	t.Parallel()

	existing := &afclient.DaemonYAML{
		Projects: []afclient.ProjectEntry{
			{RepoURL: "github.com/foo/bar"},
		},
	}
	rw := &mockConfigRW{cfg: existing}
	_, err := newTestProjectCmd(rw, "", []string{
		"remove", "github.com/foo/bar", "--non-interactive",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if rw.written == nil || len(rw.written.Projects) != 0 {
		t.Error("expected project to be removed in non-interactive mode")
	}
}

func TestProjectRemoveNotFound(t *testing.T) {
	t.Parallel()

	rw := &mockConfigRW{}
	_, err := newTestProjectCmd(rw, "", []string{
		"remove", "github.com/foo/bar", "--yes",
	})
	if err == nil {
		t.Fatal("expected error for project not found")
	}
	if !strings.Contains(err.Error(), "not found") {
		t.Errorf("expected 'not found' in error, got: %v", err)
	}
}

func TestProjectRemoveReadError(t *testing.T) {
	t.Parallel()

	rw := &mockConfigRW{readErr: errors.New("io error")}
	_, err := newTestProjectCmd(rw, "", []string{
		"remove", "github.com/foo/bar", "--yes",
	})
	if err == nil {
		t.Fatal("expected error from read failure")
	}
}

func TestProjectRemoveWriteError(t *testing.T) {
	t.Parallel()

	existing := &afclient.DaemonYAML{
		Projects: []afclient.ProjectEntry{
			{RepoURL: "github.com/foo/bar"},
		},
	}
	rw := &mockConfigRW{cfg: existing, writeErr: errors.New("write fail")}
	_, err := newTestProjectCmd(rw, "", []string{
		"remove", "github.com/foo/bar", "--yes",
	})
	if err == nil {
		t.Fatal("expected error from write failure")
	}
}

// ── credentialHelperString ────────────────────────────────────────────────────

func TestCredentialHelperStringNil(t *testing.T) {
	t.Parallel()

	s := credentialHelperString(nil)
	if !strings.Contains(s, "none") {
		t.Errorf("expected 'none' for nil helper, got %q", s)
	}
}

func TestCredentialHelperStringSSHWithPath(t *testing.T) {
	t.Parallel()

	h := &afclient.CredentialHelper{Kind: afclient.CredentialHelperSSH, SSHKeyPath: "/path/to/key"}
	s := credentialHelperString(h)
	if !strings.Contains(s, "/path/to/key") {
		t.Errorf("expected key path in string, got %q", s)
	}
}

func TestCredentialHelperStringPATWithEnvVar(t *testing.T) {
	t.Parallel()

	h := &afclient.CredentialHelper{Kind: afclient.CredentialHelperPAT, EnvVarName: "MY_TOKEN"}
	s := credentialHelperString(h)
	if !strings.Contains(s, "$MY_TOKEN") {
		t.Errorf("expected $MY_TOKEN in string, got %q", s)
	}
}

// ── daemon_config helpers ─────────────────────────────────────────────────────

func TestDaemonYAMLFindProject(t *testing.T) {
	t.Parallel()

	d := &afclient.DaemonYAML{
		Projects: []afclient.ProjectEntry{
			{RepoURL: "github.com/a/b"},
			{RepoURL: "github.com/c/d"},
		},
	}
	if i := d.FindProject("github.com/a/b"); i != 0 {
		t.Errorf("expected index 0, got %d", i)
	}
	if i := d.FindProject("github.com/c/d"); i != 1 {
		t.Errorf("expected index 1, got %d", i)
	}
	if i := d.FindProject("github.com/x/y"); i != -1 {
		t.Errorf("expected -1 for missing project, got %d", i)
	}
}

func TestDaemonYAMLAddOrUpdateProject(t *testing.T) {
	t.Parallel()

	d := &afclient.DaemonYAML{}
	d.AddOrUpdateProject(afclient.ProjectEntry{RepoURL: "github.com/a/b"})
	if len(d.Projects) != 1 {
		t.Fatalf("expected 1 project, got %d", len(d.Projects))
	}

	// Update existing.
	d.AddOrUpdateProject(afclient.ProjectEntry{
		RepoURL:       "github.com/a/b",
		CloneStrategy: afclient.CloneFull,
	})
	if len(d.Projects) != 1 {
		t.Fatalf("expected still 1 project after update, got %d", len(d.Projects))
	}
	if d.Projects[0].CloneStrategy != afclient.CloneFull {
		t.Errorf("expected full strategy after update, got %q", d.Projects[0].CloneStrategy)
	}

	// Add new.
	d.AddOrUpdateProject(afclient.ProjectEntry{RepoURL: "github.com/c/d"})
	if len(d.Projects) != 2 {
		t.Fatalf("expected 2 projects, got %d", len(d.Projects))
	}
}

func TestDaemonYAMLRemoveProject(t *testing.T) {
	t.Parallel()

	d := &afclient.DaemonYAML{
		Projects: []afclient.ProjectEntry{
			{RepoURL: "github.com/a/b"},
			{RepoURL: "github.com/c/d"},
			{RepoURL: "github.com/e/f"},
		},
	}

	removed := d.RemoveProject("github.com/c/d")
	if !removed {
		t.Fatal("expected true from RemoveProject for existing project")
	}
	if len(d.Projects) != 2 {
		t.Fatalf("expected 2 projects after remove, got %d", len(d.Projects))
	}

	notRemoved := d.RemoveProject("github.com/x/y")
	if notRemoved {
		t.Error("expected false from RemoveProject for missing project")
	}
}
