package kgextract

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"
)

// resultPostTimeout bounds the result POST. Mirrors the detached-timeout pattern
// in codesurvival/executor.go.
const resultPostTimeout = 30 * time.Second

// emitTimeout bounds a single constrained provider emit. A hung provider must
// not wedge the batch handler (and thus the poll loop) indefinitely; each emit
// ATTEMPT gets its own deadline (an observation may take two — see the repair
// retry in extractOne).
//
// 120s is generous against the measured shape: a single-shot claude completion
// for one observation lands in ~1.3–8s (CLI 2.1.226). It was NOT generous
// against the agentic shape this lane used to run on, which blew it on every
// observation — the fix was to stop booting an agent for a completion, not to
// raise this number. Keep it a ceiling for a stuck process, not a budget the
// happy path is expected to approach.
const emitTimeout = 120 * time.Second

// emitRawErrExcerpt bounds how much of an unparseable emit is quoted back to the
// model in the repair instruction (and, truncated further, into the error the
// platform records).
const emitRawErrExcerpt = 2000

// EmitterFactory builds the Emitter used for one work item. It is a factory
// (not a single Emitter) because the provider/model/authMode are per-item: each
// KgExtractWorkItem names its own provider + model. The default factory builds a
// providerEmitter from a real agent.Provider; tests inject a stub factory that
// returns a deterministic Emitter.
//
// Returning an error means no emitter could be built for the item (e.g. the
// named provider is unavailable on this host); the executor folds that into a
// status:"error" result so the platform learns the item could not run.
type EmitterFactory func(ctx context.Context, item KgExtractWorkItem) (Emitter, error)

// Executor runs kg-extraction batch items. Build via NewExecutor. Safe for
// concurrent use across items: every method holds only per-call state.
type Executor struct {
	emitterFactory EmitterFactory
	httpClient     *http.Client
	logger         *slog.Logger
	workerVersion  string
	// platformBaseURL prefixes the item's ResultEndpoint PATH. Empty means the
	// item must carry an absolute ResultEndpoint (tests pass an httptest URL
	// directly).
	platformBaseURL string
	// now is injectable for deterministic tests.
	now func() time.Time
}

// Options configures NewExecutor.
type Options struct {
	// EmitterFactory overrides the default provider-emitter factory. Tests inject
	// a stub factory returning a deterministic Emitter.
	EmitterFactory EmitterFactory
	// HTTPClient overrides http.DefaultClient for the result POST.
	HTTPClient *http.Client
	// Logger overrides slog.Default().
	Logger *slog.Logger
	// WorkerVersion stamps log context (mirrors codesurvival).
	WorkerVersion string
	// PlatformBaseURL is the base URL prefixed onto the item's ResultEndpoint
	// PATH (e.g. "https://platform.example.com"). Empty leaves the endpoint
	// as-supplied (used by tests passing an absolute httptest URL).
	PlatformBaseURL string
}

// NewExecutor constructs an Executor, filling in production defaults for any nil
// collaborator.
func NewExecutor(opts Options) *Executor {
	e := &Executor{
		emitterFactory:  opts.EmitterFactory,
		httpClient:      opts.HTTPClient,
		logger:          opts.Logger,
		workerVersion:   opts.WorkerVersion,
		platformBaseURL: strings.TrimRight(opts.PlatformBaseURL, "/"),
		now:             time.Now,
	}
	if e.httpClient == nil {
		e.httpClient = http.DefaultClient
	}
	if e.logger == nil {
		e.logger = slog.Default()
	}
	if e.workerVersion == "" {
		e.workerVersion = "dev"
	}
	// emitterFactory is intentionally left nil-able: a worker built WITHOUT a
	// provider can still receive items (it will report status:"error" per item).
	// See emitterFor.
	return e
}

// Handle is the batch-handler entry point for a kg-extraction item. It is
// best-effort by contract: every per-observation failure is folded into the
// result and a status:"error"/"partial" is POSTed; Handle returns nil so the
// poll loop NEVER crashes and NEVER touches the agent path. It returns a non-nil
// error only when the work item is rejected before any work begins (org-claim
// mismatch / unknown contract version) so the poll loop can log the rejection —
// the loop still continues. A contract-version rejection ALSO POSTs a terminal
// status:"error" result first (see postRejection), so the refusal is visible on
// the platform instead of only in this host's log.
func (e *Executor) Handle(ctx context.Context, item KgExtractWorkItem) error {
	log := e.logger.With(
		"batchJobId", item.BatchJobID,
		"workType", item.WorkType,
		"orgId", item.OrgID,
		"projectId", item.ProjectID,
		"authMode", string(item.AuthMode),
		"provider", item.Provider,
		"observations", len(item.Observations),
	)

	// Reject an unknown contract major before doing any work.
	//
	// A rejection is POSTed as a TERMINAL FAILURE before returning. The platform
	// popped this item off the org queue and holds a claim key on it; if the
	// worker returns silently the item is destroyed, the FSM row stays 'pending'
	// forever, and the claim key suppresses every re-stage of the same stable
	// batchJobId until it expires. A visible failed row is the difference between
	// "the lane is idle" and "the lane is fenced out" — the exact ambiguity that
	// hid a two-month contract drift.
	if item.ContractVersion != KGExtractionContractVersion {
		log.Warn("kg-extraction: contract version mismatch; rejecting",
			"itemVersion", item.ContractVersion, "workerVersion", KGExtractionContractVersion)
		e.postRejection(ctx, log, item, fmt.Sprintf(
			"unsupported contract version %d (worker speaks %d)",
			item.ContractVersion, KGExtractionContractVersion))
		return fmt.Errorf("kgextract: unsupported contract version %d (worker speaks %d)",
			item.ContractVersion, KGExtractionContractVersion)
	}

	// Re-verify the JWT org claim (cross-tenant guard). Reject + audit on
	// mismatch; never run the emit.
	if err := verifyOrgClaim(item.ResultAuth, item.OrgID); err != nil {
		log.Warn("kg-extraction: org claim re-verification failed; rejecting (cross-tenant guard)",
			"err", err)
		return fmt.Errorf("kgextract: org claim rejected: %w", err)
	}

	res := e.extract(ctx, log, item)

	// POST the result. Best-effort — a POST failure does not crash the loop.
	if err := e.postResult(ctx, log, item, res); err != nil {
		log.Warn("kg-extraction: result POST failed (non-fatal)", "err", err)
	}
	return nil
}

// postRejection POSTs a terminal status:"error" result for an item the executor
// refused before doing any work, so the platform's FSM row flips to 'failed'
// with the reason instead of sitting 'pending' forever.
//
// It echoes the ITEM's contractVersion rather than the worker's. That is the one
// deliberate exception to "the executor echoes the version it was built
// against": the platform pins contractVersion with a literal, so a result
// stamped with the worker's older version is rejected at ingest with a 400 —
// which would leave the failure just as invisible as posting nothing. Echoing
// the item's version is safe precisely because a rejection carries `results: []`
// — there is no graph payload that a version mismatch could malform.
//
// Deliberately NOT called for the org-claim rejection below: that guard fires
// when the item's resultAuth does not claim the item's org, so the only bearer
// token available to POST with is the suspect one. A cross-tenant guard must not
// then turn around and use it.
func (e *Executor) postRejection(ctx context.Context, log *slog.Logger, item KgExtractWorkItem, reason string) {
	res := KGExtractionResult{
		BatchJobID:      item.BatchJobID,
		ContractVersion: item.ContractVersion,
		Results:         []KGExtractionResultEntry{}, // never nil → serializes as []
		Status:          StatusError,
		Error:           "kgextract: " + reason,
	}
	if err := e.postResult(ctx, log, item, res); err != nil {
		log.Warn("kg-extraction: terminal-failure POST failed (non-fatal)", "err", err)
	}
}

// extract runs the per-observation emit→parse→validate loop and assembles the
// terminal result. It never returns an error — any failure is folded into the
// result so the caller can always POST something.
//
// Status rules (per the contract):
//   - every observation produced a (possibly empty) graph        → "ok"
//   - at least one succeeded AND at least one failed              → "partial"
//   - every observation failed (or no emitter could be built)     → "error"
func (e *Executor) extract(ctx context.Context, log *slog.Logger, item KgExtractWorkItem) KGExtractionResult {
	res := KGExtractionResult{
		BatchJobID:      item.BatchJobID,
		ContractVersion: KGExtractionContractVersion,
		Results:         []KGExtractionResultEntry{}, // never nil → serializes as []
		Status:          StatusError,                 // overwritten below
	}

	// No observations: nothing to extract. Report ok with an empty results slice
	// (the platform treats an empty batch as a no-op).
	if len(item.Observations) == 0 {
		res.Status = StatusOK
		return res
	}

	emitter, err := e.emitterFor(ctx, item)
	if err != nil {
		// No emitter ⇒ every observation fails. Report error with a summary so the
		// platform learns the item could not run on this host.
		log.Warn("kg-extraction: no emitter for item; reporting error", "err", err)
		res.Status = StatusError
		res.Error = fmt.Sprintf("emitter unavailable: %v", err)
		return res
	}

	var succeeded, failed int
	var firstErr string
	for _, obs := range item.Observations {
		graph, oerr := e.extractOne(ctx, emitter, item, obs)
		if oerr != nil {
			failed++
			if firstErr == "" {
				firstErr = fmt.Sprintf("observation %s: %v", obs.ID, oerr)
			}
			log.Warn("kg-extraction: observation emit/parse failed",
				"observationId", obs.ID, "err", oerr)
			continue
		}
		succeeded++
		res.Results = append(res.Results, KGExtractionResultEntry{
			ObservationID: obs.ID,
			Graph:         graph,
		})
	}

	switch {
	case failed == 0:
		res.Status = StatusOK
	case succeeded == 0:
		res.Status = StatusError
		res.Error = firstErr
	default:
		res.Status = StatusPartial
		res.Error = firstErr
	}

	log.Info("kg-extraction: extract complete",
		"status", string(res.Status),
		"succeeded", succeeded,
		"failed", failed,
		"resultEntries", len(res.Results),
	)
	return res
}

// extractOne runs ONE observation: a constrained provider emit, then parse +
// validate into a graph. A malformed emit gets exactly ONE bounded repair retry
// — the same system prompt, with a user turn that quotes the failure and the
// offending output back to the model — before the observation fails terminally.
//
// The retry is bounded at one on purpose. A single-shot completion that emits
// prose or a truncated object usually recovers when shown its own output; a
// model that fails twice is failing structurally, and looping would burn the
// whole batch's deadline on one observation. Every terminal failure carries the
// PARSE ERROR (and, on a second parse failure, both attempts' errors) into
// KGExtractionResult.Error, so the platform records WHY rather than a bare
// count — this lane's standing lesson is that an invisible failure is worse
// than a loud one.
//
// Each emit attempt is bounded by its own emitTimeout so a hung provider cannot
// wedge the loop; a retry therefore costs at most one additional emitTimeout.
func (e *Executor) extractOne(parentCtx context.Context, emitter Emitter, item KgExtractWorkItem, obs Observation) (ExtractedGraph, error) {
	raw, err := e.emitOnce(parentCtx, emitter, item.ExtractionSystemPrompt, obs.Content)
	if err != nil {
		return ExtractedGraph{}, err
	}
	graph, parseErr := parseGraph(raw)
	if parseErr == nil {
		return graph, nil
	}

	// One bounded repair retry.
	repaired, retryErr := e.emitOnce(parentCtx, emitter, item.ExtractionSystemPrompt, repairPrompt(obs.Content, raw, parseErr))
	if retryErr != nil {
		return ExtractedGraph{}, fmt.Errorf("parse emit: %w (repair retry emit failed: %v)", parseErr, retryErr)
	}
	graph, retryParseErr := parseGraph(repaired)
	if retryParseErr != nil {
		return ExtractedGraph{}, fmt.Errorf("parse emit after repair retry: %w (first attempt: %v)", retryParseErr, parseErr)
	}
	return graph, nil
}

// emitOnce runs a single emit attempt under its own emitTimeout.
func (e *Executor) emitOnce(parentCtx context.Context, emitter Emitter, systemPrompt, userContent string) (string, error) {
	ctx, cancel := context.WithTimeout(parentCtx, emitTimeout)
	defer cancel()
	return emitter.Emit(ctx, systemPrompt, userContent)
}

// repairPrompt builds the repair retry's user turn: the original observation,
// the output that failed to parse, and the parse error — with an explicit
// instruction to re-emit ONLY the JSON object. The bad output is quoted (rather
// than merely described) because the common failure is a wrapper the model added
// around otherwise-correct JSON, and showing it is what makes the second attempt
// converge.
func repairPrompt(observation, badOutput string, parseErr error) string {
	var b strings.Builder
	b.WriteString(observation)
	b.WriteString("\n\n---\nYour previous response could not be parsed as the required JSON object.\n")
	b.WriteString("Parse error: ")
	b.WriteString(parseErr.Error())
	b.WriteString("\n\nYour previous response was:\n")
	b.WriteString(truncateForPrompt(badOutput, emitRawErrExcerpt))
	b.WriteString("\n\nRespond again with ONLY the JSON object — no prose, no explanation, no Markdown code fences.")
	return b.String()
}

// truncateForPrompt bounds a model-supplied string embedded in a prompt or an
// error message.
func truncateForPrompt(s string, limit int) string {
	if len(s) <= limit {
		return s
	}
	return s[:limit] + "…(truncated)"
}

// emitterFor builds the Emitter for an item via the configured factory. When no
// factory is configured the executor has no way to run a real emit (a worker
// built without a provider) — return an error so the item reports status:error.
func (e *Executor) emitterFor(ctx context.Context, item KgExtractWorkItem) (Emitter, error) {
	if e.emitterFactory == nil {
		return nil, errors.New("kgextract: no emitter factory configured (worker has no provider)")
	}
	emitter, err := e.emitterFactory(ctx, item)
	if err != nil {
		return nil, err
	}
	if emitter == nil {
		return nil, errors.New("kgextract: emitter factory returned nil")
	}
	return emitter, nil
}

// resolveResultURL prefixes the item's ResultEndpoint PATH with the executor's
// platformBaseURL. An item that already carries an absolute URL (tests / a
// platform that emits a full URL) is used as-is.
func (e *Executor) resolveResultURL(endpoint string) string {
	if endpoint == "" {
		return ""
	}
	if strings.HasPrefix(endpoint, "http://") || strings.HasPrefix(endpoint, "https://") {
		return endpoint
	}
	if e.platformBaseURL == "" {
		return endpoint
	}
	if !strings.HasPrefix(endpoint, "/") {
		endpoint = "/" + endpoint
	}
	return e.platformBaseURL + endpoint
}

// postResult POSTs the KGExtractionResult to the item's resolved result URL with
// item.ResultAuth as the bearer token. Mirrors the detached-timeout, best-effort
// POST in codesurvival/executor.go.
func (e *Executor) postResult(parentCtx context.Context, log *slog.Logger, item KgExtractWorkItem, res KGExtractionResult) error {
	url := e.resolveResultURL(item.ResultEndpoint)
	if url == "" {
		return errors.New("kgextract: no result endpoint")
	}
	payload, err := json.Marshal(res)
	if err != nil {
		return fmt.Errorf("marshal result: %w", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), resultPostTimeout)
	defer cancel()
	go func() {
		select {
		case <-parentCtx.Done():
			cancel()
		case <-ctx.Done():
		}
	}()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(payload))
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+strings.TrimPrefix(item.ResultAuth, "Bearer "))

	resp, err := e.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("POST: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
	if resp.StatusCode >= 300 {
		return fmt.Errorf("non-2xx: %d", resp.StatusCode)
	}
	log.Debug("kg-extraction: result posted",
		"status", string(res.Status), "endpoint", url, "results", len(res.Results))
	return nil
}
