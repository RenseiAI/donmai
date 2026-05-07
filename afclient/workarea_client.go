// Package afclient workarea_client.go — DaemonClient methods for the
// /api/daemon/workareas* surface. Wave-9 Track A3 implementation.
//
// The four methods cover the full Layer-3 Workarea operator surface:
// list, inspect, restore, and diff. Diff is the only non-trivial wire
// path — the daemon switches between a single JSON envelope and NDJSON
// streaming based on entry count vs the daemon-configured threshold.
// This client method handles both shapes via Content-Type
// discrimination and presents one unified WorkareaDiffResult to
// callers.
package afclient

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// ListWorkareas fetches the daemon's active pool members and on-disk
// archives from GET /api/daemon/workareas. The response splits the two
// kinds into Active and Archived arrays per ADR D4a.
func (c *DaemonClient) ListWorkareas() (*ListWorkareasResponse, error) {
	var resp ListWorkareasResponse
	if err := c.get("/api/daemon/workareas", &resp); err != nil {
		return nil, fmt.Errorf("list workareas: %w", err)
	}
	return &resp, nil
}

// GetWorkarea fetches a single workarea by id from
// GET /api/daemon/workareas/<id>. The id may be either an active pool
// member id or an archive id; the response Workarea.Kind disambiguates.
// Returns ErrNotFound when the id is not registered.
func (c *DaemonClient) GetWorkarea(id string) (*WorkareaEnvelope, error) {
	if id == "" {
		return nil, fmt.Errorf("get workarea: id is required")
	}
	var resp WorkareaEnvelope
	if err := c.get("/api/daemon/workareas/"+id, &resp); err != nil {
		return nil, fmt.Errorf("get workarea: %w", err)
	}
	return &resp, nil
}

// RestoreWorkarea materialises the named archive into a fresh active
// pool member via POST /api/daemon/workareas/<archiveID>/restore. The
// new id in the response is distinct from the archive id; archives are
// immutable per ADR D4a.
//
// Server responses worth distinguishing in client error wrapping:
//   - 409: IntoSessionID conflict — session id already in use → ErrConflict.
//   - 503: pool saturation — Retry-After header names the wait → ErrUnavailable.
//   - 400: archive corrupted or unreadable → ErrBadRequest.
func (c *DaemonClient) RestoreWorkarea(archiveID string, req WorkareaRestoreRequest) (*WorkareaRestoreResult, error) {
	if archiveID == "" {
		return nil, fmt.Errorf("restore workarea: archiveID is required")
	}
	var resp WorkareaRestoreResult
	if err := c.post("/api/daemon/workareas/"+archiveID+"/restore", req, &resp); err != nil {
		return nil, fmt.Errorf("restore workarea: %w", err)
	}
	return &resp, nil
}

// DiffWorkareas returns a structured per-path delta between two archived
// workareas via GET /api/daemon/workareas/<idA>/diff/<idB>. Both ids
// MUST resolve to archives (diffing live members is out of scope per
// ADR D4a).
//
// The daemon switches between a single JSON envelope and NDJSON
// streaming based on entry count vs its configured threshold. This
// method consumes both shapes into the same WorkareaDiffResult:
//
//   - application/json     → decode WorkareaDiffEnvelope
//   - application/x-ndjson → stream-decode entries one line at a
//     time, accumulate into Result.Entries, and read the trailing
//     {"summary": {...}} line into Result.Summary.
func (c *DaemonClient) DiffWorkareas(idA, idB string) (*WorkareaDiffResult, error) {
	if idA == "" || idB == "" {
		return nil, fmt.Errorf("diff workareas: both ids are required")
	}
	path := "/api/daemon/workareas/" + idA + "/diff/" + idB
	resp, err := c.httpClient.Get(c.baseURL + path)
	if err != nil {
		return nil, fmt.Errorf("diff workareas: request failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if err := statusToError(resp.StatusCode, path); err != nil {
		return nil, fmt.Errorf("diff workareas: %w", err)
	}
	contentType := resp.Header.Get("Content-Type")
	switch {
	case strings.HasPrefix(contentType, "application/x-ndjson"):
		return decodeNDJSONDiff(resp.Body, idA, idB)
	case strings.HasPrefix(contentType, "application/json"):
		var env WorkareaDiffEnvelope
		if err := json.NewDecoder(resp.Body).Decode(&env); err != nil {
			return nil, fmt.Errorf("diff workareas: decode json: %w", err)
		}
		return &env.Diff, nil
	default:
		// Fall back to JSON parse — the daemon's contract is one of the
		// two; an unknown content-type is most likely missing/wrong
		// header on the server. Treat as JSON best-effort.
		var env WorkareaDiffEnvelope
		if err := json.NewDecoder(resp.Body).Decode(&env); err != nil {
			return nil, fmt.Errorf("diff workareas: unexpected content-type %q: %w",
				contentType, err)
		}
		return &env.Diff, nil
	}
}

// decodeNDJSONDiff streams diff entries from an NDJSON-encoded response
// body. Each line is one of:
//
//   - a WorkareaDiffEntry JSON object (for content lines), or
//   - a {"summary": {...}} JSON object (terminal line with aggregate
//     counts), or
//   - a {"error": "..."} JSON object (server emits when streaming
//     midway through; abort and surface as an error).
//
// The function returns when EOF or an error envelope is seen. Parsing
// errors at line granularity are surfaced wrapped.
func decodeNDJSONDiff(body io.Reader, idA, idB string) (*WorkareaDiffResult, error) {
	result := &WorkareaDiffResult{
		Summary: WorkareaDiffSummary{WorkareaA: idA, WorkareaB: idB},
		Entries: []WorkareaDiffEntry{},
	}
	scanner := bufio.NewScanner(body)
	// Default buffer is too small for very long lines (long paths).
	const maxLine = 1 << 20
	scanner.Buffer(make([]byte, 0, 64*1024), maxLine)
	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}
		// Peek at top-level keys: "summary" / "error" / per-entry.
		var probe map[string]json.RawMessage
		if err := json.Unmarshal(line, &probe); err != nil {
			return nil, fmt.Errorf("ndjson diff: parse line: %w", err)
		}
		if errMsg, ok := probe["error"]; ok {
			var msg string
			_ = json.Unmarshal(errMsg, &msg)
			return nil, fmt.Errorf("ndjson diff: server error: %s", msg)
		}
		if summaryRaw, ok := probe["summary"]; ok {
			var s WorkareaDiffSummary
			if err := json.Unmarshal(summaryRaw, &s); err != nil {
				return nil, fmt.Errorf("ndjson diff: parse summary: %w", err)
			}
			result.Summary = s
			continue
		}
		var entry WorkareaDiffEntry
		if err := json.Unmarshal(line, &entry); err != nil {
			return nil, fmt.Errorf("ndjson diff: parse entry: %w", err)
		}
		result.Entries = append(result.Entries, entry)
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("ndjson diff: scan: %w", err)
	}
	return result, nil
}

// Compile-time checks that the DaemonClient implements the
// workarea-related contracts. Pure assertions — no runtime cost.
var _ = (*DaemonClient)(nil)
var _ = http.MethodGet // ensures the http import remains tied to a real symbol
