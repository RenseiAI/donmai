package daemon

import (
	"strings"
	"testing"

	"github.com/RenseiAI/donmai/runtime/workarea"
)

// TestSpawner_DeclarationEntriesCheckedAgainstAllowlist pins the admission
// half of the declaration check: every declared repository source must
// resolve against the project allowlist, not only the singular
// repository. An unlisted second entry is refused with the same
// not-configured shape as the singular path; a fully listed declaration
// passes the check.
func TestSpawner_DeclarationEntriesCheckedAgainstAllowlist(t *testing.T) {
	t.Parallel()

	newSpawner := func() *WorkerSpawner {
		return NewWorkerSpawner(SpawnerOptions{
			Projects: []ProjectConfig{
				{ID: "primary", Repository: "github.com/acme/primary"},
				{ID: "secondary", Repository: "github.com/acme/secondary"},
			},
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

	t.Run("unlisted declaration entry refused", func(t *testing.T) {
		t.Parallel()
		s := newSpawner()
		spec := SessionSpec{SessionID: "declared-unlisted", Repository: "github.com/acme/primary", RepositoryDeclaration: declaration("github.com/elsewhere/hidden")}
		s.mu.Lock()
		defer s.mu.Unlock()
		if _, err := s.resolveProjectForSpecLocked(spec); err == nil {
			t.Fatal("resolveProjectForSpecLocked admitted an unlisted declaration entry, want refusal")
		} else if got := err.Error(); !strings.Contains(got, "is not configured") {
			t.Fatalf("error = %q, want the not-configured shape", got)
		}
	})

	t.Run("fully listed declaration admitted", func(t *testing.T) {
		t.Parallel()
		s := newSpawner()
		spec := SessionSpec{SessionID: "declared-listed", Repository: "github.com/acme/primary", RepositoryDeclaration: declaration("github.com/acme/secondary")}
		s.mu.Lock()
		defer s.mu.Unlock()
		if _, err := s.resolveProjectForSpecLocked(spec); err != nil {
			t.Fatalf("resolveProjectForSpecLocked = %v, want admission", err)
		}
	})

	t.Run("no declaration keeps the singular path", func(t *testing.T) {
		t.Parallel()
		s := newSpawner()
		spec := SessionSpec{SessionID: "singular", Repository: "github.com/acme/primary"}
		s.mu.Lock()
		defer s.mu.Unlock()
		if _, err := s.resolveProjectForSpecLocked(spec); err != nil {
			t.Fatalf("resolveProjectForSpecLocked = %v, want admission", err)
		}
	})
}
