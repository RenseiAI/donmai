package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/RenseiAI/donmai/afclient"
	"github.com/RenseiAI/donmai/sessionshim"
)

func postRestartPrepareRefusal(t *testing.T, d *Daemon) afclient.DaemonRestartPreflightRefusal {
	t.Helper()
	server := httptest.NewServer(NewServer(d).httpd.Handler)
	t.Cleanup(server.Close)
	res, err := http.Post(server.URL+"/api/daemon/restart/prepare", "application/json", nil)
	if err != nil {
		t.Fatalf("POST restart prepare: %v", err)
	}
	defer func() { _ = res.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(res.Body, 8192))
	if err != nil {
		t.Fatalf("read refusal body: %v", err)
	}
	if res.StatusCode != http.StatusConflict {
		t.Fatalf("POST restart prepare = %d %s, want 409", res.StatusCode, body)
	}
	var refusal afclient.DaemonRestartPreflightRefusal
	if err := json.Unmarshal(body, &refusal); err != nil {
		t.Fatalf("decode refusal %s: %v", body, err)
	}
	return refusal
}

// A preflight refused at the fence stage must say so on the wire. The closed
// top-level code stays restart_preflight_refused — clients key their typed
// decode on it — while the cause token discriminates the stage: without it an
// acceptance oracle watching a fenced restart cannot tell a correct fence
// refusal from any other preflight failure.
func TestRestartPreflightFenceRefusalAnswersFenceCause(t *testing.T) {
	store := restartExactStoreFunc(func(_ context.Context, request sessionshim.FenceRequest) (sessionshim.FenceAcknowledgement, error) {
		return sessionshim.FenceAcknowledgement{RequestBytes: request.RequestBytes, DurableRevision: "revision"}, nil
	})
	id := sessionshim.Identity{OrgID: "org", SessionID: "session"}
	d := newRestartTestDaemon(t, store, id)
	if err := d.armSessionShimAcceptanceFenceRefusal(id); err != nil {
		t.Fatalf("arm fence refusal: %v", err)
	}

	refusal := postRestartPrepareRefusal(t, d)
	if refusal.Code != afclient.DaemonRestartPreflightRefusalCode {
		t.Fatalf("refusal code = %q, want the untouched closed code %q",
			refusal.Code, afclient.DaemonRestartPreflightRefusalCode)
	}
	if refusal.Cause != afclient.DaemonRestartCauseFenceRefused {
		t.Fatalf("refusal cause = %q, want %q — the oracle cannot discriminate a fence refusal without it",
			refusal.Cause, afclient.DaemonRestartCauseFenceRefused)
	}
}

// Discriminating control: a preflight refused at a NON-fence stage answers its
// own cause token, never restart_fence_refused. A fixture that passed on the
// fence token here would prove the field exists, not that it discriminates.
func TestRestartPreflightNonFenceStageAnswersItsOwnCause(t *testing.T) {
	store := restartExactStoreFunc(func(_ context.Context, request sessionshim.FenceRequest) (sessionshim.FenceAcknowledgement, error) {
		return sessionshim.FenceAcknowledgement{RequestBytes: request.RequestBytes, DurableRevision: "revision"}, nil
	})
	id := sessionshim.Identity{OrgID: "org", SessionID: "session"}
	d := newRestartTestDaemon(t, store, id)
	d.shims.restartID = func() (string, error) { return "", errors.New("entropy unavailable") }

	refusal := postRestartPrepareRefusal(t, d)
	if refusal.Code != afclient.DaemonRestartPreflightRefusalCode {
		t.Fatalf("refusal code = %q, want the untouched closed code %q",
			refusal.Code, afclient.DaemonRestartPreflightRefusalCode)
	}
	if refusal.Cause == afclient.DaemonRestartCauseFenceRefused {
		t.Fatalf("non-fence refusal answered the fence cause %q — the token no longer discriminates", refusal.Cause)
	}
	if refusal.Cause != afclient.DaemonRestartCauseMintPreparationIdentity {
		t.Fatalf("refusal cause = %q, want %q", refusal.Cause, afclient.DaemonRestartCauseMintPreparationIdentity)
	}
}

// A stage that refuses without an assigned token must still answer a closed
// cause — the generic code itself — never an open string derived from the
// error text.
func TestRestartPreflightUnmappedStageAnswersGenericCause(t *testing.T) {
	d := newRestartTestDaemon(t, nil)
	d.setState(StateStopped)

	refusal := postRestartPrepareRefusal(t, d)
	if refusal.Cause != afclient.DaemonRestartPreflightRefusalCode {
		t.Fatalf("unmapped-stage cause = %q, want the closed generic %q",
			refusal.Cause, afclient.DaemonRestartPreflightRefusalCode)
	}
}
