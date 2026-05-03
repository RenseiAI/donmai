package prompt

import (
	"bytes"
	"embed"
	"errors"
	"fmt"
	"strings"
	"sync"
	"text/template"
)

// Embedded default templates baked into the binary at build time. The
// .agentfactory/templates/ override path is reserved for F.5; today's
// builder always renders the embedded set.
//
//go:embed templates/*.tmpl
var defaultTemplates embed.FS

// WorkType names the work-type discriminant the renderer recognises.
// Unknown values fall through to [WorkTypeDevelopment] inside [Builder.Build]
// so the runner can keep dispatching without a panic when the platform
// adds a new work type.
type WorkType string

// Recognised work types. v0.5.0 ships these three; the legacy template
// surface (acceptance, refinement, backlog-creation, merge, ...) is
// deferred to F.5 per F.1.1 §1.
const (
	WorkTypeDevelopment WorkType = "development"
	WorkTypeQA          WorkType = "qa"
	WorkTypeResearch    WorkType = "research"
)

// ErrEmptyWork is returned by [Builder.Build] when the QueuedWork
// payload carries no usable issue context (no PromptContext, no Body,
// no IssueIdentifier). The runner treats this as a permanent dispatch
// failure rather than retrying.
var ErrEmptyWork = errors.New("prompt: queued work carries no issue context")

// Builder composes (system, user) prompt pairs from a [QueuedWork].
//
// The zero value is ready to use:
//
//	var b prompt.Builder
//	system, user, err := b.Build(qw)
//
// Builders are safe for concurrent use; all internal state is the
// parsed template set, which is read-only after construction.
type Builder struct {
	// SystemAppend is the optional repository-specific instruction
	// block appended to the system prompt. Mirrors
	// RepositoryConfig.systemPrompt.append from the legacy TS.
	SystemAppend string

	// templates is lazily parsed on first use to keep the zero value
	// useful. Accessed only via [Builder.set] under tmplOnce.
	templates *template.Template

	// tmplOnce guards lazy parsing of [Builder.templates]; the
	// embedded set is read-only after parsing, so subsequent reads need
	// no further synchronisation.
	tmplOnce sync.Once
	tmplErr  error
}

// NewBuilder returns a Builder pre-configured with the embedded default
// templates. Equivalent to the zero value plus the optional
// [Builder.SystemAppend] hook; call sites that need no append may use
// the literal `prompt.Builder{}` instead.
func NewBuilder() *Builder {
	return &Builder{}
}

// Build renders the (system, user) prompt pair for qw.
//
// The system prompt is identical across work types — it is the
// runner's identity + operating-rules block, optionally augmented with
// [Builder.SystemAppend]. The user prompt is selected by [WorkType]
// (development | qa | research). Unknown work types fall through to
// the development template so a newly-added platform-side work type
// does not crash the runner; the platform side is responsible for
// gating which work types reach the Go runner.
//
// Build is deterministic: given the same inputs (including the same
// [Builder] state) it produces byte-identical output. The
// golden-file tests in builder_test.go assert this property.
//
// Stage-prompt mode (REN-1485 / REN-1487): when qw.StagePrompt is
// non-empty the runner is being dispatched by the new
// `agent.dispatch_stage` action. The user prompt body is taken from
// StagePrompt verbatim — pre-rendered platform-side from the stage
// prompt template + issue context — and the embedded user template is
// skipped. The system prompt is still rendered (carries the runner
// identity + operating rules) and a stage-context preamble is
// prepended so the agent can self-identify which stage it is running
// (`stageId=… budget.maxSubAgents=… budget.maxTokens=… budget.maxDurationSeconds=…`).
// The legacy template path is preserved when StagePrompt is empty
// (cardinal rule 1 — additive, no break).
func (b *Builder) Build(qw QueuedWork) (system, user string, err error) {
	hasStagePrompt := strings.TrimSpace(qw.StagePrompt) != ""
	if !hasStagePrompt && !hasIssueContext(qw) {
		return "", "", fmt.Errorf("%w: sessionId=%q identifier=%q",
			ErrEmptyWork, qw.SessionID, qw.IssueIdentifier)
	}

	tmpls, err := b.set()
	if err != nil {
		return "", "", err
	}

	systemBuf, err := renderTemplate(tmpls, "system_base.tmpl", systemTemplateData(qw, b.SystemAppend))
	if err != nil {
		return "", "", fmt.Errorf("render system prompt: %w", err)
	}

	if hasStagePrompt {
		// Stage-prompt mode — use platform-rendered prompt verbatim
		// with a stage-context preamble so the agent can self-identify
		// the stage + budget.
		userBuf := renderStagePromptUser(qw)
		return systemBuf, userBuf, nil
	}

	userTmpl := userTemplateName(WorkType(qw.WorkType))
	userBuf, err := renderTemplate(tmpls, userTmpl, userTemplateData(qw))
	if err != nil {
		return "", "", fmt.Errorf("render user prompt %q: %w", userTmpl, err)
	}

	return systemBuf, userBuf, nil
}

// renderStagePromptUser composes the user-prompt body for stage-prompt
// dispatch. The platform-rendered StagePrompt is prepended with a
// short context block identifying the stage id + the budget the
// runner is enforcing — surfaces what the agent should know about the
// limits it operates under without forcing it to scrape the env.
func renderStagePromptUser(qw QueuedWork) string {
	preamble := stagePreamble(qw)
	body := strings.TrimRight(qw.StagePrompt, "\n")
	if preamble == "" {
		return body
	}
	return preamble + "\n\n" + body
}

// stagePreamble returns the "Stage: X — Budget: Y" block prepended to
// the stage prompt body. Returns the empty string when no stage
// metadata is available (defensive — the dispatcher always sets at
// least StageID).
func stagePreamble(qw QueuedWork) string {
	if qw.StageID == "" && qw.StageBudget == nil {
		return ""
	}
	var lines []string
	if qw.StageID != "" {
		lines = append(lines, fmt.Sprintf("<stage>%s</stage>", qw.StageID))
	}
	if b := qw.StageBudget; b != nil {
		lines = append(lines, fmt.Sprintf(
			"<stageBudget maxDurationSeconds=%q maxSubAgents=%q maxTokens=%q />",
			fmt.Sprintf("%d", b.MaxDurationSeconds),
			fmt.Sprintf("%d", b.MaxSubAgents),
			fmt.Sprintf("%d", b.MaxTokens),
		))
	}
	if qw.StageSourceEventID != "" {
		lines = append(lines, fmt.Sprintf("<stageSourceEventId>%s</stageSourceEventId>", qw.StageSourceEventID))
	}
	return strings.Join(lines, "\n")
}

// set returns the parsed template set, parsing it on first use. It is
// idempotent and safe to call concurrently — repeated calls return the
// same parsed set without re-parsing.
func (b *Builder) set() (*template.Template, error) {
	b.tmplOnce.Do(func() {
		t, err := template.ParseFS(defaultTemplates, "templates/*.tmpl")
		if err != nil {
			b.tmplErr = fmt.Errorf("parse embedded templates: %w", err)
			return
		}
		b.templates = t
	})
	return b.templates, b.tmplErr
}

// systemTmplData carries the variable set the system_base.tmpl
// references. Kept unexported because it is internal renderer plumbing;
// callers shape the Builder via [QueuedWork] and [Builder.SystemAppend].
type systemTmplData struct {
	SessionID      string
	OrganizationID string
	ProjectName    string
	Repository     string
	Ref            string
	Append         string
}

func systemTemplateData(qw QueuedWork, appendBlock string) systemTmplData {
	return systemTmplData{
		SessionID:      strings.TrimSpace(qw.SessionID),
		OrganizationID: strings.TrimSpace(qw.OrganizationID),
		ProjectName:    strings.TrimSpace(qw.ProjectName),
		Repository:     strings.TrimSpace(qw.Repository),
		Ref:            strings.TrimSpace(qw.Ref),
		Append:         strings.TrimSpace(appendBlock),
	}
}

// userTmplData carries the variable set every user_*.tmpl references.
type userTmplData struct {
	IssueIdentifier string
	Repository      string
	Ref             string
	Context         string
	MentionContext  string
	ParentContext   string
}

func userTemplateData(qw QueuedWork) userTmplData {
	return userTmplData{
		IssueIdentifier: strings.TrimSpace(qw.IssueIdentifier),
		Repository:      strings.TrimSpace(qw.Repository),
		Ref:             strings.TrimSpace(qw.Ref),
		Context:         strings.TrimSpace(resolveContext(qw)),
		MentionContext:  strings.TrimSpace(qw.MentionContext),
		ParentContext:   strings.TrimSpace(qw.ParentContext),
	}
}

// resolveContext picks the best available issue-context string from the
// QueuedWork: PromptContext (the platform-rendered XML envelope) wins;
// otherwise we synthesize a minimal fallback from Title + Body so the
// agent still has something to work with.
func resolveContext(qw QueuedWork) string {
	if strings.TrimSpace(qw.PromptContext) != "" {
		return qw.PromptContext
	}
	var lines []string
	if qw.IssueIdentifier != "" || qw.Title != "" {
		lines = append(lines, fmt.Sprintf("Issue %s — %s",
			qw.IssueIdentifier, qw.Title))
	}
	if qw.Body != "" {
		lines = append(lines, "", qw.Body)
	}
	return strings.Join(lines, "\n")
}

// userTemplateName maps a WorkType to the user template filename. New
// work types route to the development template — the platform side is
// responsible for gating which work types reach the Go runner, but the
// runner must never crash on an unrecognised value.
func userTemplateName(w WorkType) string {
	switch w {
	case WorkTypeQA:
		return "user_qa.tmpl"
	case WorkTypeResearch:
		return "user_research.tmpl"
	case WorkTypeDevelopment, "":
		return "user_development.tmpl"
	default:
		return "user_development.tmpl"
	}
}

func hasIssueContext(qw QueuedWork) bool {
	return strings.TrimSpace(qw.PromptContext) != "" ||
		strings.TrimSpace(qw.Body) != "" ||
		strings.TrimSpace(qw.IssueIdentifier) != ""
}

// renderTemplate executes name against tmpls with data and returns a
// trimmed string. The trailing newline trim keeps golden-file diffs
// stable across editors that auto-strip end-of-file whitespace.
func renderTemplate(tmpls *template.Template, name string, data any) (string, error) {
	var buf bytes.Buffer
	if err := tmpls.ExecuteTemplate(&buf, name, data); err != nil {
		return "", err
	}
	return strings.TrimRight(buf.String(), "\n"), nil
}
