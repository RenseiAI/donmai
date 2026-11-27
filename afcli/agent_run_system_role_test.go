package afcli

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/RenseiAI/donmai/daemon"
	"github.com/RenseiAI/donmai/runner"
)

func sizedSystemRole(t *testing.T, n int) string {
	t.Helper()
	start := "[[ROLEBEGIN-9f3a]]"
	end := "]]-9f3a-ROLEEND-\u2713"
	chunk := "q\u00e9\u2713\n"
	overhead := len(start) + len(end)
	if n <= overhead {
		t.Fatalf("fixture size %d too small", n)
	}
	middleLen := n - overhead
	middle := strings.Repeat(chunk, middleLen/len(chunk)) + strings.Repeat("q", middleLen%len(chunk))
	role := start + middle + end
	if len(role) != n {
		t.Fatalf("fixture is %d bytes, want %d", len(role), n)
	}
	return role
}

func fetchQueuedWork(t *testing.T, detail *daemon.SessionDetail) runner.QueuedWork {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(detail) //nolint:gosec // fixed test-only detail
	}))
	t.Cleanup(srv.Close)
	fetched, err := fetchSessionDetail(context.Background(), srv.Client(), srv.URL, detail.SessionID, "")
	if err != nil {
		t.Fatalf("fetchSessionDetail: %v", err)
	}
	qw, err := detailToQueuedWork(fetched)
	if err != nil {
		t.Fatalf("detailToQueuedWork: %v", err)
	}
	return qw
}

func TestDetailLongRoleRoundTrip(t *testing.T) {
	for _, size := range []int{32768, 38912, 80000} {
		role := sizedSystemRole(t, size)
		legacy := &daemon.SessionDetail{SessionID: "legacy-role", SystemPromptOverride: role}
		if got := fetchQueuedWork(t, legacy); got.SystemPromptOverride != role {
			t.Fatalf("legacy size %d: role mismatch", size)
		}
		authPayload, err := json.Marshal(map[string]any{"sessionId": "auth-role", "systemPromptOverride": role})
		if err != nil {
			t.Fatalf("marshal payload: %v", err)
		}
		authority := &daemon.SessionDetail{SessionID: "auth-role", SystemPromptOverride: "MIRROR-SHORT-DISTINGUISHABLE", OperationalPayload: authPayload}
		if got := fetchQueuedWork(t, authority); got.SystemPromptOverride != role {
			t.Fatalf("authority size %d: role mismatch", size)
		} else if strings.Contains(got.SystemPromptOverride, "MIRROR-SHORT") {
			t.Fatalf("authority size %d: mirror text leaked", size)
		}
	}
}

func exactDetailJSON(t *testing.T, target int) []byte {
	t.Helper()
	role := sizedSystemRole(t, target-200)
	detail := &daemon.SessionDetail{SessionID: "bound-role", SystemPromptOverride: role}
	raw, err := json.Marshal(detail) //nolint:gosec // fixed test-only detail
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	for i := 0; i < 16 && len(raw) != target; i++ {
		delta := target - len(raw)
		if delta > 0 {
			role += strings.Repeat("q", delta)
		} else {
			role = role[:len(role)+delta]
		}
		detail.SystemPromptOverride = role
		raw, err = json.Marshal(detail) //nolint:gosec // fixed test-only detail
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
	}
	if len(raw) != target {
		t.Fatalf("detail JSON is %d bytes, want %d", len(raw), target)
	}
	if raw[len(raw)-1] != '}' {
		t.Fatal("detail JSON does not end with closing brace")
	}
	if !strings.Contains(role, "\u2713") {
		t.Fatal("role lacks multibyte content")
	}
	return raw
}

func TestFetchDetailResponseBoundary(t *testing.T) {
	exact := exactDetailJSON(t, 1<<20)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(exact)
	}))
	t.Cleanup(srv.Close)
	fetched, err := fetchSessionDetail(context.Background(), srv.Client(), srv.URL, "bound-role", "")
	if err != nil {
		t.Fatalf("1MiB fetch: %v", err)
	}
	qw, err := detailToQueuedWork(fetched)
	if err != nil {
		t.Fatalf("1MiB decode: %v", err)
	}
	if len(qw.SystemPromptOverride) == 0 {
		t.Fatal("1MiB role missing")
	}
	over := exactDetailJSON(t, (1<<20)+1)
	srvOver := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(over)
	}))
	t.Cleanup(srvOver.Close)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := fetchSessionDetail(ctx, srvOver.Client(), srvOver.URL, "bound-role", ""); err == nil {
		t.Fatal("1MiB+1 fetch succeeded, want decode error")
	} else if !strings.Contains(err.Error(), "decode") {
		t.Fatalf("1MiB+1 error %q, want decode failure", err)
	}
}
