package runner

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/RenseiAI/donmai/executioncell"
	"github.com/RenseiAI/donmai/runtime/worktree"
)

// ValidateBaseRefMember rejects malformed presence before a typed decoder can
// turn null into an absent string. It grants no transport or branch authority.
func ValidateBaseRefMember(raw []byte) error { _, _, err := baseRefPayloadMember(raw); return err }

func baseRefPayloadMember(raw []byte) (string, bool, error) {
	if len(raw) == 0 {
		return "", false, nil
	}
	var members map[string]json.RawMessage
	if err := json.Unmarshal(raw, &members); err != nil {
		return "", false, err
	}
	value, present := members["baseRef"]
	if !present {
		return "", false, nil
	}
	// Closed validation is scoped to presence of the new intent; absence
	// leaves legacy payload handling to its existing validators.
	if _, err := executioncell.NormalizeOperationalPayload(raw); err != nil {
		return "", true, err
	}
	var err error
	if bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
		return "", true, errors.New("runner: baseRef cannot be null")
	}
	var base string
	if err = json.Unmarshal(value, &base); err != nil {
		return "", true, errors.New("runner: baseRef must be a branch string")
	}
	if err = worktree.ValidateBranchBaseRef(base); err != nil {
		return "", true, err
	}
	if value, ok := members["ref"]; ok {
		var ref string
		if err = json.Unmarshal(value, &ref); err != nil || bytes.Equal(bytes.TrimSpace(value), []byte("null")) || ref != "" {
			return "", true, errors.New("runner: baseRef and amend-existing ref are mutually exclusive")
		}
	}
	return base, true, nil
}

func validateBaseRefTransport(work QueuedWork, mode RuntimeTransportMode) error {
	if len(work.OperationalPayload) > 0 {
		base, _, err := baseRefPayloadMember(work.OperationalPayload)
		if err != nil {
			return err
		}
		if base != work.BaseRef {
			return errors.New("runner: baseRef differs from immutable operational payload")
		}
	}
	if work.BaseRef == "" {
		return nil
	}
	if mode != RuntimeTransportLocalV2 {
		return errors.New("runner: baseRef requires the versioned local/v2 transport")
	}
	if err := worktree.ValidateBranchBaseRef(work.BaseRef); err != nil {
		return err
	}
	if work.Ref != "" {
		return errors.New("runner: baseRef and amend-existing ref are mutually exclusive")
	}
	if work.Branch != "" {
		if err := worktree.ValidateBranchBaseRef(work.Branch); err != nil {
			return err
		}
		if work.Branch == work.BaseRef || work.Branch == "main" || work.Branch == "master" {
			return errors.New("runner: new work branch must differ from the base and protected branch names")
		}
	}
	if work.Repository == "" || work.RepositoryDeclaration != nil || work.PullRequest != nil || work.WorkareaMode == worktree.ModeShared || work.Mode != "" {
		return fmt.Errorf("runner: baseRef requires an exclusive singular repository for new-branch work")
	}
	return nil
}

func verifyNewWorkBranch(ctx context.Context, path, branch string) error {
	value, err := runGit(ctx, path, gitIdentity{}, "symbolic-ref", "--quiet", "HEAD")
	if err != nil || strings.TrimSpace(value) != "refs/heads/"+branch {
		return errors.New("runner: new work branch was not selected; refusing execution on the base")
	}
	return nil
}
