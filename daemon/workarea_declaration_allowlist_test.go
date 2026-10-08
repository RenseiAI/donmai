package daemon

import (
	"strings"
	"testing"

	"github.com/RenseiAI/donmai/runtime/workarea"
)

// TestSpawner_DeclarationEntriesCheckedAgainstAllowlist pins the admission
// half of the declaration check: every declared repository source must be
// the location of a repository configured for the session's own project,
// and that project must be enabled on this machine, on every admission path
// (repository-resolved, project-scoped with a repository, project-scoped
// without one). A declaration cannot carry a repository the singular
// repository field would be refused for.
func TestSpawner_DeclarationEntriesCheckedAgainstAllowlist(t *testing.T) {
	t.Parallel()

	newSpawner := func() *WorkerSpawner {
		return NewWorkerSpawner(SpawnerOptions{
			Projects: []ProjectConfig{
				{ID: "primary", Repository: "github.com/acme/primary"},
				{ID: "primary", Repository: "github.com/acme/secondary"},
				{ID: "other", Repository: "github.com/acme/other"},
				{ID: "disabled", Repository: "github.com/acme/disabled"},
			},
			EnabledProjectIDs:     []string{"primary", "other"},
			MaxConcurrentSessions: 4,
		})
	}

	declaration := func(second string) *workarea.RepositoryDeclarationV1 {
		return &workarea.RepositoryDeclarationV1{
			Protocol: workarea.ProtocolSessionRootV1,
			Repositories: []workarea.DeclaredRepositoryV1{
				{Source: workarea.RepositorySource{Repository: "github.com/acme/primary"}, Role: workarea.RepositoryRolePrimary, Authority: workarea.RepositoryMutable},
				{Source: workarea.RepositorySource{Repository: second}, Name: "secondary", Role: workarea.RepositoryRoleSecondary, Authority: workarea.RepositoryMutable},
			},
		}
	}

	shapes := []struct {
		name string
		spec func(*workarea.RepositoryDeclarationV1) SessionSpec
	}{
		{name: "repository-resolved", spec: func(d *workarea.RepositoryDeclarationV1) SessionSpec {
			return SessionSpec{SessionID: "declared", Repository: "github.com/acme/primary", RepositoryDeclaration: d}
		}},
		{name: "project-scoped", spec: func(d *workarea.RepositoryDeclarationV1) SessionSpec {
			return SessionSpec{SessionID: "declared", ProjectID: "primary", Repository: "github.com/acme/primary", RepositoryDeclaration: d}
		}},
		{name: "project-scoped repository-free", spec: func(d *workarea.RepositoryDeclarationV1) SessionSpec {
			return SessionSpec{SessionID: "declared", ProjectID: "primary", RepositoryDeclaration: d}
		}},
	}

	cases := []struct {
		name    string
		second  string
		wantErr string // empty: admitted
	}{
		{name: "same-project repository admitted", second: "github.com/acme/secondary"},
		{name: "same-project repository in another spelling admitted", second: "https://GitHub.com/acme/secondary.git"},
		{name: "unconfigured repository refused", second: "github.com/elsewhere/hidden", wantErr: `repository "github.com/elsewhere/hidden" is not configured for project "primary"`},
		{name: "not-enabled project's repository refused", second: "github.com/acme/disabled", wantErr: `project "disabled" is not allowed`},
		{name: "other enabled project's repository refused", second: "github.com/acme/other", wantErr: `is configured for project "other", not for the session's project "primary"`},
		{name: "suffix-spoofed location refused", second: "https://evil.example/x/github.com/acme/secondary", wantErr: "is not configured for project"},
		{name: "bare repository name refused", second: "secondary", wantErr: "is not configured for project"},
		{name: "project id as a source refused", second: "primary", wantErr: "is not configured for project"},
		{name: "empty source refused", second: " ", wantErr: "has no source"},
	}

	for _, shape := range shapes {
		for _, tc := range cases {
			t.Run(shape.name+"/"+tc.name, func(t *testing.T) {
				t.Parallel()
				s := newSpawner()
				s.mu.Lock()
				defer s.mu.Unlock()
				_, err := s.resolveProjectForSpecLocked(shape.spec(declaration(tc.second)))
				if tc.wantErr == "" {
					if err != nil {
						t.Fatalf("resolveProjectForSpecLocked = %v, want admission", err)
					}
					return
				}
				if err == nil {
					t.Fatalf("resolveProjectForSpecLocked admitted declared source %q, want refusal %q", tc.second, tc.wantErr)
				}
				if !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("error = %q, want it to contain %q", err, tc.wantErr)
				}
			})
		}
	}

	t.Run("no declaration keeps the singular path", func(t *testing.T) {
		t.Parallel()
		s := newSpawner()
		s.mu.Lock()
		defer s.mu.Unlock()
		if _, err := s.resolveProjectForSpecLocked(SessionSpec{SessionID: "singular", Repository: "github.com/acme/primary"}); err != nil {
			t.Fatalf("resolveProjectForSpecLocked = %v, want admission", err)
		}
		_, err := s.resolveProjectForSpecLocked(SessionSpec{SessionID: "singular-disabled", Repository: "github.com/acme/disabled"})
		if err == nil || !strings.Contains(err.Error(), `project "disabled" is not allowed`) {
			t.Fatalf("singular repository of a not-enabled project = %v, want the not-allowed refusal the declaration mirrors", err)
		}
	})
}
