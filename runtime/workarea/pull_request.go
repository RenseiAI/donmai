package workarea

import (
	"errors"
	"fmt"
	"net/url"
	"strings"
)

// PullRequestV1 is the optional dispatched-pull-request record carried beside
// the repository on a session's work item. It is present only when the work
// source dispatched the session AGAINST an existing pull request (a review, a
// follow-up commit, a rebase) rather than against a bare branch.
//
// # Wire shape
//
// JSON, camelCase, every field omitempty — absence is the norm and must stay
// byte-compatible with a work source that never emits the object:
//
//	"pullRequest": {
//	  "owner": "…", "repo": "…", "number": 7,
//	  "url": "https://…/pull/7",
//	  "headSha": "…", "headRef": "…",
//	  "baseSha": "…", "baseRef": "…",
//	  "title": "…", "state": "open",
//	  "localRef": "refs/pull/7/head"
//	}
//
// # What the workarea does with it
//
// Owner and Repo bind the record to a repository. Provisioning refuses a
// record whose owner/repo is not the repository the session was dispatched
// against, before it issues any git command: a record that names a different
// repository is either a routing bug or an attempt to pull a head from
// somewhere the session has no business reading, and the answer to both is to
// stop rather than to fetch. Number selects the forge's read-only
// pull-request ref (refs/pull/<n>/head), LocalRef names the ref the agent is
// told to use, HeadSHA is the commit the dispatch was decided against, and
// BaseSHA names the commit a diff is taken from. URL, HeadRef, BaseRef, Title
// and State are context for the prompt; the provisioner reads none of them.
//
// # No token, no API
//
// The record is DATA, not access. It carries no credential and names no API
// endpoint. The runner materializes the head with one `git fetch` over the
// remote the clone already used — the credential stays in the provisioning
// process, never reaches the agent's environment, and nothing in this record
// grants the agent forge API access. An agent that needs to read the pull
// request reads the fetched commits.
//
// # Why the head is verified
//
// A pull request is mutable: its head moves on every push, and a force-push
// rewrites it entirely. HeadSHA freezes the head the dispatch decision was made
// against. Fetching the ref without checking it against HeadSHA would silently
// review whatever the branch happens to point at when provisioning runs —
// producing a review, an approval or a follow-up commit attributed to a head
// nobody chose. The provisioner therefore compares the fetched tip with HeadSHA
// and fails the session, naming both commits, rather than proceeding.
type PullRequestV1 struct {
	// Owner is the forge account or organization owning the repository.
	// Required: with Repo it is checked against the repository the session
	// clones, before any fetch.
	Owner string `json:"owner,omitempty"`
	// Repo is the repository name within Owner. Required; see Owner.
	Repo string `json:"repo,omitempty"`
	// Number is the pull-request number. Required; it selects the
	// refs/pull/<Number>/head ref the provisioner fetches.
	Number int `json:"number,omitempty"`
	// URL is the human-facing pull-request page. Context only.
	URL string `json:"url,omitempty"`
	// HeadSHA is the head commit the dispatch decision was made against.
	// Required; the provisioner refuses any other tip.
	HeadSHA string `json:"headSha,omitempty"`
	// HeadRef is the head branch name on the contributing fork or branch.
	// Context only — the fetch goes through refs/pull/<Number>/head, which is
	// readable even when the head branch is not.
	HeadRef string `json:"headRef,omitempty"`
	// BaseSHA is the base commit a diff of this pull request is taken from.
	// Optional; when set it must be reachable in the clone once the head has
	// been fetched, and provisioning fails naming it when it is not. There is
	// deliberately no fallback fetch of BaseRef: a base commit the clone
	// cannot see after the head arrived means the base was rewritten since
	// dispatch, and fetching the branch's CURRENT tip instead would answer a
	// question nobody asked while looking like success.
	BaseSHA string `json:"baseSha,omitempty"`
	// BaseRef is the base branch name. Context only — it is reported in the
	// failure reason when BaseSHA is unreachable, never fetched.
	BaseRef string `json:"baseRef,omitempty"`
	// Title is the pull-request title. Context only.
	Title string `json:"title,omitempty"`
	// State is the pull-request state ("open", "closed", "merged"). Context
	// only — provisioning does not refuse a closed pull request, because
	// reviewing or amending one is legitimate work.
	State string `json:"state,omitempty"`
	// LocalRef is the ref name the agent is told to use, e.g.
	// "refs/pull/7/head". Empty defaults to that same conventional name.
	LocalRef string `json:"localRef,omitempty"`
}

// Sentinel errors for the pull-request record. Callers classify with errors.Is.
var (
	// ErrPullRequestInvalid marks a record that cannot be acted on at all: a
	// missing number, a missing or malformed head commit, an unusable local
	// ref. It is deterministic — retrying provisioning cannot fix it.
	ErrPullRequestInvalid = errors.New("workarea: invalid pull request record")

	// ErrPullRequestRepositoryMismatch marks a record bound to a different
	// repository than the one the session clones. It is deterministic and is
	// raised before any git command runs.
	ErrPullRequestRepositoryMismatch = errors.New("workarea: pull request record names a different repository")
)

// RemoteHeadRef is the forge-served, read-only ref that publishes the pull
// request's head commit. It is readable with the base repository's credential
// even when the head lives on a fork whose branch the credential cannot see,
// which is why provisioning fetches this rather than HeadRef.
func (p *PullRequestV1) RemoteHeadRef() string {
	return fmt.Sprintf("refs/pull/%d/head", p.Number)
}

// ResolvedLocalRef is the ref the fetched head is written to, and the ref the
// agent is told to use. An empty LocalRef defaults to the same conventional
// name the forge serves.
func (p *PullRequestV1) ResolvedLocalRef() string {
	if ref := strings.TrimSpace(p.LocalRef); ref != "" {
		return ref
	}
	return p.RemoteHeadRef()
}

// RepositoryIdentity extracts the owner/name pair from a repository as it is
// spelled on the wire, which may be an https URL, an scp-style ssh remote, a
// file:// URL, a local path, or a bare "owner/name" slug. A trailing ".git"
// and any embedded userinfo are discarded. ok is false when no two-segment
// identity can be read out, which callers must treat as "cannot verify", never
// as "matches".
func RepositoryIdentity(repository string) (owner, name string, ok bool) {
	raw := strings.TrimSpace(repository)
	if raw == "" {
		return "", "", false
	}
	path := raw
	switch {
	case strings.Contains(raw, "://"):
		u, err := url.Parse(raw)
		if err != nil {
			return "", "", false
		}
		path = u.Path
	case strings.Contains(raw, ":") && !strings.HasPrefix(raw, "/"):
		// scp-style: [user@]host:owner/name.git
		_, path, _ = strings.Cut(raw, ":")
	}
	path = strings.Trim(path, "/")
	path = strings.TrimSuffix(path, ".git")
	segments := strings.Split(path, "/")
	if len(segments) < 2 {
		return "", "", false
	}
	owner, name = segments[len(segments)-2], segments[len(segments)-1]
	if owner == "" || name == "" {
		return "", "", false
	}
	return owner, name, true
}

// BindsToRepository reports whether this record belongs to the repository the
// session is cloning. Comparison is case-insensitive because forges treat
// owner and repository names that way, so a case difference is not evidence of
// a different repository — but anything else is, and an identity that cannot
// be read out of the repository spelling is refused rather than assumed to
// match.
func (p *PullRequestV1) BindsToRepository(repository string) error {
	wantOwner, wantName, ok := RepositoryIdentity(repository)
	if !ok {
		return fmt.Errorf("%w: cannot read an owner/name identity out of the session repository, so the record naming %s/%s cannot be verified against it",
			ErrPullRequestRepositoryMismatch, p.Owner, p.Repo)
	}
	if !strings.EqualFold(p.Owner, wantOwner) || !strings.EqualFold(p.Repo, wantName) {
		return fmt.Errorf("%w: record names %s/%s but the session clones %s/%s",
			ErrPullRequestRepositoryMismatch, p.Owner, p.Repo, wantOwner, wantName)
	}
	return nil
}

// Validate reports whether the record can drive a provisioning fetch. URL,
// HeadRef, Title and State are never required — a work source that carries
// less context still provisions — but Owner and Repo are, because a record
// that cannot be bound to a repository cannot be proven to belong to this
// session's.
func (p *PullRequestV1) Validate() error {
	if p == nil {
		return nil
	}
	if strings.TrimSpace(p.Owner) == "" || strings.TrimSpace(p.Repo) == "" {
		return fmt.Errorf("%w: owner and repo are required to bind the record to a repository", ErrPullRequestInvalid)
	}
	if p.Number <= 0 {
		return fmt.Errorf("%w: number must be positive, got %d", ErrPullRequestInvalid, p.Number)
	}
	if err := validateCommitID(p.HeadSHA); err != nil {
		return fmt.Errorf("%w: headSha: %w", ErrPullRequestInvalid, err)
	}
	if err := validateFetchRefName(p.ResolvedLocalRef()); err != nil {
		return fmt.Errorf("%w: localRef: %w", ErrPullRequestInvalid, err)
	}
	if p.BaseSHA != "" {
		if err := validateCommitID(p.BaseSHA); err != nil {
			return fmt.Errorf("%w: baseSha: %w", ErrPullRequestInvalid, err)
		}
	}
	return nil
}

// validateCommitID accepts only a full-length lowercase-or-uppercase hex object
// id (SHA-1 or SHA-256). An abbreviation is refused deliberately: the whole
// point of the recorded head is an exact comparison, and a prefix comparison
// would accept a different commit that happens to share the prefix.
func validateCommitID(id string) error {
	if id == "" {
		return errors.New("required")
	}
	if len(id) != 40 && len(id) != 64 {
		return fmt.Errorf("must be a full 40- or 64-character object id, got %d characters", len(id))
	}
	for _, r := range id {
		switch {
		case r >= '0' && r <= '9', r >= 'a' && r <= 'f', r >= 'A' && r <= 'F':
		default:
			return errors.New("must be hexadecimal")
		}
	}
	return nil
}

// validateFetchRefName refuses anything that could turn a refspec into
// something other than one fully-qualified ref: an option (leading dash), a
// path escape, a second refspec component, or any of git's own ref-name
// metacharacters.
func validateFetchRefName(ref string) error {
	if ref == "" {
		return errors.New("required")
	}
	if !strings.HasPrefix(ref, "refs/") {
		return fmt.Errorf("must be fully qualified (refs/…), got %q", ref)
	}
	if strings.HasPrefix(ref, "-") || strings.HasSuffix(ref, "/") || strings.HasSuffix(ref, ".lock") {
		return fmt.Errorf("not a usable ref name: %q", ref)
	}
	if strings.Contains(ref, "..") || strings.Contains(ref, "@{") || strings.Contains(ref, "//") {
		return fmt.Errorf("not a usable ref name: %q", ref)
	}
	for _, bad := range []string{" ", "\t", "\n", "\r", "~", "^", ":", "?", "*", "[", "\\", "\x7f"} {
		if strings.Contains(ref, bad) {
			return fmt.Errorf("not a usable ref name: %q", ref)
		}
	}
	for _, segment := range strings.Split(ref, "/") {
		if segment == "" || strings.HasPrefix(segment, ".") || strings.HasSuffix(segment, ".") {
			return fmt.Errorf("not a usable ref name: %q", ref)
		}
	}
	return nil
}
