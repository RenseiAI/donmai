package runner

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/RenseiAI/donmai/agent"
)

// ExecutionSecurityReportContractVersion identifies the pre-spawn report
// submission the runner posts through the preflight registration route and
// the acceptance it waits for before spawning.
const ExecutionSecurityReportContractVersion = "execution-security-report/v1"

// executionSecurityReportDecisionAccepted is the only decision that authorizes
// a spawn. Every other decision authorizes zero spawn.
const executionSecurityReportDecisionAccepted = "accepted"

// executionSecurityReportDecisionRefused denies the spawn. The response may
// name the below-stamp dimensions in the closed execution-security
// vocabulary.
const executionSecurityReportDecisionRefused = "refused"

// executionSecurityReportTimeout bounds one report submission. There is no
// retry: an ambiguous post authorizes zero spawn, and a second post would
// risk recording the same report twice.
const executionSecurityReportTimeout = 10 * time.Second

// executionSecurityReportResponseLimit bounds the acceptance body. The
// decision envelope is small; anything larger is not an acceptance.
const executionSecurityReportResponseLimit = 64 * 1024

// ExecutionSecurityReportSubmission is the wire body the runner posts before
// spawn: what the rendered session will enforce, bound to the session and
// worker the stamp was dispatched to. Secret-free by construction — levels,
// enforcing layers and digests only, never prompt, environment or credential
// values.
type ExecutionSecurityReportSubmission struct {
	ContractVersion string                        `json:"contractVersion"`
	SessionID       string                        `json:"sessionId"`
	WorkerID        string                        `json:"workerId"`
	Report          agent.ExecutionSecurityReport `json:"report"`
}

// ExecutionSecurityReportAcceptance is the closed acceptance envelope. Only
// Decision "accepted" authorizes the spawn; "refused" carries the closed
// refusal code and the below-stamp dimensions when the receiver names them.
type ExecutionSecurityReportAcceptance struct {
	ContractVersion string                             `json:"contractVersion"`
	Decision        string                             `json:"decision"`
	Code            string                             `json:"code,omitempty"`
	Dimensions      []agent.ExecutionSecurityDimension `json:"dimensions,omitempty"`
}

// ValidateExecutionSecurityReportSubmission checks the closed submission
// shape before it is posted: the contract version, the session and worker
// binding, and the report's own closed shape.
func ValidateExecutionSecurityReportSubmission(submission ExecutionSecurityReportSubmission) error {
	if submission.ContractVersion != ExecutionSecurityReportContractVersion {
		return fmt.Errorf("runner: unsupported execution security report version %q", submission.ContractVersion)
	}
	if strings.TrimSpace(submission.SessionID) == "" || strings.TrimSpace(submission.WorkerID) == "" {
		return errors.New("runner: execution security report requires a session and worker binding")
	}
	if err := agent.ValidateExecutionSecurityReport(submission.Report); err != nil {
		return err
	}
	return nil
}

// decodeExecutionSecurityReportAcceptance strictly decodes one bounded
// acceptance envelope. Unknown fields refuse: a receiver that answers in a
// newer shape is not an acceptance this runner understands.
func decodeExecutionSecurityReportAcceptance(raw []byte) (ExecutionSecurityReportAcceptance, error) {
	var acceptance ExecutionSecurityReportAcceptance
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&acceptance); err != nil {
		return ExecutionSecurityReportAcceptance{}, fmt.Errorf("runner: decode execution security report acceptance: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		return ExecutionSecurityReportAcceptance{}, errors.New("runner: execution security report acceptance has trailing JSON")
	}
	if acceptance.ContractVersion != ExecutionSecurityReportContractVersion {
		return ExecutionSecurityReportAcceptance{}, fmt.Errorf("runner: unsupported execution security report acceptance version %q", acceptance.ContractVersion)
	}
	switch acceptance.Decision {
	case executionSecurityReportDecisionAccepted:
		if acceptance.Code != "" || len(acceptance.Dimensions) != 0 {
			return ExecutionSecurityReportAcceptance{}, errors.New("runner: accepted execution security report cannot carry refusal fields")
		}
	case executionSecurityReportDecisionRefused:
		if strings.TrimSpace(acceptance.Code) == "" {
			return ExecutionSecurityReportAcceptance{}, errors.New("runner: refused execution security report requires a code")
		}
	default:
		return ExecutionSecurityReportAcceptance{}, errors.New("runner: execution security report decision must be accepted or refused")
	}
	return acceptance, nil
}

// ExecutionSecurityReporter submits the pre-spawn execution-security report
// and waits for acceptance. It is selected from trusted runner
// configuration; work items never select it.
type ExecutionSecurityReporter interface {
	SubmitExecutionSecurityReport(context.Context, ExecutionSecurityReportSubmission) (ExecutionSecurityReportAcceptance, error)
}

// HTTPExecutionSecurityReporterOptions configure the report submission
// client. Token is read per call so rotation does not strand a long-lived
// runner.
type HTTPExecutionSecurityReporterOptions struct {
	// BaseURL is the controller origin that owns the preflight registration
	// route. Required.
	BaseURL string
	// Client performs the post. Nil uses a bounded default.
	Client *http.Client
	// Token returns the worker bearer for the Authorization header. Required;
	// an empty bearer refuses the submission before any byte is posted.
	Token func() string
	// WorkerID returns the worker identity carried in the submission body.
	// Required.
	WorkerID func() string
}

// HTTPExecutionSecurityReporter posts the pre-spawn report to the
// controller's preflight registration route and decodes the closed
// acceptance envelope. It refuses redirects.
type HTTPExecutionSecurityReporter struct {
	base     *url.URL
	client   *http.Client
	token    func() string
	workerID func() string
}

// NewHTTPExecutionSecurityReporter validates and freezes the controller
// origin while retaining dynamic token and worker sources for rotation.
func NewHTTPExecutionSecurityReporter(options HTTPExecutionSecurityReporterOptions) (*HTTPExecutionSecurityReporter, error) {
	base, err := url.Parse(strings.TrimSpace(options.BaseURL))
	if err != nil || base.Scheme == "" || base.Host == "" || base.User != nil || base.RawQuery != "" || base.Fragment != "" || (base.Path != "" && base.Path != "/") {
		return nil, errors.New("runner: execution security reporter requires a fixed absolute origin")
	}
	if options.Token == nil {
		return nil, errors.New("runner: execution security reporter token source is required")
	}
	if options.WorkerID == nil {
		return nil, errors.New("runner: execution security reporter worker source is required")
	}
	client := options.Client
	if client == nil {
		client = &http.Client{Timeout: executionSecurityReportTimeout}
	}
	clientCopy := *client
	clientCopy.CheckRedirect = func(_ *http.Request, _ []*http.Request) error {
		return http.ErrUseLastResponse
	}
	base.Path, base.RawPath = "", ""
	return &HTTPExecutionSecurityReporter{base: base, client: &clientCopy, token: options.Token, workerID: options.WorkerID}, nil
}

// executionSecurityReportRoute is the preflight registration route the report
// travels on: the same route the host receipt registration uses, extended to
// accept the runner's enforcement report before spawn.
func executionSecurityReportRoute(base *url.URL, sessionID string) string {
	endpoint := *base
	endpoint.Path = "/api/daemon/sessions/" + url.PathEscape(sessionID) + "/execution-preflight-registration"
	return endpoint.String()
}

// SubmitExecutionSecurityReport posts one bounded closed submission and
// decodes one bounded closed acceptance over the configured authenticated
// channel. A transport failure, a non-2xx status, or an undecodable body is
// an error, never an acceptance: only a decoded "accepted" decision
// authorizes the spawn.
func (r *HTTPExecutionSecurityReporter) SubmitExecutionSecurityReport(ctx context.Context, submission ExecutionSecurityReportSubmission) (ExecutionSecurityReportAcceptance, error) {
	if r == nil || r.base == nil || r.client == nil || r.token == nil || r.workerID == nil {
		return ExecutionSecurityReportAcceptance{}, errors.New("runner: execution security HTTP reporter is not configured")
	}
	if err := ValidateExecutionSecurityReportSubmission(submission); err != nil {
		return ExecutionSecurityReportAcceptance{}, err
	}
	requestContext, cancel := context.WithTimeout(ctx, executionSecurityReportTimeout)
	defer cancel()
	token := strings.TrimSpace(r.token())
	if token == "" {
		return ExecutionSecurityReportAcceptance{}, errors.New("runner: execution security reporter authentication is unavailable")
	}
	workerID := strings.TrimSpace(r.workerID())
	if workerID == "" {
		return ExecutionSecurityReportAcceptance{}, errors.New("runner: execution security reporter worker identity is unavailable")
	}
	submission.WorkerID = workerID
	body, err := json.Marshal(submission)
	if err != nil {
		return ExecutionSecurityReportAcceptance{}, err
	}
	httpRequest, err := http.NewRequestWithContext(requestContext, http.MethodPost, executionSecurityReportRoute(r.base, submission.SessionID), bytes.NewReader(body))
	if err != nil {
		return ExecutionSecurityReportAcceptance{}, err
	}
	httpRequest.Header.Set("Authorization", "Bearer "+token)
	httpRequest.Header.Set("Content-Type", "application/json")
	httpRequest.Header.Set("Accept", "application/json")
	response, err := r.client.Do(httpRequest)
	if err != nil {
		return ExecutionSecurityReportAcceptance{}, fmt.Errorf("runner: submit execution security report: %w", err)
	}
	defer func() { _ = response.Body.Close() }()
	raw, readErr := io.ReadAll(io.LimitReader(response.Body, executionSecurityReportResponseLimit+1))
	if readErr != nil {
		return ExecutionSecurityReportAcceptance{}, fmt.Errorf("runner: read execution security report acceptance: %w", readErr)
	}
	if len(raw) > executionSecurityReportResponseLimit {
		return ExecutionSecurityReportAcceptance{}, errors.New("runner: execution security report acceptance is too large")
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		if acceptance, decodeErr := decodeExecutionSecurityReportAcceptance(bytes.TrimSpace(raw)); decodeErr == nil && acceptance.Decision == executionSecurityReportDecisionRefused {
			return acceptance, nil
		}
		return ExecutionSecurityReportAcceptance{}, fmt.Errorf("runner: execution security report submission returned HTTP %d", response.StatusCode)
	}
	acceptance, err := decodeExecutionSecurityReportAcceptance(bytes.TrimSpace(raw))
	if err != nil {
		return ExecutionSecurityReportAcceptance{}, err
	}
	return acceptance, nil
}

// executionSecurityReporter resolves the report submission client: the
// configured reporter when one is set, otherwise the HTTP reporter posting
// to the session's own controller origin with the given worker credential.
func (r *Runner) executionSecurityReporter(qw QueuedWork, workerID, token string) ExecutionSecurityReporter {
	if r.executionSecurityReporterOverride != nil {
		return r.executionSecurityReporterOverride
	}
	reporter, err := NewHTTPExecutionSecurityReporter(HTTPExecutionSecurityReporterOptions{
		BaseURL:  qw.PlatformURL,
		Client:   r.httpClient,
		Token:    func() string { return token },
		WorkerID: func() string { return workerID },
	})
	if err != nil {
		return invalidExecutionSecurityReporter(err)
	}
	return reporter
}

// executionSecurityReportCredentials resolves the worker identity and bearer
// exactly like the terminal result post: the freshest credential when a
// provider is wired, otherwise the dispatched pair. Empty values stay empty
// so the submission refuses before any byte is posted.
func executionSecurityReportCredentials(ctx context.Context, qw QueuedWork, provider CredentialProvider) (workerID, token string) {
	workerID, token = qw.WorkerID, qw.AuthToken
	if provider == nil {
		return workerID, token
	}
	fresh, err := provider(ctx)
	if err != nil {
		return workerID, token
	}
	if fresh.WorkerID != "" {
		workerID = fresh.WorkerID
	}
	if fresh.AuthToken != "" {
		token = fresh.AuthToken
	}
	return workerID, token
}

// invalidExecutionSecurityReporter carries a construction failure to the
// submission site so a misconfigured reporter refuses the spawn instead of
// panicking or skipping the wait.
func invalidExecutionSecurityReporter(err error) ExecutionSecurityReporter {
	return invalidReporter{err: err}
}

type invalidReporter struct {
	err error
}

func (r invalidReporter) SubmitExecutionSecurityReport(context.Context, ExecutionSecurityReportSubmission) (ExecutionSecurityReportAcceptance, error) {
	if r.err != nil {
		return ExecutionSecurityReportAcceptance{}, r.err
	}
	return ExecutionSecurityReportAcceptance{}, errors.New("runner: execution security reporter is not configured")
}

// submitExecutionSecurityReport sends what the rendered session will enforce
// through the preflight registration call and waits for acceptance. Only a
// decoded "accepted" decision returns nil. A refusal or any submission
// failure returns a typed execution-security error: the caller must not
// spawn.
func (r *Runner) submitExecutionSecurityReport(ctx context.Context, qw QueuedWork, report agent.ExecutionSecurityReport) error {
	submission := ExecutionSecurityReportSubmission{
		ContractVersion: ExecutionSecurityReportContractVersion,
		SessionID:       qw.SessionID,
		WorkerID:        qw.WorkerID,
		Report:          report,
	}
	if err := ValidateExecutionSecurityReportSubmission(submission); err != nil {
		return err
	}
	workerID, token := executionSecurityReportCredentials(ctx, qw, r.credentialProvider)
	submission.WorkerID = workerID
	acceptance, err := r.executionSecurityReporter(qw, workerID, token).SubmitExecutionSecurityReport(ctx, submission)
	if err != nil {
		return &agent.ExecutionSecurityError{
			Code:   agent.ExecutionSecurityReceiptUnmet,
			Detail: fmt.Sprintf("the execution security report submission failed: %v", err),
		}
	}
	switch acceptance.Decision {
	case executionSecurityReportDecisionAccepted:
		r.logger.Info("execution security report accepted",
			"sessionId", qw.SessionID,
			"workerId", qw.WorkerID,
		)
		return nil
	case executionSecurityReportDecisionRefused:
		return executionSecurityRefusalFromAcceptance(acceptance)
	default:
		return &agent.ExecutionSecurityError{
			Code:   agent.ExecutionSecurityReceiptUnmet,
			Detail: fmt.Sprintf("the execution security report acceptance decision %q is not accepted", acceptance.Decision),
		}
	}
}

// executionSecurityRefusalFromAcceptance projects a refused acceptance onto
// the typed execution-security refusal: the receipt-unmet code with the
// below-stamp dimensions the receiver named, restricted to the closed
// dimension vocabulary.
func executionSecurityRefusalFromAcceptance(acceptance ExecutionSecurityReportAcceptance) *agent.ExecutionSecurityError {
	refusal := &agent.ExecutionSecurityError{
		Code:   agent.ExecutionSecurityReceiptUnmet,
		Detail: fmt.Sprintf("the execution security report was refused: %s", strings.TrimSpace(acceptance.Code)),
	}
	for _, dimension := range acceptance.Dimensions {
		known := false
		for _, candidate := range agent.ExecutionSecurityDimensions() {
			if dimension == candidate {
				known = true
				break
			}
		}
		if known {
			refusal.Dimensions = append(refusal.Dimensions, dimension)
		}
	}
	if len(refusal.Dimensions) == 1 {
		refusal.Dimension = refusal.Dimensions[0]
	}
	return refusal
}
