package runner

import (
	"github.com/RenseiAI/donmai/agent"
	"github.com/RenseiAI/donmai/runtime/workarea"
)

// resolveRepositoryWorkarea resolves the work item's normalized repository
// declaration for provider and validates it against the executor's
// attestations. Both the daemon preflight and the runner call it, so both
// derive the same declaration from the same work item.
func resolveRepositoryWorkarea(
	qw QueuedWork,
	provider agent.Provider,
) (*workarea.NormalizedDeclaration, workarea.ExecutorWorkareaCapabilities, error) {
	normalized, _, capabilities, err := resolveRepositoryWorkareaDeclaration(qw, provider)
	return normalized, capabilities, err
}

// effectiveRepositoryDeclaration is the work item's own repository
// declaration, else, for a plain single-repository work item whose executor
// can hold a declared read-only leaf, the declaration its
// DONMAI_SIBLING_REPOS entries describe (siblings.go). Nil when neither
// applies; derived = true for the second.
func effectiveRepositoryDeclaration(qw QueuedWork, provider agent.Provider) (declaration *workarea.RepositoryDeclarationV1, derived bool) {
	if qw.RepositoryDeclaration != nil {
		return qw.RepositoryDeclaration, false
	}
	if !siblingContextEligible(qw) || !executorAttestsSiblingContext(provider) {
		return nil, false
	}
	declaration = siblingContextDeclaration(qw)
	return declaration, declaration != nil
}

// resolveRepositoryWorkareaDeclaration is resolveRepositoryWorkarea that also
// returns the wire declaration it resolved, for provisioning. A declaration
// derived from DONMAI_SIBLING_REPOS that does not validate is dropped rather
// than failing the session: sibling context is never fatal, and the work item
// then provisions as it did without one.
func resolveRepositoryWorkareaDeclaration(
	qw QueuedWork,
	provider agent.Provider,
) (*workarea.NormalizedDeclaration, *workarea.RepositoryDeclarationV1, workarea.ExecutorWorkareaCapabilities, error) {
	capabilities := workarea.ExecutorWorkareaCapabilities{}
	declaration, derived := effectiveRepositoryDeclaration(qw, provider)
	if declaration == nil {
		return nil, nil, capabilities, nil
	}
	normalized, capabilities, err := validateRepositoryWorkarea(qw, provider, declaration)
	if err != nil {
		if derived {
			return nil, nil, workarea.ExecutorWorkareaCapabilities{}, nil
		}
		return nil, nil, capabilities, err
	}
	return normalized, declaration, capabilities, nil
}

func validateRepositoryWorkarea(
	qw QueuedWork,
	provider agent.Provider,
	declaration *workarea.RepositoryDeclarationV1,
) (*workarea.NormalizedDeclaration, workarea.ExecutorWorkareaCapabilities, error) {
	capabilities := workarea.ExecutorWorkareaCapabilities{}
	supportsReadOnlySelectedCWD := false
	normalized, err := declaration.Normalize()
	if err != nil {
		return nil, capabilities, err
	}
	if err := normalized.ValidatePrimarySource(workarea.RepositorySource{Repository: qw.Repository, Ref: qw.Ref}); err != nil {
		return nil, capabilities, err
	}
	if harness, ok := provider.(agent.HarnessProvider); ok {
		manifest := harness.Manifest()
		for _, protocol := range manifest.Caps.MultiRepositoryWorkareaProtocols {
			capabilities.MultiRepositoryWorkareaProtocols = append(capabilities.MultiRepositoryWorkareaProtocols, workarea.Protocol(protocol))
		}
		capabilities.RepositoryAuthorityEnforcement = workarea.RepositoryAuthorityEnforcement(manifest.Caps.RepositoryAuthorityEnforcement)
		supportsReadOnlySelectedCWD = manifest.Caps.SupportsReadOnlySelectedCWD
	}
	if err := capabilities.ValidateFor(normalized); err != nil {
		return nil, capabilities, err
	}
	if normalized.Selected.Authority == workarea.RepositoryReadOnly && !supportsReadOnlySelectedCWD {
		return nil, capabilities, &workarea.RepositoryContractError{
			Reason: workarea.ReasonAuthorityEnforcementMissing, RuleID: workarea.RuleReadOnlyExecutorEnforced,
			Repository: normalized.Selected.Name,
			Detail:     "the interactive executor cannot keep its selected CWD read-only",
		}
	}
	return &normalized, capabilities, nil
}

// repositoryAuthorityPolicy builds the sandbox authority policy for a
// declared workarea: each declared repository's provisioned path, writable
// only when the declaration grants it mutable authority.
//
// A declared repository without a provisioned path is excused only when the
// worktree manager reported it skipped — a read-only context repository,
// never the selected one, whose clone failed and which the session runs
// without. Any other missing path is a contract violation that fails the
// session: a repository is never silently dropped from the policy, since a
// read-only repository missing from it would no longer be held read-only.
func repositoryAuthorityPolicy(
	declaration workarea.NormalizedDeclaration,
	paths map[string]string,
	skipped map[string]struct{},
	workareaRoot, selectedPath, enforcement string,
) (*agent.RepositoryAuthorityPolicy, error) {
	policy := &agent.RepositoryAuthorityPolicy{
		Protocol: string(declaration.Protocol), WorkareaRoot: workareaRoot,
		SelectedPath: selectedPath, Enforcement: enforcement,
	}
	for _, repository := range declaration.Repositories {
		path := paths[repository.Name]
		if path == "" {
			if _, wasSkipped := skipped[repository.Name]; wasSkipped &&
				repository.Role == workarea.RepositoryRoleContext &&
				repository.Authority == workarea.RepositoryReadOnly &&
				repository.Name != declaration.Selected.Name {
				continue
			}
			return nil, &workarea.RepositoryContractError{
				Reason: workarea.ReasonDeclarationRecordInvalid, RuleID: workarea.RuleDeclarationRecordSecretFree,
				Repository: repository.Name, Detail: "declared repository has no provisioned path",
			}
		}
		if repository.Authority == workarea.RepositoryMutable {
			policy.MutablePaths = append(policy.MutablePaths, path)
		} else {
			policy.ReadOnlyPaths = append(policy.ReadOnlyPaths, path)
		}
	}
	return policy, nil
}
