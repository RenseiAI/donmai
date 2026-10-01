package worktree

import (
	"context"
	"fmt"
	"strings"
)

// ValidateBranchBaseRef accepts a literal branch name, never a ref expression
// or an implicit default. Existence in refs/heads must be proven separately.
// Unlike legacy ref helpers it does not strip remote/ref namespace prefixes.
func ValidateBranchBaseRef(ref string) error {
	invalid := ref == "" || ref == "HEAD" || ref == "@" || strings.TrimSpace(ref) != ref || strings.HasPrefix(ref, "-") || strings.HasPrefix(ref, "refs/") || strings.Contains(ref, "..") || strings.Contains(ref, "@{") || strings.Contains(ref, "//") || strings.ContainsAny(ref, " ~^:?*[\\") || len(ref) > 1024
	for _, r := range ref {
		if r < 0x20 || r == 0x7f {
			invalid = true
		}
	}
	for _, part := range strings.Split(ref, "/") {
		if part == "" || strings.HasPrefix(part, ".") || strings.HasSuffix(part, ".") || strings.HasSuffix(part, ".lock") {
			invalid = true
		}
	}
	if invalid {
		return fmt.Errorf("%w: an explicit literal base branch is required", ErrInvalidBaseRef)
	}
	return nil
}

func validateRequiredBranchBase(spec ProvisionSpec) error {
	if !spec.RequireBranchBase {
		return nil
	}
	if err := ValidateBranchBaseRef(spec.BaseRef); err != nil {
		return err
	}
	if spec.SkipBaseFetch || spec.Mode == ModeShared || spec.RepositoryDeclaration != nil || spec.PullRequest != nil || spec.Strategy == StrategyEmpty {
		return fmt.Errorf("%w: branch-only base requires fresh exclusive singular provisioning", ErrInvalidBaseRef)
	}
	if spec.Strategy == StrategyClone && spec.Branch != spec.BaseRef {
		return fmt.Errorf("%w: clone branch differs from required base", ErrInvalidBaseRef)
	}
	return nil
}

func branchTipSHA(value string) bool {
	if len(value) != 40 && len(value) != 64 {
		return false
	}
	for _, r := range value {
		if !(r >= '0' && r <= '9' || r >= 'a' && r <= 'f') {
			return false
		}
	}
	return true
}

func (m *Manager) exactBranchTrackingTip(ctx context.Context, repository, branch string) (string, error) {
	output, err := m.runGit(ctx, "", "-C", repository, "show-ref", "--verify", "--hash", "refs/remotes/origin/"+branch)
	tip := strings.TrimSpace(string(output))
	if err != nil || !branchTipSHA(tip) {
		return "", fmt.Errorf("%w: current base branch tracking proof is unavailable", ErrInvalidBaseRef)
	}
	return tip, nil
}

func (m *Manager) verifyClonedBranchBase(ctx context.Context, repository, branch string) error {
	output, err := m.runGit(ctx, "", "-C", repository, "symbolic-ref", "--quiet", "HEAD")
	if err != nil || strings.TrimSpace(string(output)) != "refs/heads/"+branch {
		return fmt.Errorf("%w: clone did not select the required branch", ErrInvalidBaseRef)
	}
	tip, err := m.exactBranchTrackingTip(ctx, repository, branch)
	if err != nil {
		return err
	}
	output, err = m.runGit(ctx, "", "-C", repository, "rev-parse", "--verify", "HEAD^{commit}")
	if err != nil || strings.TrimSpace(string(output)) != tip {
		return fmt.Errorf("%w: clone head differs from required remote branch", ErrInvalidBaseRef)
	}
	return nil
}
