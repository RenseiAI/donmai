package agent

import "errors"

// Sentinel errors returned by Provider and Handle implementations.
//
// Callers wrap these with fmt.Errorf("context: %w", err) and check
// with errors.Is. Per F.1.1 §2 these are the contract-level errors;
// provider-specific errors live in the provider subpackage.
var (
	// ErrUnsupported is returned when a provider is asked to perform a
	// capability it does not advertise (e.g. Handle.Inject on a
	// provider with SupportsMessageInjection=false, or Provider.Resume
	// on a provider with SupportsSessionResume=false).
	ErrUnsupported = errors.New("agent: capability not supported by provider")

	// ErrNoProvider is returned when the runner cannot resolve a
	// Provider for a requested ProviderName (e.g. an org's profile
	// names a provider not registered with the running binary).
	ErrNoProvider = errors.New("agent: no provider registered for name")

	// ErrSessionNotFound is returned by Provider.Resume when the
	// requested session id is unknown or has already terminated.
	ErrSessionNotFound = errors.New("agent: session id not found")

	// ErrSpawnFailed is returned by Provider.Spawn when the provider
	// could not start the session (CLI missing, app-server
	// unreachable, etc.). Wrap with fmt.Errorf for context.
	ErrSpawnFailed = errors.New("agent: spawn failed")

	// ErrProviderUnavailable is returned by provider constructors
	// (provider.New) when the provider's runtime dependency is missing
	// or not reachable (e.g. `claude` binary not on PATH, codex
	// app-server failed to start). It is a probe-time error distinct
	// from ErrSpawnFailed which is per-session.
	ErrProviderUnavailable = errors.New("agent: provider runtime unavailable")
)
