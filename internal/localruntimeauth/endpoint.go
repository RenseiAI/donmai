package localruntimeauth

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"strconv"
)

const endpointFileName = "endpoint.json"

// ErrMissingEndpoint means no bound listener origin has been published yet.
var ErrMissingEndpoint = errors.New("localruntimeauth: endpoint is missing")

// Endpoint is non-secret metadata about an actually bound daemon listener.
// It grants neither claim authority nor permission to redirect a live worker.
type Endpoint struct {
	Version         int    `json:"version"`
	ScopeID         string `json:"scopeId"`
	HostID          string `json:"hostId"`
	WorkerID        string `json:"workerId"`
	QueueRootSHA256 string `json:"queueRootSha256"`
	Origin          string `json:"origin"`
	InstanceID      string `json:"instanceId"`
}

func validLoopbackOrigin(origin string) bool {
	parsed, err := url.Parse(origin)
	if err != nil || parsed.Scheme != "http" || parsed.Opaque != "" || parsed.User != nil ||
		parsed.Path != "" || parsed.RawPath != "" || parsed.RawQuery != "" || parsed.ForceQuery ||
		parsed.Fragment != "" || parsed.RawFragment != "" {
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
	canonical := (&url.URL{Scheme: "http", Host: net.JoinHostPort(ip.String(), strconv.Itoa(port))}).String()
	return origin == canonical
}

func (s *Store) readEndpointLocked() (Endpoint, error) {
	if err := s.dirSync(s.root); err != nil {
		return Endpoint{}, fmt.Errorf("reaffirm private endpoint directory: %w", err)
	}
	var endpoint Endpoint
	if err := readRecord(s.root, endpointFileName, &endpoint); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return Endpoint{}, ErrMissingEndpoint
		}
		return Endpoint{}, err
	}
	if endpoint.Version != formatVersion || !validLoopbackOrigin(endpoint.Origin) ||
		!localIDPattern.MatchString(endpoint.InstanceID) || !validDigest(endpoint.QueueRootSHA256) {
		return Endpoint{}, ErrCorruptState
	}
	if endpoint.ScopeID != s.identity.ScopeID || endpoint.HostID != s.identity.HostID ||
		endpoint.WorkerID != s.identity.WorkerID || endpoint.QueueRootSHA256 != s.queueRootSHA256 {
		return Endpoint{}, ErrBindingMismatch
	}
	return endpoint, nil
}

// Endpoint reads only the persisted, bound origin. A stale-but-valid origin
// after restart is not automatically a new worker redirect; the controller
// must reconcile its in-flight senders before choosing a new listener.
func (s *Store) Endpoint() (Endpoint, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.checkOpenLocked(false); err != nil {
		return Endpoint{}, err
	}
	if err := lockReader(s.lockFile); err != nil {
		return Endpoint{}, err
	}
	defer func() { _ = unlockWriter(s.lockFile) }()
	if err := s.ensureBootstrapLocked(); err != nil {
		return Endpoint{}, err
	}
	return s.readEndpointLocked()
}

// PublishEndpoint requires the trusted caller to hold the matching queue
// Store's exclusive writer lease and supply the actual bound listener origin.
// This package cannot manufacture or infer proof of that external lease.
func (s *Store) PublishEndpoint(ctx context.Context, origin, instanceID string) (Endpoint, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.checkOpenLocked(true); err != nil {
		return Endpoint{}, err
	}
	if !validLoopbackOrigin(origin) || !localIDPattern.MatchString(instanceID) {
		return Endpoint{}, ErrInvalidInput
	}
	if err := ctx.Err(); err != nil {
		return Endpoint{}, err
	}
	if err := lockWriter(s.lockFile); err != nil {
		return Endpoint{}, err
	}
	defer func() { _ = unlockWriter(s.lockFile) }()
	if err := s.ensureBootstrapLocked(); err != nil {
		return Endpoint{}, err
	}
	existing, err := s.readEndpointLocked()
	if err == nil && existing.Origin == origin && existing.InstanceID == instanceID {
		return existing, nil
	}
	if err != nil && !errors.Is(err, ErrMissingEndpoint) {
		return Endpoint{}, err
	}
	value := Endpoint{
		Version: formatVersion, ScopeID: s.identity.ScopeID, HostID: s.identity.HostID,
		WorkerID: s.identity.WorkerID, QueueRootSHA256: s.queueRootSHA256,
		Origin: origin, InstanceID: instanceID,
	}
	raw, err := encodeRecord(value)
	if err != nil {
		return Endpoint{}, err
	}
	if err := s.writeEndpointReplace(ctx, raw); err != nil {
		return Endpoint{}, err
	}
	return value, nil
}

func (s *Store) writeEndpointReplace(ctx context.Context, raw []byte) error {
	temp, err := s.writeTemp(s.root, endpointFileName, raw)
	if err != nil {
		return err
	}
	if s.fault != nil {
		if err := s.fault("before_rename"); err != nil {
			_ = s.root.Remove(temp)
			return fmt.Errorf("private endpoint write interrupted before publication: %w", err)
		}
	}
	if err := ctx.Err(); err != nil {
		_ = s.root.Remove(temp)
		return err
	}
	if err := s.root.Rename(temp, endpointFileName); err != nil {
		_ = s.root.Remove(temp)
		return fmt.Errorf("publish private endpoint: %w", err)
	}
	if s.fault != nil {
		if err := s.fault("after_rename"); err != nil {
			s.poisoned = true
			return fmt.Errorf("%w: %v", ErrAmbiguousCommit, err)
		}
	}
	if err := s.dirSync(s.root); err != nil {
		s.poisoned = true
		return fmt.Errorf("%w: sync private endpoint directory: %v", ErrAmbiguousCommit, err)
	}
	return nil
}
