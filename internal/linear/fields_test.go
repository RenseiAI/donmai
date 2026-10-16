package linear

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"
)

// ─── cheap-default field selection ───────────────────────────────────────────

func TestParseIssueFields(t *testing.T) {
	tests := []struct {
		name    string
		raw     string
		univ    []string
		want    []string
		wantErr string
	}{
		{name: "single field", raw: "title", univ: ListIssueFullFields, want: []string{"title"}},
		{name: "multiple fields keep order", raw: "status,id,title", univ: ListIssueFullFields, want: []string{"status", "id", "title"}},
		{name: "case and whitespace tolerant", raw: " Title , STATUS ", univ: ListIssueFullFields, want: []string{"title", "status"}},
		{name: "duplicates collapse", raw: "id,id,title", univ: ListIssueFullFields, want: []string{"id", "title"}},
		{name: "parent is selectable", raw: "id,parent", univ: ListIssueFullFields, want: []string{"id", "parent"}},
		{name: "get-issue universe", raw: "identifier,parentId", univ: GetIssueFields, want: []string{"identifier", "parentId"}},
		{name: "unknown field fails", raw: "title,nope", univ: ListIssueFullFields, wantErr: `unknown field "nope"`},
		{name: "list universe rejects get-only key", raw: "description", univ: ListIssueFullFields, wantErr: `unknown field "description"`},
		{name: "empty name fails", raw: "title,,status", univ: ListIssueFullFields, wantErr: "empty field name"},
		{name: "blank fails", raw: "  ", univ: ListIssueFullFields, wantErr: "empty field name"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ParseIssueFields(tt.raw, tt.univ)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("err = %v, want %q", err, tt.wantErr)
				}
				if !strings.Contains(err.Error(), "valid fields:") {
					t.Errorf("err = %v, want the valid field list for remediation", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("err = %v", err)
			}
			if strings.Join(got, ",") != strings.Join(tt.want, ",") {
				t.Errorf("fields = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestSlimFieldSetContract(t *testing.T) {
	want := []string{"id", "title", "status", "project", "sortOrder", "parent", "labels"}
	if strings.Join(ListIssueSlimFields, ",") != strings.Join(want, ",") {
		t.Fatalf("slim fields = %v, want %v", ListIssueSlimFields, want)
	}
	slim := make(map[string]bool, len(ListIssueSlimFields))
	for _, field := range ListIssueSlimFields {
		slim[field] = true
	}
	for _, field := range ListIssueFullFields {
		if field == "priority" || field == "assignee" || field == "createdAt" {
			if slim[field] {
				t.Errorf("expensive field %q must not be in the slim default", field)
			}
			continue
		}
		if !slim[field] && field != "priority" && field != "assignee" && field != "createdAt" {
			t.Errorf("slim default dropped full-set field %q", field)
		}
	}
	if !IsSlimIssueFieldSet(ListIssueSlimFields) || IsSlimIssueFieldSet(ListIssueFullFields) {
		t.Error("slim detection disagrees with the declared sets")
	}
}

func TestSelectIssueKeys(t *testing.T) {
	row := map[string]any{
		"id": "ENG-1", "title": "First", "status": "Backlog", "priority": 2,
		"parentId": "parent-1", "parentIdentifier": "ENG-0",
	}
	tests := []struct {
		name   string
		fields []string
		want   map[string]any
	}{
		{name: "subset", fields: []string{"id", "title"}, want: map[string]any{"id": "ENG-1", "title": "First"}},
		{name: "parent expands to both keys", fields: []string{"parent"}, want: map[string]any{"parentId": "parent-1", "parentIdentifier": "ENG-0"}},
		{name: "missing key stays missing", fields: []string{"id", "createdAt"}, want: map[string]any{"id": "ENG-1"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := SelectIssueKeys(row, tt.fields)
			if len(got) != len(tt.want) {
				t.Fatalf("keys = %v, want %v", got, tt.want)
			}
			for key, want := range tt.want {
				if got[key] != want {
					t.Errorf("key %q = %v, want %v", key, got[key], want)
				}
			}
		})
	}
}

// ─── Retry-After equal jitter ─────────────────────────────────────────────────

func TestJitteredRetryDelayBounds(t *testing.T) {
	defer func(restore func(int64) int64) { retryJitterIntn = restore }(retryJitterIntn)
	tests := []struct {
		name   string
		delay  time.Duration
		rand   int64
		lo, hi time.Duration
	}{
		{name: "zero stays zero", delay: 0, rand: 0, lo: 0, hi: 0},
		{name: "tiny delay still jitters its half window", delay: time.Millisecond, rand: 0, lo: 500 * time.Microsecond, hi: 500 * time.Microsecond},
		{name: "minimum draw hits half", delay: 10 * time.Second, rand: 0, lo: 5 * time.Second, hi: 5 * time.Second},
		{name: "maximum draw hits deadline", delay: 10 * time.Second, rand: 5_000_000_001, lo: 10 * time.Second, hi: 10 * time.Second},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			retryJitterIntn = func(int64) int64 { return tt.rand }
			if got := jitteredRetryDelay(tt.delay); got < tt.lo || got > tt.hi {
				t.Errorf("delay = %v, want within [%v, %v]", got, tt.lo, tt.hi)
			}
		})
	}
}

func TestJitteredRetryDelayNeverExceedsServerDeadline(t *testing.T) {
	defer func(restore func(int64) int64) { retryJitterIntn = restore }(retryJitterIntn)
	// Sweep the whole draw range at 1s granularity: no draw may sleep past
	// the server's Retry-After value.
	for _, delay := range []time.Duration{time.Second, 30 * time.Second, time.Minute} {
		half := int64(delay / 2)
		for _, draw := range []int64{0, 1, half / 2, half - 1, half} {
			retryJitterIntn = func(int64) int64 { return draw }
			if got := jitteredRetryDelay(delay); got < delay/2 || got > delay {
				t.Fatalf("delay %v draw %d slept %v, outside [%v, %v]", delay, draw, got, delay/2, delay)
			}
		}
	}
}

// TestRetrySleepsJitteredDelay pins the client wiring: with the jitter seam
// forced to its minimum draw, a 2s Retry-After must sleep ~1s (half), proving
// the retry loop sleeps the jittered value rather than the bare server value.
func TestRetrySleepsJitteredDelay(t *testing.T) {
	defer func(restore func(int64) int64) { retryJitterIntn = restore }(retryJitterIntn)
	retryJitterIntn = func(int64) int64 { return 0 }
	calls := 0
	c, _ := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		calls++
		if calls == 1 {
			w.Header().Set("Retry-After", "2")
			w.WriteHeader(429)
			_, _ = w.Write([]byte(`{"errors":[{"message":"budget exhausted"}]}`))
			return
		}
		writeGQLData(w, `{"teams":{"nodes":[],"pageInfo":{"hasNextPage":false,"endCursor":null}}}`)
	})
	start := time.Now()
	if _, err := c.ListTeams(context.Background()); err != nil {
		t.Fatalf("err = %v", err)
	}
	if calls != 2 {
		t.Fatalf("calls = %d, want 2", calls)
	}
	elapsed := time.Since(start)
	if elapsed < time.Second || elapsed >= 2*time.Second {
		t.Fatalf("retry slept %v, want the minimum-jitter ~1s for a 2s Retry-After", elapsed)
	}
}
