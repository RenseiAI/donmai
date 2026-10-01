// Package localgithub provides GitHub issue intake and publication readback
// for a configured local daemon. It owns no queue or execution authority.
package localgithub

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/RenseiAI/donmai/daemon"
)

var (
	// ErrInvalidConfiguration means trusted source settings are incomplete.
	ErrInvalidConfiguration = errors.New("localgithub: invalid configured source")
	// ErrScopeMismatch means GitHub facts disagree with configured identity.
	ErrScopeMismatch = errors.New("localgithub: GitHub source identity mismatch")
	// ErrRemote means a bounded GitHub request failed or was refused.
	ErrRemote = errors.New("localgithub: GitHub read or write failed")
	// ErrPublicationMismatch means remote evidence conflicts with the outbox.
	ErrPublicationMismatch = errors.New("localgithub: publication readback mismatch")
	// ErrAmbiguousPublication holds a possible comment write for reconciliation.
	ErrAmbiguousPublication = errors.New("localgithub: publication may be pending; reconcile before retry")
	// ErrCorruptPublicationRoot means private intent metadata is invalid.
	ErrCorruptPublicationRoot = errors.New("localgithub: private publication state invalid")
	// ErrClosed means the Source has released its private file handle.
	ErrClosed = errors.New("localgithub: source is closed")
)

const (
	defaultAPIOrigin    = "https://api.github.com"
	maxAPIResponse      = 8 << 20
	maxIssueBody        = 64 << 10
	maxIssueTitle       = 1024
	maxIssuePageCount   = 100
	maxPollItems        = 2000
	maxPollTextBytes    = 8 << 20
	maxCommentPageCount = 100
	itemsPerPage        = 100
)

var (
	ownerRepoPattern = regexp.MustCompile(`^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$`)
	localIDPattern   = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]{0,127}$`)
	shaPattern       = regexp.MustCompile(`^(?:[0-9a-f]{40}|[0-9a-f]{64})$`)
	digestPattern    = regexp.MustCompile(`^[0-9a-f]{64}$`)
)

// Options contains trusted process configuration, not issue-supplied data.
// The caller supplies the existing GitHub credential without logging it.
type Options struct {
	Repositories    []daemon.LocalGitHubRepository
	Token           string
	HTTPClient      *http.Client
	BaseURL         string
	PublicationRoot string
}

func (Options) String() string { return "localgithub.Options{[REDACTED]}" }

// GoString keeps options credentials out of diagnostic formatting.
func (Options) GoString() string { return "localgithub.Options{[REDACTED]}" }

// Format prevents alternate fmt verbs from traversing Token.
func (Options) Format(s fmt.State, _ rune) {
	_, _ = io.WriteString(s, "localgithub.Options{[REDACTED]}")
}

// MarshalJSON emits no configured bearer material.
func (Options) MarshalJSON() ([]byte, error) {
	return []byte(`"[REDACTED]"`), nil
}

// Source implements the daemon's trusted GitHub intake/publication seam.
type Source struct {
	mu           sync.Mutex
	repositories []daemon.LocalGitHubRepository
	byID         map[int64]daemon.LocalGitHubRepository
	baseURL      string
	token        *string
	client       *http.Client
	root         *os.Root
	rootPath     string
	closed       bool
	poisoned     bool
	now          func() time.Time
	fileSync     func(*os.File) error
	dirSync      func(*os.Root) error
	fault        func(string) error
}

func (*Source) String() string { return "localgithub.Source{[REDACTED]}" }

// GoString keeps Source credential material out of diagnostics.
func (*Source) GoString() string { return "localgithub.Source{[REDACTED]}" }

// Format prevents alternate fmt verbs from traversing Source fields.
func (*Source) Format(s fmt.State, _ rune) {
	_, _ = io.WriteString(s, "localgithub.Source{[REDACTED]}")
}

var (
	_ daemon.LocalRuntimeSource = (*Source)(nil)
	_ json.Marshaler            = Options{}
)

// New freezes a set of configured GitHub repositories and opens private
// comment-intent storage. A custom API origin is restricted to injected
// literal-loopback fixtures; production uses api.github.com only.
func New(options Options) (*Source, error) {
	if !validAPIToken(options.Token) ||
		len(options.Repositories) == 0 || len(options.Repositories) > 128 {
		return nil, ErrInvalidConfiguration
	}
	baseURL, client, err := apiTransport(options)
	if err != nil {
		return nil, ErrInvalidConfiguration
	}
	byID := make(map[int64]daemon.LocalGitHubRepository, len(options.Repositories))
	byName := make(map[string]struct{}, len(options.Repositories))
	repositories := make([]daemon.LocalGitHubRepository, 0, len(options.Repositories))
	for _, repository := range options.Repositories {
		if repository.RepositoryID <= 0 || !validOwnerRepo(repository.OwnerRepo) ||
			repository.Label == "" || strings.TrimSpace(repository.Label) != repository.Label ||
			len(repository.Label) > 128 || !validConfiguredRef(repository.Ref) {
			return nil, ErrInvalidConfiguration
		}
		if _, exists := byID[repository.RepositoryID]; exists {
			return nil, ErrInvalidConfiguration
		}
		if _, exists := byName[strings.ToLower(repository.OwnerRepo)]; exists {
			return nil, ErrInvalidConfiguration
		}
		byID[repository.RepositoryID] = repository
		byName[strings.ToLower(repository.OwnerRepo)] = struct{}{}
		repositories = append(repositories, repository)
	}
	if !filepath.IsAbs(options.PublicationRoot) || filepath.Clean(options.PublicationRoot) != options.PublicationRoot {
		return nil, ErrInvalidConfiguration
	}
	if err := ensurePrivateRoot(options.PublicationRoot); err != nil {
		return nil, err
	}
	root, err := os.OpenRoot(options.PublicationRoot)
	if err != nil {
		return nil, fmt.Errorf("open private publication root: %w", err)
	}
	if err := syncDirectory(root); err != nil {
		_ = root.Close()
		return nil, fmt.Errorf("reaffirm private publication root: %w", err)
	}
	token := options.Token
	return &Source{
		repositories: repositories, byID: byID, baseURL: baseURL, token: &token,
		client: client, root: root, rootPath: options.PublicationRoot, now: time.Now,
		fileSync: func(file *os.File) error { return file.Sync() }, dirSync: syncDirectory,
	}, nil
}

func validAPIToken(token string) bool {
	if token == "" || strings.TrimSpace(token) != token {
		return false
	}
	for _, char := range token {
		if char < 0x21 || char == 0x7f {
			return false
		}
	}
	return true
}

// apiTransport is shared by long-lived intake and the one-shot setup read.
// Callers may inject only literal-loopback fixtures; production is fixed to
// the GitHub API origin. Redirects are always refused before bearer forwarding.
func apiTransport(options Options) (string, *http.Client, error) {
	baseURL := options.BaseURL
	if baseURL == "" {
		baseURL = defaultAPIOrigin
	} else if !validLoopbackAPIOrigin(baseURL) || options.HTTPClient == nil {
		return "", nil, ErrInvalidConfiguration
	}
	client := &http.Client{Timeout: 15 * time.Second}
	if options.HTTPClient != nil {
		copyClient := *options.HTTPClient
		client = &copyClient
		if client.Timeout <= 0 {
			client.Timeout = 15 * time.Second
		}
	}
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return errors.New("localgithub: API redirect refused") }
	return baseURL, client, nil
}

func validConfiguredRef(ref string) bool {
	if ref == "" {
		return true
	}
	if len(ref) > 256 || strings.TrimSpace(ref) != ref || strings.Contains(ref, "..") {
		return false
	}
	for _, char := range ref {
		if char < 0x21 || char == 0x7f {
			return false
		}
	}
	return true
}

func validOwnerRepo(value string) bool {
	if !ownerRepoPattern.MatchString(value) {
		return false
	}
	owner, repo, _ := strings.Cut(value, "/")
	return owner != "." && owner != ".." && repo != "." && repo != ".."
}

func validLoopbackAPIOrigin(raw string) bool {
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Scheme != "http" || parsed.Opaque != "" || parsed.User != nil ||
		parsed.Path != "" || parsed.RawPath != "" || parsed.RawQuery != "" || parsed.Fragment != "" {
		return false
	}
	host, portText, err := net.SplitHostPort(parsed.Host)
	if err != nil {
		return false
	}
	ip := net.ParseIP(host)
	port, err := strconv.Atoi(portText)
	if err != nil || ip == nil || !ip.IsLoopback() || port < 1 || port > 65535 {
		return false
	}
	return raw == (&url.URL{Scheme: "http", Host: net.JoinHostPort(ip.String(), strconv.Itoa(port))}).String()
}

func (s *Source) configured(source daemon.LocalIntakeRequest) (daemon.LocalGitHubRepository, bool) {
	repository, ok := s.byID[source.RepositoryID]
	return repository, ok && repository.OwnerRepo == source.OwnerRepo
}

func (s *Source) checkOpen() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return ErrClosed
	}
	if s.poisoned {
		return ErrAmbiguousPublication
	}
	return nil
}

// Close releases private file handles. It never deletes an uncertain intent.
func (s *Source) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil
	}
	s.closed = true
	return s.root.Close()
}
