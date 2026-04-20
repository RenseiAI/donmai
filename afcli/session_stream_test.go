package afcli

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/spf13/cobra"

	"github.com/RenseiAI/agentfactory-tui/afclient"
)

// streamStubDataSource is a DataSource stub specifically for session stream
// tests. It embeds stubDataSource (from agent_test.go) and overrides
// GetActivities to return a controlled sequence of responses across calls.
// A synchronous sync.Mutex protects the call counter and allows the test to
// inspect each invocation's afterCursor argument in order.
type streamStubDataSource struct {
	stubDataSource

	mu sync.Mutex

	// responses is the ordered list of (response, error) pairs returned
	// on successive GetActivities calls. When calls exceed len(responses),
	// the final element is returned repeatedly (so tests can simulate
	// "stays in terminal state" or "keeps working indefinitely").
	responses []streamStubResp

	// cursors records the afterCursor value received on each call. A nil
	// argument is recorded as an empty string to disambiguate, along with
	// a parallel seenNil slice for explicit assertions on the first call.
	cursors []string
	seenNil []bool

	// hangCh, when non-nil, makes GetActivities block until the channel
	// receives or is closed. Used to simulate long-running polls that must
	// be interrupted by context cancellation.
	hangCh chan struct{}
}

type streamStubResp struct {
	resp *afclient.ActivityListResponse
	err  error
}

func (s *streamStubDataSource) GetActivities(_ string, afterCursor *string) (*afclient.ActivityListResponse, error) {
	s.mu.Lock()
	if afterCursor == nil {
		s.cursors = append(s.cursors, "")
		s.seenNil = append(s.seenNil, true)
	} else {
		s.cursors = append(s.cursors, *afterCursor)
		s.seenNil = append(s.seenNil, false)
	}
	idx := len(s.cursors) - 1
	var out streamStubResp
	if idx < len(s.responses) {
		out = s.responses[idx]
	} else if len(s.responses) > 0 {
		// Beyond the controlled sequence: return an empty response that
		// preserves the last-known SessionStatus. This lets tests exit
		// naturally when the final scripted response is terminal.
		last := s.responses[len(s.responses)-1]
		if last.err != nil {
			out = last
		} else {
			var status afclient.SessionStatus
			if last.resp != nil {
				status = last.resp.SessionStatus
			}
			out = streamStubResp{resp: &afclient.ActivityListResponse{
				Activities:    nil,
				SessionStatus: status,
			}}
		}
	}
	hang := s.hangCh
	s.mu.Unlock()

	if hang != nil {
		<-hang
	}
	return out.resp, out.err
}

// newStreamCmd builds a fresh session-stream command wired to stub. Args are
// applied and buffered output is returned for assertion.
func newStreamCmd(stub afclient.DataSource, args []string) (*cobra.Command, *bytes.Buffer) {
	ds := func() afclient.DataSource { return stub }
	cmd := newSessionStreamCmd(ds)
	buf := &bytes.Buffer{}
	cmd.SetOut(buf)
	cmd.SetErr(buf)
	cmd.SetArgs(args)
	return cmd, buf
}

func TestSessionStreamPrintsHistoricalByDefault(t *testing.T) {
	t.Parallel()

	stub := &streamStubDataSource{
		responses: []streamStubResp{
			{
				resp: &afclient.ActivityListResponse{
					Activities: []afclient.ActivityEvent{
						{ID: "1", Type: afclient.ActivityThought, Content: "first", Timestamp: "t1"},
						{ID: "2", Type: afclient.ActivityAction, Content: "second", Timestamp: "t2"},
						{ID: "3", Type: afclient.ActivityResponse, Content: "third", Timestamp: "t3"},
					},
					SessionStatus: afclient.StatusCompleted,
				},
			},
		},
	}

	cmd, buf := newStreamCmd(stub, []string{"sess-x", "--interval=10ms"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("execute: %v", err)
	}

	out := buf.String()
	for _, want := range []string{"first", "second", "third"} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q; got:\n%s", want, out)
		}
	}
}

func TestSessionStreamFollowOnlySkipsHistorical(t *testing.T) {
	t.Parallel()

	stub := &streamStubDataSource{
		responses: []streamStubResp{
			{
				resp: &afclient.ActivityListResponse{
					Activities: []afclient.ActivityEvent{
						{ID: "1", Type: afclient.ActivityThought, Content: "hist-one", Timestamp: "t1"},
						{ID: "2", Type: afclient.ActivityAction, Content: "hist-two", Timestamp: "t2"},
						{ID: "3", Type: afclient.ActivityResponse, Content: "hist-three", Timestamp: "t3"},
					},
					SessionStatus: afclient.StatusWorking,
				},
			},
			{
				resp: &afclient.ActivityListResponse{
					Activities: []afclient.ActivityEvent{
						{ID: "4", Type: afclient.ActivityThought, Content: "new-one", Timestamp: "t4"},
						{ID: "5", Type: afclient.ActivityResponse, Content: "new-two", Timestamp: "t5"},
					},
					SessionStatus: afclient.StatusCompleted,
				},
			},
		},
	}

	cmd, buf := newStreamCmd(stub, []string{"sess-x", "--interval=10ms", "--follow-only"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("execute: %v", err)
	}

	out := buf.String()
	for _, reject := range []string{"hist-one", "hist-two", "hist-three"} {
		if strings.Contains(out, reject) {
			t.Errorf("--follow-only output should not contain %q; got:\n%s", reject, out)
		}
	}
	for _, want := range []string{"new-one", "new-two"} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q; got:\n%s", want, out)
		}
	}
}

func TestSessionStreamJSONMode(t *testing.T) {
	t.Parallel()

	events := []afclient.ActivityEvent{
		{ID: "1", Type: afclient.ActivityThought, Content: "alpha", Timestamp: "t1"},
		{ID: "2", Type: afclient.ActivityAction, Content: "beta", Timestamp: "t2"},
	}
	stub := &streamStubDataSource{
		responses: []streamStubResp{
			{
				resp: &afclient.ActivityListResponse{
					Activities:    events,
					SessionStatus: afclient.StatusCompleted,
				},
			},
		},
	}

	cmd, buf := newStreamCmd(stub, []string{"sess-x", "--interval=10ms", "--json"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("execute: %v", err)
	}

	var lines []string
	scanner := bufio.NewScanner(buf)
	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			continue
		}
		lines = append(lines, line)
	}
	if err := scanner.Err(); err != nil {
		t.Fatalf("scan: %v", err)
	}

	if got, want := len(lines), len(events); got != want {
		t.Fatalf("ndjson line count = %d, want %d; output:\n%s", got, want, buf.String())
	}

	for i, line := range lines {
		var got afclient.ActivityEvent
		if err := json.Unmarshal([]byte(line), &got); err != nil {
			t.Fatalf("line %d not valid JSON: %v; line: %q", i, err, line)
		}
		if got.ID != events[i].ID || got.Content != events[i].Content {
			t.Errorf("line %d decoded = %+v, want %+v", i, got, events[i])
		}
	}
}

func TestSessionStreamCursorAdvances(t *testing.T) {
	t.Parallel()

	stub := &streamStubDataSource{
		responses: []streamStubResp{
			{
				resp: &afclient.ActivityListResponse{
					Activities: []afclient.ActivityEvent{
						{ID: "1", Type: afclient.ActivityThought, Content: "a", Timestamp: "t1"},
						{ID: "2", Type: afclient.ActivityAction, Content: "b", Timestamp: "t2"},
					},
					SessionStatus: afclient.StatusWorking,
				},
			},
			{
				resp: &afclient.ActivityListResponse{
					Activities: []afclient.ActivityEvent{
						{ID: "3", Type: afclient.ActivityThought, Content: "c", Timestamp: "t3"},
					},
					SessionStatus: afclient.StatusCompleted,
				},
			},
		},
	}

	cmd, _ := newStreamCmd(stub, []string{"sess-x", "--interval=10ms"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("execute: %v", err)
	}

	stub.mu.Lock()
	defer stub.mu.Unlock()

	if len(stub.seenNil) < 2 {
		t.Fatalf("expected at least 2 GetActivities calls, got %d", len(stub.seenNil))
	}
	if !stub.seenNil[0] {
		t.Errorf("first call afterCursor should be nil, got %q", stub.cursors[0])
	}
	if stub.seenNil[1] {
		t.Errorf("second call afterCursor should be non-nil")
	}
	if stub.cursors[1] != "2" {
		t.Errorf("second call afterCursor = %q, want %q", stub.cursors[1], "2")
	}
}

func TestSessionStreamTerminatesOnCompleted(t *testing.T) {
	t.Parallel()

	stub := &streamStubDataSource{
		responses: []streamStubResp{
			{
				resp: &afclient.ActivityListResponse{
					Activities:    nil,
					SessionStatus: afclient.StatusCompleted,
				},
			},
		},
	}

	done := make(chan error, 1)
	cmd, _ := newStreamCmd(stub, []string{"sess-x", "--interval=10ms"})
	go func() { done <- cmd.Execute() }()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("execute: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("stream did not terminate within 2s on completed status")
	}
}

func TestSessionStreamContextCancellation(t *testing.T) {
	t.Parallel()

	// Infinite working-status responses with empty activities so the loop
	// would run forever if context cancellation didn't work.
	stub := &streamStubDataSource{
		responses: []streamStubResp{
			{
				resp: &afclient.ActivityListResponse{
					Activities:    nil,
					SessionStatus: afclient.StatusWorking,
				},
			},
		},
	}

	ds := func() afclient.DataSource { return stub }
	cmd := newSessionStreamCmd(ds)
	buf := &bytes.Buffer{}
	cmd.SetOut(buf)
	cmd.SetErr(buf)
	cmd.SetArgs([]string{"sess-x", "--interval=50ms"})

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancel before execute
	cmd.SetContext(ctx)

	done := make(chan error, 1)
	go func() { done <- cmd.Execute() }()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("expected nil error on context cancel, got %v", err)
		}
	case <-time.After(1 * time.Second):
		t.Fatal("stream did not exit within 1s of context cancellation")
	}
}

func TestSessionStreamNotFoundWraps(t *testing.T) {
	t.Parallel()

	stub := &streamStubDataSource{
		responses: []streamStubResp{
			{err: fmt.Errorf("lookup: %w", afclient.ErrNotFound)},
		},
	}

	cmd, _ := newStreamCmd(stub, []string{"missing-id", "--interval=10ms"})
	err := cmd.Execute()
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !errors.Is(err, afclient.ErrNotFound) {
		t.Errorf("errors.Is(err, ErrNotFound) = false; err: %v", err)
	}
	if !strings.Contains(err.Error(), "session missing-id") {
		t.Errorf("error message should contain 'session missing-id'; got: %v", err)
	}
}

func TestSessionStreamNoHotLoop(t *testing.T) {
	t.Parallel()

	const (
		interval     = 50 * time.Millisecond
		workingPolls = 3
	)

	// workingPolls empty WORKING responses, then COMPLETED.
	responses := make([]streamStubResp, 0, workingPolls+1)
	for i := 0; i < workingPolls; i++ {
		responses = append(responses, streamStubResp{
			resp: &afclient.ActivityListResponse{
				Activities:    nil,
				SessionStatus: afclient.StatusWorking,
			},
		})
	}
	responses = append(responses, streamStubResp{
		resp: &afclient.ActivityListResponse{
			Activities:    nil,
			SessionStatus: afclient.StatusCompleted,
		},
	})

	stub := &streamStubDataSource{responses: responses}

	cmd, _ := newStreamCmd(stub, []string{"sess-x", fmt.Sprintf("--interval=%s", interval)})

	start := time.Now()
	if err := cmd.Execute(); err != nil {
		t.Fatalf("execute: %v", err)
	}
	elapsed := time.Since(start)

	// We make (workingPolls+1) polls and sleep interval between each
	// *subsequent* poll. There are workingPolls sleeps before the final
	// terminating response, so elapsed should be >= workingPolls*interval.
	// Allow some jitter by requiring >= (workingPolls-1)*interval.
	minElapsed := time.Duration(workingPolls-1) * interval
	if elapsed < minElapsed {
		t.Errorf("loop appears to hot-loop: elapsed %v < minimum %v", elapsed, minElapsed)
	}
}
