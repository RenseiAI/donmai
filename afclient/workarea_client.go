// Package afclient workarea_client.go — DaemonClient methods for the
// /api/daemon/workareas* surface. Wave-9 Phase 2 ships canonical method
// signatures stubbed with ErrUnimplemented; Track A3 fills in the HTTP
// implementation including the streaming-NDJSON diff variant. Sub-agents
// must replace ErrUnimplemented with the real c.get/c.post call to the
// route specified in each method's doc comment.
//
// Note for A3: the diff method's signature accepts an io.Writer-shaped
// callback so consumers can stream NDJSON entries without buffering. The
// stub signature lands here; the implementation lives in the same file
// once A3 lands.
package afclient

import "fmt"

// ListWorkareas fetches the daemon's active pool members and on-disk
// archives from GET /api/daemon/workareas. The response splits the two
// kinds into Active and Archived arrays per ADR D4a.
func (c *DaemonClient) ListWorkareas() (*ListWorkareasResponse, error) {
	return nil, fmt.Errorf("ListWorkareas: %w", ErrUnimplemented)
}

// GetWorkarea fetches a single workarea by id from
// GET /api/daemon/workareas/<id>. The id may be either an active pool
// member id or an archive id; the response Workarea.Kind disambiguates.
// Returns ErrNotFound when the id is not registered.
func (c *DaemonClient) GetWorkarea(_ string) (*WorkareaEnvelope, error) {
	return nil, fmt.Errorf("GetWorkarea: %w", ErrUnimplemented)
}

// RestoreWorkarea materialises the named archive into a fresh active
// pool member via POST /api/daemon/workareas/<archiveID>/restore. The
// new id in the response is distinct from the archive id; archives are
// immutable per ADR D4a.
//
// Server responses worth distinguishing in client error wrapping:
//   - 409: IntoSessionID conflict — session id already in use.
//   - 503: pool saturation — Retry-After header names the wait.
//   - 400: archive corrupted or unreadable.
func (c *DaemonClient) RestoreWorkarea(_ string, _ WorkareaRestoreRequest) (*WorkareaRestoreResult, error) {
	return nil, fmt.Errorf("RestoreWorkarea: %w", ErrUnimplemented)
}

// DiffWorkareas returns a structured per-path delta between two archived
// workareas via GET /api/daemon/workareas/<idA>/diff/<idB>. Both ids
// MUST resolve to archives (diffing live members is out of scope per
// ADR D4a).
//
// Implementation note (A3): the daemon switches between a single JSON
// envelope and NDJSON streaming based on entry count. This client
// method consumes both shapes into the same WorkareaDiffResult — Track
// A3 implements Content-Type discrimination and accumulates streamed
// entries into Result.Entries.
func (c *DaemonClient) DiffWorkareas(_ string, _ string) (*WorkareaDiffResult, error) {
	return nil, fmt.Errorf("DiffWorkareas: %w", ErrUnimplemented)
}
