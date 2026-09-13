package worktree

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/RenseiAI/donmai/runtime/workarea"
)

const (
	// PublicationReasonPublished means every exact clean HEAD is present at its
	// admitted remote branch or unchanged admitted remote HEAD.
	PublicationReasonPublished = "published"
	// PublicationReasonDirty means tracked, deleted, or eligible untracked
	// checkout bytes remain.
	PublicationReasonDirty = "dirty"
	// PublicationReasonUnpublished means a clean local HEAD differs from the
	// admitted remote authority.
	PublicationReasonUnpublished = "unpublished"
	// PublicationReasonUncertain means bounded inspection could not prove exact
	// publication and therefore must retain.
	PublicationReasonUncertain = "uncertain"
)

const publicationGitTimeout = 5 * time.Second

// PublicationTarget is one admitted mutable repository whose exact current
// bytes must either be published or retained before interactive teardown.
// Repository is used only as a scoped remote input; it is never logged or
// persisted by the disposition check.
type PublicationTarget struct {
	Name            string
	Repository      string
	Branch          string
	AllowRemoteHEAD bool
}

// InteractivePublicationSpec binds a disposition check to one manager-owned
// session and its admitted mutable repositories.
type InteractivePublicationSpec struct {
	SessionID    string
	Repositories []PublicationTarget
}

// PublicationAssessment is the bounded, secret-free disposition result.
type PublicationAssessment struct {
	Retain         bool
	Reason         string
	RepositoryName string
}

// AcquireInteractiveTerminalLease atomically inspects the manager-owned
// checkout and, when publication is not proved, commits the existing terminal
// lease with archive disposition before a competing teardown can remove it.
// Requested keeps an explicit upstream lease request active even when the
// checkout is clean and published.
func (m *Manager) AcquireInteractiveTerminalLease(
	ctx context.Context,
	publication InteractivePublicationSpec,
	leaseSpec workarea.AcquireSpec,
	requested bool,
) (*workarea.TerminalLease, PublicationAssessment, error) {
	if publication.SessionID == "" || leaseSpec.SessionID != publication.SessionID {
		return nil, PublicationAssessment{}, fmt.Errorf("runtime/worktree: interactive publication and lease session identities must match")
	}
	unlock := m.lockSession(publication.SessionID)
	defer unlock()

	m.mu.Lock()
	res := m.sessions[publication.SessionID]
	m.mu.Unlock()
	if res == nil {
		return nil, PublicationAssessment{}, fmt.Errorf("%w: %s", ErrUnknownSession, publication.SessionID)
	}
	assessment := m.assessInteractivePublication(ctx, *res, publication.Repositories)
	if !assessment.Retain && !requested {
		return nil, assessment, nil
	}
	if assessment.Retain {
		leaseSpec.ReleaseDisposition = "archive"
		metadata := make(map[string]string, len(leaseSpec.ReleaseMetadata)+2)
		for key, value := range leaseSpec.ReleaseMetadata {
			metadata[key] = value
		}
		metadata["retentionReason"] = "interactive-unpublished"
		metadata["publicationAssessment"] = assessment.Reason
		if assessment.RepositoryName != "" {
			metadata["publicationRepository"] = assessment.RepositoryName
		}
		leaseSpec.ReleaseMetadata = metadata
	}
	lease, err := m.acquireTerminalLeaseLocked(ctx, leaseSpec)
	return lease, assessment, err
}

func (m *Manager) assessInteractivePublication(
	ctx context.Context,
	res ProvisionResult,
	targets []PublicationTarget,
) PublicationAssessment {
	if len(targets) == 0 {
		return PublicationAssessment{Retain: true, Reason: PublicationReasonUncertain}
	}
	paths := make(map[string]string, len(res.Repositories))
	for name, path := range res.Repositories {
		paths[name] = path
	}
	for _, target := range targets {
		path := paths[target.Name]
		if path == "" && len(targets) == 1 && filepath.Clean(res.Path) == filepath.Clean(res.workareaRootOrPath()) {
			path = res.Path
		}
		assessment := m.assessRepositoryPublication(ctx, path, target)
		if assessment.Retain {
			return assessment
		}
	}
	return PublicationAssessment{Reason: PublicationReasonPublished}
}

func (m *Manager) assessRepositoryPublication(
	ctx context.Context,
	path string,
	target PublicationTarget,
) PublicationAssessment {
	retain := func(reason string) PublicationAssessment {
		return PublicationAssessment{Retain: true, Reason: reason, RepositoryName: target.Name}
	}
	if path == "" || !safePublicationRepository(target.Repository) {
		return retain(PublicationReasonUncertain)
	}
	branch, err := normalizeBaseRef(target.Branch)
	if err != nil || branch == "" {
		return retain(PublicationReasonUncertain)
	}
	config := []string{
		"-c", "core.hooksPath=/dev/null",
		"-c", "core.fsmonitor=false",
		"-c", "core.untrackedCache=false",
		"-c", "credential.helper=",
		"-c", "protocol.ext.allow=never",
		"-c", "submodule.recurse=false",
	}
	statusArgs := append(append([]string{}, config...), "-C", path, "status", "--porcelain=v1", "--untracked-files=all", "--ignore-submodules=none")
	status, err := m.runPublicationGit(ctx, target.Repository, statusArgs...)
	if err != nil {
		return retain(PublicationReasonUncertain)
	}
	if strings.TrimSpace(string(status)) != "" {
		return retain(PublicationReasonDirty)
	}
	branchArgs := append(append([]string{}, config...), "-C", path, "symbolic-ref", "--quiet", "--short", "HEAD")
	currentBranch, err := m.runPublicationGit(ctx, target.Repository, branchArgs...)
	if err != nil || strings.TrimSpace(string(currentBranch)) != branch {
		return retain(PublicationReasonUncertain)
	}
	headArgs := append(append([]string{}, config...), "-C", path, "rev-parse", "--verify", "HEAD^{commit}")
	head, err := m.runPublicationGit(ctx, target.Repository, headArgs...)
	if err != nil || !validGitObjectID(strings.TrimSpace(string(head))) {
		return retain(PublicationReasonUncertain)
	}
	localHead := strings.TrimSpace(string(head))
	remoteArgs := append(append([]string{}, config...), "ls-remote", "--exit-code", "--heads", target.Repository, "refs/heads/"+branch)
	remote, remoteErr := m.runPublicationGit(ctx, target.Repository, remoteArgs...)
	if remoteErr == nil {
		remoteHead, ok := exactRemoteHead(remote, "refs/heads/"+branch)
		if !ok {
			return retain(PublicationReasonUncertain)
		}
		if remoteHead == localHead {
			return PublicationAssessment{Reason: PublicationReasonPublished}
		}
		return retain(PublicationReasonUnpublished)
	}
	if !target.AllowRemoteHEAD {
		return retain(PublicationReasonUncertain)
	}
	headRemoteArgs := append(append([]string{}, config...), "ls-remote", "--exit-code", "--symref", target.Repository, "HEAD")
	remoteHEAD, headErr := m.runPublicationGit(ctx, target.Repository, headRemoteArgs...)
	if headErr != nil {
		return retain(PublicationReasonUncertain)
	}
	remoteHead, ok := exactRemoteHead(remoteHEAD, "HEAD")
	if !ok {
		return retain(PublicationReasonUncertain)
	}
	if remoteHead != localHead {
		return retain(PublicationReasonUnpublished)
	}
	return PublicationAssessment{Reason: PublicationReasonPublished}
}

func safePublicationRepository(repository string) bool {
	trimmed := strings.TrimSpace(repository)
	return trimmed != "" && !strings.HasPrefix(trimmed, "-") && !strings.Contains(trimmed, "::") &&
		!strings.ContainsAny(trimmed, "\x00\r\n")
}

func (m *Manager) runPublicationGit(ctx context.Context, repository string, args ...string) ([]byte, error) {
	commandCtx, cancel := context.WithTimeout(ctx, publicationGitTimeout)
	defer cancel()
	return m.runGit(commandCtx, repository, args...)
}

func exactRemoteHead(output []byte, ref string) (string, bool) {
	var found string
	for _, line := range strings.Split(strings.TrimSpace(string(output)), "\n") {
		fields := strings.Fields(line)
		if len(fields) != 2 || fields[1] != ref || !validGitObjectID(fields[0]) {
			continue
		}
		if found != "" && found != fields[0] {
			return "", false
		}
		found = fields[0]
	}
	return found, found != ""
}

func validGitObjectID(value string) bool {
	if len(value) != 40 && len(value) != 64 {
		return false
	}
	for _, char := range value {
		if (char < '0' || char > '9') && (char < 'a' || char > 'f') {
			return false
		}
	}
	return true
}
