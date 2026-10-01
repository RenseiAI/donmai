package daemon

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/RenseiAI/donmai/executioncell"
	"github.com/RenseiAI/donmai/internal/localqueue"
	"github.com/RenseiAI/donmai/internal/localruntimeauth"
)

// LocalIntakeRequest is authored issue context, never execution authority.
// The trusted compiler chooses all runtime/profile/credential and callback data.
type LocalIntakeRequest struct {
	RepositoryID int64  `json:"repositoryId"`
	OwnerRepo    string `json:"ownerRepo"`
	IssueNumber  int    `json:"issueNumber"`
	IssueURL     string `json:"issueUrl"`
	Title        string `json:"title"`
	Body         string `json:"body"`
}

// LocalIntakeResult names the durable session, including duplicate intake.
type LocalIntakeResult struct {
	Status  string                   `json:"status"`
	Session executioncell.SessionRef `json:"session"`
}

// LocalGitHubRepository is trusted operator source configuration. RepositoryID
// is the immutable GitHub numeric identity, independently read back by a source.
type LocalGitHubRepository struct {
	RepositoryID int64  `json:"repositoryId" yaml:"repositoryId"`
	OwnerRepo    string `json:"ownerRepo" yaml:"ownerRepo"`
	Label        string `json:"label" yaml:"label"`
	Ref          string `json:"ref,omitempty" yaml:"ref,omitempty"`
}

// LocalPublicationTask is a durable outbox projection, not an authorization
// bearer. Source contains only issue correlation (empty Title and Body).
type LocalPublicationTask struct {
	Key            string
	Kind           string
	SessionID      string
	Source         LocalIntakeRequest
	Status         string
	ResultSHA256   string
	PullRequestURL string
	CommitSHA      string
	Branch         string
	BaseRef        string
}

// LocalPublicationResult is independently observed external publication.
// Matching an agent-reported URL/head without readback does not satisfy it.
type LocalPublicationResult struct {
	URL     string
	HeadSHA string
	ReadAt  string
}

// LocalRuntimeSource is a trusted, configured issue/publication adapter.
// Eligibility is a point-in-time observation, never an atomic remote claim.
// Repeated Poll and Publish calls must preserve durable source/outbox identity.
type LocalRuntimeSource interface {
	Poll(context.Context) ([]LocalIntakeRequest, error)
	Eligible(context.Context, LocalIntakeRequest) (bool, error)
	Publish(context.Context, LocalPublicationTask) (LocalPublicationResult, error)
}

// LocalRuntimeIdentity is the persisted local authority, not tracker identity.
type LocalRuntimeIdentity struct{ ScopeID, HostID, WorkerID string }

// LocalCompileRequest combines authored context with controller-minted IDs.
// Only trusted in-process composition constructs these IDs.
type LocalCompileRequest struct {
	Source      LocalIntakeRequest
	SessionID   string
	ReceiptID   string
	ChallengeID string
}

// LocalAdmissionEvidence retains existing closed execution contracts exactly.
// It is produced by a shipped compiler, never accepted from intake JSON.
type LocalAdmissionEvidence struct {
	Session            executioncell.SessionRef
	Intent             executioncell.DispatchIntent
	Receipt            json.RawMessage
	EffectiveCell      json.RawMessage
	RuntimeBinding     json.RawMessage
	OperationalPayload json.RawMessage
	HostPreflight      json.RawMessage
	ProducerEvidence   json.RawMessage
}

// LocalRuntimeCompiler supplies actual registry/catalog/preflight evidence.
// Revalidate observes current runtime/configuration before execution without
// replacing the immutable admission or granting online model entitlement.
// Compile and Revalidate participate in the applied-policy read lease. Implementations
// must not synchronously publish policy or reenter this runtime's admission.
type LocalRuntimeCompiler interface {
	Selectors() *executioncell.ExecutionSelectorRegistry
	Compile(context.Context, LocalCompileRequest) (LocalAdmissionEvidence, error)
	Revalidate(context.Context, LocalAdmissionEvidence) error
}

// LocalRuntimeOptions is trusted process composition for a file queue.
// Roots contain protected local state, not credentials in daemon config JSON.
// Source eligibility and pre-spawn callbacks must not synchronously publish
// this runtime's policy or reenter admission while a launch lease is held.
type LocalRuntimeOptions struct {
	QueueRoot    string
	AuthRoot     string
	Repositories []LocalGitHubRepository
	NewCompiler  func(LocalRuntimeIdentity) (LocalRuntimeCompiler, error)
	Source       LocalRuntimeSource
	PollInterval time.Duration
	// NewSource reconstructs trusted source policy after an operator config edit.
	// Daemon never selects an adapter from incoming issue data.
	NewSource func([]LocalGitHubRepository) (LocalRuntimeSource, error)
}

const localRuntimePrefix = "/api/daemon/local"

var localRepositoryPattern = regexp.MustCompile(`^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$`)

// localLaunchPermit is an in-process capability owned by one dispatch while
// its applied-policy read lease remains held. It is never serialized.
type localLaunchPermit struct{ attempt localqueue.AttemptRef }

type localRuntime struct {
	daemon               *Daemon
	options              LocalRuntimeOptions
	identity             LocalRuntimeIdentity
	workerCommand        []string
	workerArtifactDigest string
	store                *localqueue.Store
	auth                 *localruntimeauth.Store
	// ops serializes local transactions but is never held across Spawn: the
	// owned child must be able to fetch detail and report an early terminal result.
	// Lock order is sourceMu -> policyMu -> daemon.mu. Applied configuration
	// publication holds policyMu exclusively, but releases it before callbacks.
	policyMu      sync.RWMutex
	launchPermits map[string]*localLaunchPermit // guarded by mu
	ops           sync.Mutex
	mu            sync.Mutex
	sourceMu      sync.RWMutex
	runContext    context.Context
	closeDone     chan struct{}
	closeErr      error
	// observationSync is a per-runtime durability fault seam; nil uses fsync.
	observationSync func(*os.Root) error
	source          LocalRuntimeSource
	repositories    []LocalGitHubRepository
	endpoint        string
	instanceID      string
	held            map[string]SessionHandle
	holdReasons     map[string]string
	cancel          context.CancelFunc
	done            chan struct{}
	closed          bool
}

func localQueueRoot(raw string) (string, error) {
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Scheme != "file" || parsed.Host != "" || parsed.User != nil || parsed.Opaque != "" || parsed.RawQuery != "" || parsed.ForceQuery || parsed.Fragment != "" || parsed.RawPath != "" || !filepath.IsAbs(parsed.Path) || filepath.Clean(parsed.Path) != parsed.Path {
		return "", errors.New("local runtime requires a canonical absolute file queue URL")
	}
	return parsed.Path, nil
}

func localRuntimeRequested(cfg *Config) bool {
	return cfg != nil && strings.HasPrefix(strings.ToLower(cfg.Orchestrator.URL), "file:")
}

func validateLocalRepositories(repositories []LocalGitHubRepository) error {
	seen := map[int64]bool{}
	for _, repository := range repositories {
		parts := strings.Split(repository.OwnerRepo, "/")
		if repository.RepositoryID <= 0 || !localRepositoryPattern.MatchString(repository.OwnerRepo) || len(parts) != 2 || parts[0] == "." || parts[0] == ".." || parts[1] == "." || parts[1] == ".." || strings.TrimSpace(repository.Label) == "" || seen[repository.RepositoryID] {
			return errors.New("local runtime requires distinct GitHub repository IDs, owner/repo and labels")
		}
		seen[repository.RepositoryID] = true
	}
	return nil
}

func newLocalRuntime(ctx context.Context, d *Daemon, cfg *Config) (*localRuntime, error) {
	if err := ValidateLocalExecutionSecurity(cfg); err != nil {
		return nil, err
	}
	if d.opts.LocalRuntime == nil || d.opts.LocalRuntime.NewCompiler == nil {
		return nil, errors.New("file queue requires the shipped local runtime compiler and protected receiver composition")
	}
	queueRoot, err := localQueueRoot(cfg.Orchestrator.URL)
	if err != nil {
		return nil, err
	}
	options := *d.opts.LocalRuntime
	if options.QueueRoot == "" {
		options.QueueRoot = queueRoot
	}
	if options.QueueRoot != queueRoot {
		return nil, errors.New("local runtime queue root disagrees with configured file URL")
	}
	if options.AuthRoot == "" {
		options.AuthRoot = queueRoot + ".auth"
	}
	if options.PollInterval <= 0 {
		options.PollInterval = 5 * time.Second
	}
	options.Repositories = slices.Clone(options.Repositories)
	if cfg.LocalRuntime != nil {
		options.Repositories = slices.Clone(cfg.LocalRuntime.Repositories)
	}
	if err = validateLocalRepositories(options.Repositories); err != nil {
		return nil, err
	}
	auth, err := localruntimeauth.Bootstrap(options.AuthRoot, options.QueueRoot)
	if err != nil {
		return nil, fmt.Errorf("open protected local authority: %w", err)
	}
	fail := func(cause error) (*localRuntime, error) { _ = auth.Close(); return nil, cause }
	authority := auth.Identity()
	identity := LocalRuntimeIdentity{ScopeID: authority.ScopeID, HostID: authority.HostID, WorkerID: authority.WorkerID}
	compiler, err := options.NewCompiler(identity)
	if err == nil && compiler == nil {
		err = errors.New("local compiler factory returned no compiler")
	}
	if err != nil {
		return fail(err)
	}
	defer func() { _ = closeLocalCompiler(compiler) }()
	selectors := compiler.Selectors()
	store, err := localqueue.Open(options.QueueRoot, localqueue.ConfiguredHostBinding{ScopeID: identity.ScopeID, HostID: identity.HostID, WorkerID: identity.WorkerID, Selectors: selectors})
	if err != nil {
		return fail(err)
	}
	if err = store.UpdateSelectors(ctx, selectors); err != nil {
		_ = store.Close()
		return fail(err)
	}
	instanceID, err := newLocalReference("daemon_")
	if err != nil {
		_ = store.Close()
		return fail(err)
	}
	runtime := &localRuntime{daemon: d, options: options, identity: identity, store: store, auth: auth, instanceID: instanceID, source: options.Source, repositories: slices.Clone(options.Repositories), held: map[string]SessionHandle{}, holdReasons: map[string]string{}}
	if options.NewSource != nil {
		runtime.source, err = options.NewSource(slices.Clone(options.Repositories))
		if err != nil {
			_ = store.Close()
			return fail(err)
		}
	}
	if runtime.source == nil {
		_ = store.Close()
		return fail(errors.New("file queue requires a configured local issue source and publication adapter"))
	}
	if err = runtime.recover(ctx); err != nil {
		_ = store.Close()
		return fail(err)
	}
	return runtime, nil
}

func newLocalReference(prefix string) (string, error) {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", fmt.Errorf("mint local reference: %w", err)
	}
	return prefix + hex.EncodeToString(raw[:]), nil
}

func localSource(request LocalIntakeRequest) localqueue.GitHubSource {
	return localqueue.GitHubSource{RepositoryID: request.RepositoryID, OwnerRepo: request.OwnerRepo, IssueNumber: request.IssueNumber, IssueURL: request.IssueURL}
}

func (l *localRuntime) validateIntake(request LocalIntakeRequest) error {
	if request.IssueNumber <= 0 || len(request.Title) > 8192 || len(request.Body) > 1<<20 || strings.TrimSpace(request.Title) == "" {
		return errors.New("local intake requires a bounded issue title/body and positive issue number")
	}
	l.mu.Lock()
	repositories := slices.Clone(l.repositories)
	l.mu.Unlock()
	for _, repository := range repositories {
		if repository.RepositoryID == request.RepositoryID && strings.EqualFold(repository.OwnerRepo, request.OwnerRepo) {
			expected := "https://github.com/" + request.OwnerRepo + "/issues/" + strconv.Itoa(request.IssueNumber)
			if request.IssueURL != expected {
				return errors.New("local intake issue URL disagrees with configured source")
			}
			return nil
		}
	}
	return errors.New("local intake source is outside the current configured repositories")
}

func localEvidence(envelope localqueue.AdmissionEnvelope) LocalAdmissionEvidence {
	return LocalAdmissionEvidence{Session: envelope.Session, Intent: envelope.Intent, Receipt: bytes.Clone(envelope.Receipt), EffectiveCell: bytes.Clone(envelope.EffectiveCell), RuntimeBinding: bytes.Clone(envelope.RuntimeBinding), OperationalPayload: bytes.Clone(envelope.OperationalPayload), HostPreflight: bytes.Clone(envelope.HostPreflight), ProducerEvidence: bytes.Clone(envelope.ProducerEvidence)}
}

func (l *localRuntime) admitIntake(ctx context.Context, request LocalIntakeRequest) (LocalIntakeResult, error) {
	l.policyMu.RLock()
	defer l.policyMu.RUnlock()
	if err := l.sourcePolicyCurrent(); err != nil {
		return LocalIntakeResult{}, err
	}
	if err := l.validateIntake(request); err != nil {
		return LocalIntakeResult{}, err
	}
	l.ops.Lock()
	defer l.ops.Unlock()
	existing, err := l.store.SessionForSource(ctx, l.identity.ScopeID, localSource(request))
	if err == nil {
		return LocalIntakeResult{Status: "duplicate", Session: existing.Admission.Envelope.Session}, nil
	}
	if !errors.Is(err, localqueue.ErrUnknownSession) {
		return LocalIntakeResult{}, err
	}
	compiler, err := l.options.NewCompiler(l.identity)
	if err == nil && compiler == nil {
		err = errors.New("local compiler factory returned no compiler")
	}
	if err != nil {
		return LocalIntakeResult{}, err
	}
	defer func() { _ = closeLocalCompiler(compiler) }()
	if err = l.store.UpdateSelectors(ctx, compiler.Selectors()); err != nil {
		return LocalIntakeResult{}, err
	}
	sessionID, err := newLocalReference("session_")
	if err != nil {
		return LocalIntakeResult{}, err
	}
	receiptID, err := newLocalReference("admission_")
	if err != nil {
		return LocalIntakeResult{}, err
	}
	challengeID, err := newLocalReference("challenge_")
	if err != nil {
		return LocalIntakeResult{}, err
	}
	compiled, err := compiler.Compile(ctx, LocalCompileRequest{Source: request, SessionID: sessionID, ReceiptID: receiptID, ChallengeID: challengeID})
	if err != nil {
		return LocalIntakeResult{}, err
	}
	if err := l.validateCurrentAdmissionPolicy(compiled.OperationalPayload, compiled.HostPreflight); err != nil {
		return LocalIntakeResult{}, err
	}
	if compiled.Session.SessionID != sessionID || compiled.Session.AdmissionReceiptID != receiptID {
		return LocalIntakeResult{}, errors.New("local compiler changed controller-owned session identity")
	}
	handle, err := newLocalReference("credential_")
	if err != nil {
		return LocalIntakeResult{}, err
	}
	envelope := localqueue.AdmissionEnvelope{ScopeID: l.identity.ScopeID, Source: localSource(request), Session: compiled.Session, Intent: compiled.Intent, Receipt: compiled.Receipt, EffectiveCell: compiled.EffectiveCell, RuntimeBinding: compiled.RuntimeBinding, OperationalPayload: compiled.OperationalPayload, HostPreflight: compiled.HostPreflight, ProducerEvidence: compiled.ProducerEvidence, CredentialHandle: handle, InitialEvent: localqueue.LifecycleEvent{Type: localqueue.EventSessionAdmitted, ScopeID: l.identity.ScopeID, SessionID: sessionID, ReceiptID: receiptID, RecordedAt: time.Now().UTC().Format(time.RFC3339Nano)}, Dispatch: localqueue.DispatchOutbox{State: localqueue.DispatchPending}}
	record, err := l.store.Admit(ctx, l.store.CurrentRevision(), envelope)
	if err != nil {
		return LocalIntakeResult{}, err
	}
	return LocalIntakeResult{Status: "admitted", Session: record.Envelope.Session}, nil
}

func (l *localRuntime) sourceRequest(envelope localqueue.AdmissionEnvelope) (LocalIntakeRequest, error) {
	request := LocalIntakeRequest{RepositoryID: envelope.Source.RepositoryID, OwnerRepo: envelope.Source.OwnerRepo, IssueNumber: envelope.Source.IssueNumber, IssueURL: envelope.Source.IssueURL}
	var body struct {
		Title string `json:"title"`
		Body  string `json:"body"`
	}
	if err := json.Unmarshal(envelope.OperationalPayload, &body); err != nil {
		return request, err
	}
	request.Title, request.Body = body.Title, body.Body
	return request, nil
}

// sourceLease pins an immutable adapter until its complete operation finishes.
// Replacement/Close wait outside daemon/spawner metadata locks.
func (l *localRuntime) sourceLease() (LocalRuntimeSource, func(), error) {
	l.sourceMu.RLock()
	l.mu.Lock()
	closed := l.closed
	source := l.source
	l.mu.Unlock()
	if closed || source == nil {
		l.sourceMu.RUnlock()
		return nil, nil, errors.New("local source is unavailable")
	}
	if err := l.sourcePolicyCurrent(); err != nil {
		l.sourceMu.RUnlock()
		return nil, nil, err
	}
	return source, l.sourceMu.RUnlock, nil
}

func (l *localRuntime) sourcePolicyCurrent() error {
	if err := l.checkCurrentConfiguration(); err != nil {
		return err
	}
	cfg := l.daemon.Config()
	l.mu.Lock()
	repositories := slices.Clone(l.repositories)
	l.mu.Unlock()
	if cfg != nil && cfg.LocalRuntime != nil && !reflect.DeepEqual(cfg.LocalRuntime.Repositories, repositories) {
		return errors.New("local source configuration is changing; execution held")
	}
	return nil
}

func (l *localRuntime) intake(ctx context.Context, request LocalIntakeRequest) (LocalIntakeResult, error) {
	if state := l.daemon.State(); state == StateStarting || state == StateStopped || state == StateDraining {
		return LocalIntakeResult{}, errors.New("local daemon is not accepting intake")
	}
	l.mu.Lock()
	owner := l.runContext
	l.mu.Unlock()
	if owner == nil {
		return LocalIntakeResult{}, errors.New("local receiver is not ready")
	}
	callCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	stop := context.AfterFunc(owner, cancel)
	defer stop()
	source, release, err := l.sourceLease()
	if err != nil {
		return LocalIntakeResult{}, err
	}
	defer release()
	if err = l.validateIntake(request); err != nil {
		return LocalIntakeResult{}, err
	}
	eligible, err := source.Eligible(callCtx, request)
	if err != nil {
		return LocalIntakeResult{}, err
	}
	if !eligible {
		return LocalIntakeResult{}, errors.New("local issue is no longer eligible")
	}
	if err = l.sourcePolicyCurrent(); err != nil {
		return LocalIntakeResult{}, err
	}
	return l.admitIntake(callCtx, request)
}

func closeLocalSource(source LocalRuntimeSource) error {
	if closer, ok := source.(io.Closer); ok {
		return closer.Close()
	}
	return nil
}

func closeLocalCompiler(compiler LocalRuntimeCompiler) error {
	if closer, ok := compiler.(io.Closer); ok {
		return closer.Close()
	}
	return nil
}
