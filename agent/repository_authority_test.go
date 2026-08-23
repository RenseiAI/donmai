package agent

import (
	"errors"
	"path/filepath"
	"testing"
)

func TestValidateSpecCapabilitiesRequiresCompleteDirectChildAuthorityPartition(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	primary := filepath.Join(root, "primary")
	contextPath := filepath.Join(root, "context")
	manifest := HarnessManifest{Caps: HarnessCaps{
		MultiRepositoryWorkareaProtocols: []string{"session-root-v1"},
		RepositoryAuthorityEnforcement:   "isolated-read-only-v1",
	}}
	base := Spec{
		Cwd: primary, SandboxLevel: SandboxWorkspaceWrite,
		RepositoryAuthority: &RepositoryAuthorityPolicy{
			Protocol: "session-root-v1", WorkareaRoot: root, SelectedPath: primary,
			MutablePaths: []string{primary, contextPath}, Enforcement: "isolated-read-only-v1",
		},
	}
	if err := ValidateSpecCapabilities(base, manifest); err != nil {
		t.Fatalf("complete all-mutable partition refused: %v", err)
	}

	tests := []struct {
		name   string
		mutate func(*RepositoryAuthorityPolicy)
	}{
		{name: "nested path", mutate: func(policy *RepositoryAuthorityPolicy) {
			policy.MutablePaths[1] = filepath.Join(primary, "nested")
		}},
		{name: "cross partition duplicate", mutate: func(policy *RepositoryAuthorityPolicy) {
			policy.MutablePaths = []string{primary}
			policy.ReadOnlyPaths = []string{primary}
		}},
		{name: "selected absent", mutate: func(policy *RepositoryAuthorityPolicy) {
			policy.MutablePaths = []string{contextPath}
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			spec := base
			policy := *base.RepositoryAuthority
			policy.MutablePaths = append([]string(nil), base.RepositoryAuthority.MutablePaths...)
			policy.ReadOnlyPaths = append([]string(nil), base.RepositoryAuthority.ReadOnlyPaths...)
			test.mutate(&policy)
			spec.RepositoryAuthority = &policy
			err := ValidateSpecCapabilities(spec, manifest)
			var denial *SpecAdmissionError
			if !errors.As(err, &denial) || denial.Field != "repositoryAuthority" {
				t.Fatalf("ValidateSpecCapabilities = %v, want repositoryAuthority denial", err)
			}
		})
	}
}
