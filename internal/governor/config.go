// Package governor implements the in-process scan/dispatch loop that backs
// the `af governor start` command. The Runner accepts injected
// linear.Linear and queue.Queue dependencies so it can be unit-tested
// without real I/O.
package governor

import (
	"errors"
	"time"
)

// Mode controls how the governor reacts to work-items.
type Mode string

const (
	// ModeEventDriven triggers scans on Redis pub/sub events (TODO: subscribe
	// to Redis pub/sub channel — out of scope for this initial port).
	// For now the runner treats event-driven identically to poll-only.
	ModeEventDriven Mode = "event-driven"

	// ModePollOnly triggers scans on a fixed ticker interval.
	ModePollOnly Mode = "poll-only"
)

// Config holds the runtime configuration for a Governor Runner.
type Config struct {
	// Projects is the list of Linear project names to scan.
	Projects []string

	// ScanInterval is how often the runner polls Linear for work.
	ScanInterval time.Duration

	// MaxDispatches caps how many issues may be enqueued per scan cycle.
	MaxDispatches int

	// Once causes the runner to perform exactly one scan and then exit.
	Once bool

	// Mode selects the dispatch trigger strategy.
	Mode Mode

	// AutoResearch enables automatic dispatch of issues in the "Triage" phase.
	AutoResearch bool // default true

	// AutoBacklogCreation enables automatic dispatch of backlog-creation issues.
	AutoBacklogCreation bool // default true

	// AutoDevelopment enables automatic dispatch of development-phase issues.
	AutoDevelopment bool // default true

	// AutoQA enables automatic dispatch of QA-phase issues.
	AutoQA bool // default true

	// AutoAcceptance enables automatic dispatch of acceptance-phase issues.
	AutoAcceptance bool // default true
}

// DefaultConfig returns a Config with sensible defaults.
func DefaultConfig() Config {
	return Config{
		ScanInterval:        5 * time.Minute,
		MaxDispatches:       10,
		Mode:                ModePollOnly,
		AutoResearch:        true,
		AutoBacklogCreation: true,
		AutoDevelopment:     true,
		AutoQA:              true,
		AutoAcceptance:      true,
	}
}

// Validate returns an error if the configuration is invalid.
func (c Config) Validate() error {
	if len(c.Projects) == 0 {
		return errors.New("governor: Projects must not be empty")
	}
	if c.MaxDispatches <= 0 {
		return errors.New("governor: MaxDispatches must be > 0")
	}
	if c.ScanInterval <= 0 {
		return errors.New("governor: ScanInterval must be > 0")
	}
	switch c.Mode {
	case ModeEventDriven, ModePollOnly:
		// valid
	default:
		return errors.New("governor: Mode must be \"event-driven\" or \"poll-only\"")
	}
	return nil
}
