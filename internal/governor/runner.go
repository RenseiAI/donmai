package governor

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	"github.com/RenseiAI/agentfactory-tui/internal/linear"
	"github.com/RenseiAI/agentfactory-tui/internal/queue"
)

// issuePayload is the JSON envelope sent to the work queue.
type issuePayload struct {
	IssueID    string `json:"issueId"`
	Identifier string `json:"identifier"`
	Project    string `json:"project"`
	Phase      string `json:"phase"`
}

// linearScanner is the narrow Linear interface used by the governor.
// It only requires the three methods the scan loop actually calls,
// so test mocks and the full *linear.Client both satisfy it.
type linearScanner interface {
	ListIssuesByProject(ctx context.Context, projectName string, states []string) ([]linear.Issue, error)
	GetIssue(ctx context.Context, id string) (*linear.Issue, error)
	ListSubIssues(ctx context.Context, parentID string) ([]linear.Issue, error)
}

// Runner polls Linear projects and enqueues issues for automated processing.
type Runner struct {
	cfg    Config
	linear linearScanner
	queue  queue.Queue
	logger *slog.Logger
}

// NewRunner constructs a Runner, validates cfg, and defaults logger to
// slog.Default() when nil.
func NewRunner(cfg Config, lin linearScanner, q queue.Queue, logger *slog.Logger) (*Runner, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &Runner{
		cfg:    cfg,
		linear: lin,
		queue:  q,
		logger: logger,
	}, nil
}

// Run executes the governor scan loop.
//
// If cfg.Once is true, exactly one scan is performed and Run returns nil
// (or ctx.Err() if the context is already cancelled).
//
// Otherwise Run loops on a time.Ticker until ctx is cancelled, returning
// ctx.Err().
//
// Errors from individual projects are logged via slog and do NOT cause Run to
// return early — only context cancellation or a startup validation failure
// will terminate the loop.
//
// TODO (event-driven): subscribe to Redis pub/sub channel so that the runner
// can react to work-item events in addition to periodic polling.  For now,
// ModeEventDriven is treated identically to ModePollOnly.
func (r *Runner) Run(ctx context.Context) error {
	if err := r.cfg.Validate(); err != nil {
		return err
	}

	if r.cfg.Once {
		if err := ctx.Err(); err != nil {
			return err
		}
		r.scan(ctx)
		return nil
	}

	ticker := time.NewTicker(r.cfg.ScanInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			r.scan(ctx)
		}
	}
}

// scan iterates every configured project, fetches open issues from Linear, and
// enqueues dispatch candidates up to MaxDispatches.
func (r *Runner) scan(ctx context.Context) {
	for _, project := range r.cfg.Projects {
		if err := ctx.Err(); err != nil {
			return
		}
		r.scanProject(ctx, project)
	}
}

// scanProject fetches issues for a single project and enqueues dispatch
// candidates.  Errors are logged and do not bubble up.
func (r *Runner) scanProject(ctx context.Context, project string) {
	start := time.Now()

	issues, err := r.linear.ListIssuesByProject(ctx, project, []string{"Backlog", "Started"})
	if err != nil {
		r.logger.Error("governor scan project error",
			"project", project,
			"error", fmt.Errorf("scan project %q: %w", project, err),
		)
		return
	}

	dispatched := 0

	for _, issue := range issues {
		if dispatched >= r.cfg.MaxDispatches {
			break
		}

		decision, reason := Decide(issue, r.cfg)

		if decision != DecisionDispatch {
			r.logger.Debug("governor decision",
				"issue", issue.Identifier,
				"decision", "skip",
				"reason", reason,
			)
			continue
		}

		payload, marshalErr := json.Marshal(issuePayload{
			IssueID:    issue.ID,
			Identifier: issue.Identifier,
			Project:    issue.Project.Name,
			Phase:      reason,
		})
		if marshalErr != nil {
			r.logger.Error("governor marshal payload",
				"issue", issue.Identifier,
				"error", marshalErr,
			)
			continue
		}

		if enqErr := r.queue.Enqueue(ctx, payload); enqErr != nil {
			r.logger.Error("governor enqueue error",
				"issue", issue.Identifier,
				"error", fmt.Errorf("scan project %q: %w", project, enqErr),
			)
			continue
		}

		if _, incrErr := r.queue.IncrDispatchCounter(ctx, project); incrErr != nil {
			r.logger.Error("governor incr counter error",
				"project", project,
				"error", incrErr,
			)
		}

		r.logger.Debug("governor decision",
			"issue", issue.Identifier,
			"decision", "dispatch",
			"reason", reason,
		)

		dispatched++
	}

	r.logger.Info("governor scan",
		"project", project,
		"dispatched", dispatched,
		"duration", time.Since(start),
	)
}
