package afclient

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// The refusal error prints its prefix once and its cause once. The doubled
// form — "daemon restart preflight refused: daemon restart preflight refused"
// — came from appending the server's message, which is the same fixed string
// as the sentinel prefix.
func TestRestartPreflightRefusalErrorPrintsCauseOnce(t *testing.T) {
	withCause := (&DaemonRestartPreflightRefusalError{
		Code:    DaemonRestartPreflightRefusalCode,
		Cause:   DaemonRestartCauseFenceRefused,
		Message: "daemon restart preflight refused",
	}).Error()
	if withCause != "daemon restart preflight refused (restart_fence_refused)" {
		t.Fatalf("Error() = %q, want the prefix with the cause in parentheses", withCause)
	}
	if got := strings.Count(withCause, "daemon restart preflight refused"); got != 1 {
		t.Fatalf("prefix appears %d times in %q, want exactly once", got, withCause)
	}
	if got := strings.Count(withCause, DaemonRestartCauseFenceRefused); got != 1 {
		t.Fatalf("cause appears %d times in %q, want exactly once", got, withCause)
	}

	// A causeless legacy refusal (older daemon) must not double the prefix
	// either: the server's message IS the prefix.
	legacy := (&DaemonRestartPreflightRefusalError{
		Code:    DaemonRestartPreflightRefusalCode,
		Message: "daemon restart preflight refused",
	}).Error()
	if got := strings.Count(legacy, "daemon restart preflight refused"); got != 1 {
		t.Fatalf("legacy prefix appears %d times in %q, want exactly once", got, legacy)
	}
}

// A 409 carrying the additive cause field still decodes as the typed refusal
// (the closed top-level code is untouched and remains the discriminator), and
// the cause survives to the caller.
func TestPrepareRestartDecodesRefusalCause(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusConflict)
		_, _ = io.WriteString(w,
			`{"error":"daemon restart preflight refused","code":"restart_preflight_refused","cause":"restart_fence_refused"}`)
	}))
	t.Cleanup(srv.Close)

	_, err := NewDaemonClientFromURL(srv.URL).PrepareRestart()
	var refusal *DaemonRestartPreflightRefusalError
	if !errors.As(err, &refusal) {
		t.Fatalf("PrepareRestart error = %v, want the typed refusal despite the additive cause field", err)
	}
	if refusal.Cause != DaemonRestartCauseFenceRefused {
		t.Fatalf("refusal cause = %q, want %q", refusal.Cause, DaemonRestartCauseFenceRefused)
	}
	if got := strings.Count(err.Error(), "daemon restart preflight refused"); got != 1 {
		t.Fatalf("prefix appears %d times in %q, want exactly once", got, err.Error())
	}
	if got := strings.Count(err.Error(), DaemonRestartCauseFenceRefused); got != 1 {
		t.Fatalf("cause appears %d times in %q, want exactly once", got, err.Error())
	}
}
