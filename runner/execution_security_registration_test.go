package runner

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/RenseiAI/donmai/agent"
)

func indexZeroSubmissionReport(t *testing.T) ExecutionSecurityReportSubmission {
	t.Helper()
	report, err := agent.RenderExecutionSecurity(agent.Spec{ExecutionSecurity: executionSecuritySection(nil)}, agent.HarnessManifest{})
	if err != nil {
		t.Fatalf("RenderExecutionSecurity: %v", err)
	}
	return ExecutionSecurityReportSubmission{
		ContractVersion: ExecutionSecurityReportContractVersion,
		SessionID:       "session-1",
		WorkerID:        "worker-1",
		Report:          report,
	}
}

func TestValidateExecutionSecurityReportSubmission(t *testing.T) {
	t.Parallel()
	valid := indexZeroSubmissionReport(t)
	if err := ValidateExecutionSecurityReportSubmission(valid); err != nil {
		t.Fatalf("valid submission refused: %v", err)
	}
	mutated := valid
	mutated.ContractVersion = "execution-security-report/v0"
	if err := ValidateExecutionSecurityReportSubmission(mutated); err == nil {
		t.Fatal("wrong contract version accepted")
	}
	mutated = valid
	mutated.SessionID = ""
	if err := ValidateExecutionSecurityReportSubmission(mutated); err == nil {
		t.Fatal("empty session binding accepted")
	}
	mutated = valid
	mutated.Report.ToolApproval.AchievedLevel = agent.ExecutionSecurityLevel("everywhere")
	if err := ValidateExecutionSecurityReportSubmission(mutated); err == nil {
		t.Fatal("report with an unknown level accepted")
	}
}

func TestDecodeExecutionSecurityReportAcceptance(t *testing.T) {
	t.Parallel()
	accepted, err := decodeExecutionSecurityReportAcceptance([]byte(`{"contractVersion":"execution-security-report/v1","decision":"accepted"}`))
	if err != nil || accepted.Decision != "accepted" {
		t.Fatalf("accepted decode = %+v err=%v", accepted, err)
	}
	refused, err := decodeExecutionSecurityReportAcceptance([]byte(`{"contractVersion":"execution-security-report/v1","decision":"refused","code":"report_below_stamp","dimensions":["network","isolation"]}`))
	if err != nil || refused.Decision != "refused" || refused.Code != "report_below_stamp" || len(refused.Dimensions) != 2 {
		t.Fatalf("refused decode = %+v err=%v", refused, err)
	}
	for _, raw := range []string{
		`{"contractVersion":"execution-security-report/v1","decision":"accepted","code":"x"}`,
		`{"contractVersion":"execution-security-report/v1","decision":"refused"}`,
		`{"contractVersion":"execution-security-report/v1","decision":"maybe"}`,
		`{"contractVersion":"execution-security-report/v1","decision":"accepted","unknown":true}`,
		`{"contractVersion":"execution-security-report/v1","decision":"accepted"} trailing`,
		`not json`,
	} {
		if _, err := decodeExecutionSecurityReportAcceptance([]byte(raw)); err == nil {
			t.Fatalf("malformed acceptance accepted: %s", raw)
		}
	}
}

func TestHTTPExecutionSecurityReporterValidation(t *testing.T) {
	t.Parallel()
	if _, err := NewHTTPExecutionSecurityReporter(HTTPExecutionSecurityReporterOptions{}); err == nil {
		t.Fatal("empty options accepted")
	}
	if _, err := NewHTTPExecutionSecurityReporter(HTTPExecutionSecurityReporterOptions{BaseURL: "https://controller.example", Token: func() string { return "t" }}); err == nil {
		t.Fatal("missing worker source accepted")
	}
	if _, err := NewHTTPExecutionSecurityReporter(HTTPExecutionSecurityReporterOptions{BaseURL: "://bad-origin", Token: func() string { return "t" }, WorkerID: func() string { return "w" }}); err == nil {
		t.Fatal("malformed origin accepted")
	}
	var nilReporter *HTTPExecutionSecurityReporter
	if _, err := nilReporter.SubmitExecutionSecurityReport(context.Background(), indexZeroSubmissionReport(t)); err == nil {
		t.Fatal("nil reporter submitted")
	}
}

// TestHTTPExecutionSecurityReporterRoundTrip pins the wire contract: the
// submission posts to the preflight registration route with the worker
// bearer, an accepted envelope authorizes, and a refused envelope is
// returned for the caller to project. A legacy double that answers 200
// with an unrelated body is a submission failure, never an acceptance.
func TestHTTPExecutionSecurityReporterRoundTrip(t *testing.T) {
	t.Parallel()
	var (
		gotPath string
		gotAuth string
		gotBody []byte
		answer  = `{"contractVersion":"execution-security-report/v1","decision":"accepted"}`
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotAuth = r.URL.Path, r.Header.Get("Authorization")
		var err error
		gotBody, err = io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read submission body: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(answer))
	}))
	t.Cleanup(srv.Close)

	reporter, err := NewHTTPExecutionSecurityReporter(HTTPExecutionSecurityReporterOptions{
		BaseURL:  srv.URL,
		Client:   srv.Client(),
		Token:    func() string { return "worker-token" },
		WorkerID: func() string { return "worker-1" },
	})
	if err != nil {
		t.Fatal(err)
	}
	submission := indexZeroSubmissionReport(t)
	acceptance, err := reporter.SubmitExecutionSecurityReport(context.Background(), submission)
	if err != nil || acceptance.Decision != "accepted" {
		t.Fatalf("acceptance = %+v err=%v", acceptance, err)
	}
	if want := "/api/daemon/sessions/session-1/execution-preflight-registration"; gotPath != want {
		t.Fatalf("route = %q, want %q", gotPath, want)
	}
	if gotAuth != "Bearer worker-token" {
		t.Fatalf("authorization = %q, want the worker bearer", gotAuth)
	}
	if !strings.Contains(string(gotBody), `"execution-security-report/v1"`) || !strings.Contains(string(gotBody), `"sessionId":"session-1"`) {
		t.Fatalf("submission body is not the closed report envelope: %s", gotBody)
	}

	answer = `{"contractVersion":"execution-security-report/v1","decision":"refused","code":"report_below_stamp","dimensions":["network"]}`
	acceptance, err = reporter.SubmitExecutionSecurityReport(context.Background(), submission)
	if err != nil || acceptance.Decision != "refused" || acceptance.Code != "report_below_stamp" {
		t.Fatalf("refused acceptance = %+v err=%v", acceptance, err)
	}

	answer = `{"refreshed":true,"ok":true}`
	if _, err := reporter.SubmitExecutionSecurityReport(context.Background(), submission); err == nil {
		t.Fatal("legacy double body accepted as an acceptance")
	}
}

// TestHTTPExecutionSecurityReporterStatusHandling pins the fail-closed
// status map: a non-2xx carrying a refused envelope is returned as a
// refusal; a bare non-2xx is a submission failure, never an acceptance.
func TestHTTPExecutionSecurityReporterStatusHandling(t *testing.T) {
	t.Parallel()
	status := http.StatusForbidden
	answer := `{"contractVersion":"execution-security-report/v1","decision":"refused","code":"report_below_stamp"}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(answer))
	}))
	t.Cleanup(srv.Close)
	reporter, err := NewHTTPExecutionSecurityReporter(HTTPExecutionSecurityReporterOptions{
		BaseURL:  srv.URL,
		Client:   srv.Client(),
		Token:    func() string { return "worker-token" },
		WorkerID: func() string { return "worker-1" },
	})
	if err != nil {
		t.Fatal(err)
	}
	submission := indexZeroSubmissionReport(t)
	acceptance, err := reporter.SubmitExecutionSecurityReport(context.Background(), submission)
	if err != nil || acceptance.Decision != "refused" {
		t.Fatalf("non-2xx refused envelope = %+v err=%v, want the refusal", acceptance, err)
	}
	answer = `not a decision envelope`
	if _, err := reporter.SubmitExecutionSecurityReport(context.Background(), submission); err == nil {
		t.Fatal("bare non-2xx accepted as a submission result")
	}
}

// credential floor: no bearer means no post at all.
func TestHTTPExecutionSecurityReporterRefusesWithoutAuth(t *testing.T) {
	t.Parallel()
	posted := false
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {
		posted = true
	}))
	t.Cleanup(srv.Close)
	reporter, err := NewHTTPExecutionSecurityReporter(HTTPExecutionSecurityReporterOptions{
		BaseURL:  srv.URL,
		Client:   srv.Client(),
		Token:    func() string { return "" },
		WorkerID: func() string { return "worker-1" },
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := reporter.SubmitExecutionSecurityReport(context.Background(), indexZeroSubmissionReport(t)); err == nil {
		t.Fatal("submission without a bearer succeeded")
	}
	if posted {
		t.Fatal("submission without a bearer posted")
	}
}

func TestExecutionSecurityRefusalFromAcceptance(t *testing.T) {
	t.Parallel()
	refusal := executionSecurityRefusalFromAcceptance(ExecutionSecurityReportAcceptance{
		ContractVersion: ExecutionSecurityReportContractVersion,
		Decision:        "refused",
		Code:            "report_below_stamp",
		Dimensions:      []agent.ExecutionSecurityDimension{agent.ExecutionSecurityNetwork, "elsewhere"},
	})
	if refusal.Code != agent.ExecutionSecurityReceiptUnmet {
		t.Fatalf("code = %q, want %q", refusal.Code, agent.ExecutionSecurityReceiptUnmet)
	}
	if len(refusal.Dimensions) != 1 || refusal.Dimensions[0] != agent.ExecutionSecurityNetwork || refusal.Dimension != agent.ExecutionSecurityNetwork {
		t.Fatalf("dimensions = %+v (singular %q), want only the closed network dimension", refusal.Dimensions, refusal.Dimension)
	}
	if !strings.Contains(refusal.Detail, "report_below_stamp") {
		t.Fatalf("detail = %q, want the receiver code", refusal.Detail)
	}
}

func TestExecutionSecurityReportCredentials(t *testing.T) {
	t.Parallel()
	qw := QueuedWork{WorkerID: "dispatched-worker"}
	qw.AuthToken = "dispatched-token"
	workerID, token := executionSecurityReportCredentials(context.Background(), qw, nil)
	if workerID != "dispatched-worker" || token != "dispatched-token" {
		t.Fatalf("credentials = %q/%q, want the dispatched pair", workerID, token)
	}
	workerID, token = executionSecurityReportCredentials(context.Background(), qw, func(context.Context) (RuntimeCredentials, error) {
		return RuntimeCredentials{WorkerID: "fresh-worker", AuthToken: "fresh-token"}, nil
	})
	if workerID != "fresh-worker" || token != "fresh-token" {
		t.Fatalf("credentials = %q/%q, want the fresh pair", workerID, token)
	}
	workerID, token = executionSecurityReportCredentials(context.Background(), qw, func(context.Context) (RuntimeCredentials, error) {
		return RuntimeCredentials{}, errors.New("rotation unavailable")
	})
	if workerID != "dispatched-worker" || token != "dispatched-token" {
		t.Fatalf("credentials = %q/%q, want the dispatched fallback", workerID, token)
	}
}

// bogusDecisionReporter answers with a decision outside the closed
// accepted/refused envelope, proving the caller fails closed on it.
type bogusDecisionReporter struct{}

func (bogusDecisionReporter) SubmitExecutionSecurityReport(context.Context, ExecutionSecurityReportSubmission) (ExecutionSecurityReportAcceptance, error) {
	return ExecutionSecurityReportAcceptance{ContractVersion: ExecutionSecurityReportContractVersion, Decision: "maybe"}, nil
}

// TestSubmitExecutionSecurityReportFailureIsTyped pins the fail-closed
// projection: a failed post is execution_security_receipt_unmet, so the
// caller records a typed refusal rather than an untyped spawn failure.
// A decision outside the closed envelope fails the same way.
func TestSubmitExecutionSecurityReportFailureIsTyped(t *testing.T) {
	t.Parallel()
	h := newRunnerHarness(t)
	h.runner.executionSecurityReporterOverride = invalidExecutionSecurityReporter(errors.New("controller unreachable"))
	report, err := agent.RenderExecutionSecurity(agent.Spec{ExecutionSecurity: executionSecuritySection(nil)}, agent.HarnessManifest{})
	if err != nil {
		t.Fatal(err)
	}
	qw := h.queuedWork("REPORT-FAILURE-TYPED")
	err = h.runner.submitExecutionSecurityReport(context.Background(), qw, report)
	if agent.ExecutionSecurityErrorCode(err) != agent.ExecutionSecurityReceiptUnmet {
		t.Fatalf("err = %v, want execution_security_receipt_unmet", err)
	}
	if !strings.Contains(err.Error(), "controller unreachable") {
		t.Fatalf("err = %v, want the submission cause", err)
	}
	h.runner.executionSecurityReporterOverride = bogusDecisionReporter{}
	if err := h.runner.submitExecutionSecurityReport(context.Background(), qw, report); agent.ExecutionSecurityErrorCode(err) != agent.ExecutionSecurityReceiptUnmet {
		t.Fatalf("bogus decision err = %v, want execution_security_receipt_unmet", err)
	}
}
