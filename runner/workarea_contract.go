package runner

import (
	"github.com/RenseiAI/donmai/agent"
	"github.com/RenseiAI/donmai/runtime/workarea"
)

func resolveRepositoryWorkarea(
	qw QueuedWork,
	provider agent.Provider,
) (*workarea.NormalizedDeclaration, workarea.ExecutorWorkareaCapabilities, error) {
	capabilities := workarea.ExecutorWorkareaCapabilities{}
	supportsReadOnlySelectedCWD := false
	if qw.RepositoryDeclaration == nil {
		return nil, capabilities, nil
	}
	normalized, err := qw.RepositoryDeclaration.Normalize()
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
