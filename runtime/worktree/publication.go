package worktree

import (
	"context"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/RenseiAI/donmai/runtime/harnessstate"
	"github.com/RenseiAI/donmai/runtime/workarea"
)

const (
	// PublicationReasonPublished means every exact clean HEAD is present at its
	// admitted remote branch or unchanged admitted remote HEAD.
	PublicationReasonPublished = "published"
	// PublicationReasonDirty means tracked, deleted, untracked, or ignored
	// non-harness checkout bytes remain.
	PublicationReasonDirty = "dirty"
	// PublicationReasonUnpublished means a clean local HEAD differs from the
	// admitted remote authority.
	PublicationReasonUnpublished = "unpublished"
	// PublicationReasonUncertain means bounded inspection could not prove exact
	// publication and therefore must retain.
	PublicationReasonUncertain = "uncertain"
)

const (
	publicationGitTimeout       = 5 * time.Second
	publicationMaxGitOutput     = 1 << 20
	publicationMaxLocalRefCount = 1024
)

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
	if err != nil || len(status) > publicationMaxGitOutput {
		return retain(PublicationReasonUncertain)
	}
	if strings.TrimSpace(string(status)) != "" {
		return retain(PublicationReasonDirty)
	}
	ignoredArgs := append(append([]string{}, config...), "-C", path, "ls-files", "--others", "--ignored", "--exclude-standard", "-z")
	ignored, err := m.runPublicationGit(ctx, target.Repository, ignoredArgs...)
	if err != nil || len(ignored) > publicationMaxGitOutput {
		return retain(PublicationReasonUncertain)
	}
	if hasMeaningfulIgnoredFiles(ignored) {
		return retain(PublicationReasonDirty)
	}
	branchArgs := append(append([]string{}, config...), "-C", path, "symbolic-ref", "--quiet", "--short", "HEAD")
	currentBranch, err := m.runPublicationGit(ctx, target.Repository, branchArgs...)
	if err != nil || len(currentBranch) > publicationMaxGitOutput || strings.TrimSpace(string(currentBranch)) != branch {
		return retain(PublicationReasonUncertain)
	}
	headArgs := append(append([]string{}, config...), "-C", path, "rev-parse", "--verify", "HEAD^{commit}")
	head, err := m.runPublicationGit(ctx, target.Repository, headArgs...)
	if err != nil || len(head) > publicationMaxGitOutput || !validGitObjectID(strings.TrimSpace(string(head))) {
		return retain(PublicationReasonUncertain)
	}
	localHead := strings.TrimSpace(string(head))
	localRefsArgs := append(append([]string{}, config...), "-C", path, "for-each-ref", "--format=%(objectname)%09%(refname)%09%(symref)", "refs")
	localRefsOutput, err := m.runPublicationGit(ctx, target.Repository, localRefsArgs...)
	if err != nil || len(localRefsOutput) > publicationMaxGitOutput {
		return retain(PublicationReasonUncertain)
	}
	localRefs, ok := parseLocalPublicationRefs(localRefsOutput)
	if !ok || len(localRefs) > publicationMaxLocalRefCount {
		return retain(PublicationReasonUncertain)
	}

	currentRef := "refs/heads/" + branch
	patterns := make(map[string]struct{}, len(localRefs)+1)
	remoteObligations := make(map[string]string, len(localRefs))
	patterns[currentRef] = struct{}{}
	currentLocal := false
	for _, ref := range localRefs {
		if ref.Name == currentRef {
			currentLocal = ref.ObjectID == localHead && ref.Symbolic == ""
		}
		remoteRef := ref.Name
		switch {
		case strings.HasPrefix(ref.Name, "refs/heads/"), strings.HasPrefix(ref.Name, "refs/tags/"):
			if ref.Symbolic != "" {
				return retain(PublicationReasonUncertain)
			}
		case ref.Name == "refs/remotes/origin/HEAD":
			if !strings.HasPrefix(ref.Symbolic, "refs/remotes/origin/") {
				return retain(PublicationReasonUncertain)
			}
			continue
		case strings.HasPrefix(ref.Name, "refs/remotes/origin/"):
			if ref.Symbolic != "" {
				return retain(PublicationReasonUncertain)
			}
			remoteRef = "refs/heads/" + strings.TrimPrefix(ref.Name, "refs/remotes/origin/")
		default:
			// Stashes, notes, replace refs, non-admitted remote-tracking refs,
			// and every other local-only namespace carry repository state that
			// ordinary admitted heads/tags publication does not.
			return retain(PublicationReasonUnpublished)
		}
		remoteObligations[ref.Name] = remoteRef
		patterns[remoteRef] = struct{}{}
	}
	if !currentLocal {
		return retain(PublicationReasonUncertain)
	}
	remotePatterns := make([]string, 0, len(patterns))
	for pattern := range patterns {
		remotePatterns = append(remotePatterns, pattern)
	}
	sort.Strings(remotePatterns)
	remoteArgs := append(append([]string{}, config...), "ls-remote", "--refs", "--heads", "--tags", target.Repository)
	remoteArgs = append(remoteArgs, remotePatterns...)
	remoteOutput, err := m.runPublicationGit(ctx, target.Repository, remoteArgs...)
	if err != nil || len(remoteOutput) > publicationMaxGitOutput {
		return retain(PublicationReasonUncertain)
	}
	remoteRefs, ok := parseRemotePublicationRefs(remoteOutput)
	if !ok {
		return retain(PublicationReasonUncertain)
	}
	currentPublished := remoteRefs[currentRef] == localHead
	if !currentPublished && target.AllowRemoteHEAD {
		headRemoteArgs := append(append([]string{}, config...), "ls-remote", "--exit-code", "--symref", target.Repository, "HEAD")
		remoteHEAD, headErr := m.runPublicationGit(ctx, target.Repository, headRemoteArgs...)
		if headErr != nil || len(remoteHEAD) > publicationMaxGitOutput {
			return retain(PublicationReasonUncertain)
		}
		remoteHead, found := exactRemoteHead(remoteHEAD, "HEAD")
		if !found {
			return retain(PublicationReasonUncertain)
		}
		currentPublished = remoteHead == localHead
	}
	if !currentPublished {
		return retain(PublicationReasonUnpublished)
	}
	for _, ref := range localRefs {
		if ref.Name == currentRef || ref.Name == "refs/remotes/origin/HEAD" {
			continue
		}
		if remoteRefs[remoteObligations[ref.Name]] != ref.ObjectID {
			return retain(PublicationReasonUnpublished)
		}
	}
	return PublicationAssessment{Reason: PublicationReasonPublished}
}

type publicationRef struct {
	ObjectID string
	Name     string
	Symbolic string
}

func parseLocalPublicationRefs(output []byte) ([]publicationRef, bool) {
	trimmed := strings.TrimSuffix(string(output), "\n")
	if trimmed == "" {
		return nil, true
	}
	lines := strings.Split(trimmed, "\n")
	refs := make([]publicationRef, 0, len(lines))
	for _, line := range lines {
		fields := strings.Split(line, "\t")
		if len(fields) != 3 || !validGitObjectID(fields[0]) || !strings.HasPrefix(fields[1], "refs/") {
			return nil, false
		}
		refs = append(refs, publicationRef{ObjectID: fields[0], Name: fields[1], Symbolic: fields[2]})
	}
	return refs, true
}

func parseRemotePublicationRefs(output []byte) (map[string]string, bool) {
	refs := make(map[string]string)
	trimmed := strings.TrimSpace(string(output))
	if trimmed == "" {
		return refs, true
	}
	for _, line := range strings.Split(trimmed, "\n") {
		fields := strings.Fields(line)
		if len(fields) != 2 || !validGitObjectID(fields[0]) ||
			(!strings.HasPrefix(fields[1], "refs/heads/") && !strings.HasPrefix(fields[1], "refs/tags/")) {
			return nil, false
		}
		if prior := refs[fields[1]]; prior != "" && prior != fields[0] {
			return nil, false
		}
		refs[fields[1]] = fields[0]
	}
	return refs, true
}

func hasMeaningfulIgnoredFiles(output []byte) bool {
	for _, rawPath := range strings.Split(string(output), "\x00") {
		path := filepath.ToSlash(strings.TrimPrefix(rawPath, "./"))
		if path == "" {
			continue
		}
		top, _, descendant := strings.Cut(path, "/")
		if !descendant || !harnessstate.IsStateDir(top) {
			return true
		}
	}
	return false
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
