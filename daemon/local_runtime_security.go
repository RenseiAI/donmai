package daemon

import (
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"time"

	"github.com/RenseiAI/donmai/agent"
	"github.com/RenseiAI/donmai/executioncell"
	"github.com/RenseiAI/donmai/internal/localqueue"
)

func localSecurityRevision(levels agent.ExecutionSecurityLevels) (string, error) {
	return executioncell.DigestContractValue(struct {
		Schema string
		Levels agent.ExecutionSecurityLevels
	}{LocalRuntimeConfigAPIVersion, levels})
}

// NewLocalExecutionSecurityStamp stamps explicit own configuration. It never
// obtains a level from a missing policy, session flag, or compatibility reader.
func NewLocalExecutionSecurityStamp(scopeID string, policy *LocalExecutionSecurity, at time.Time) (*agent.ExecutionSecurity, error) {
	if strings.TrimSpace(scopeID) == "" || at.IsZero() {
		return nil, errors.New("local stamp requires authority scope and resolution time")
	}
	if err := policy.Validate(); err != nil {
		return nil, err
	}
	levels := policy.Levels()
	revision, err := localSecurityRevision(levels)
	if err != nil {
		return nil, err
	}
	sources := map[agent.ExecutionSecurityDimension]string{}
	for _, dimension := range agent.ExecutionSecurityDimensions() {
		sources[dimension] = "local-config:" + scopeID
	}
	return &agent.ExecutionSecurity{Version: agent.ExecutionSecurityWireVersion, Levels: levels, Sources: sources, Digest: agent.ExecutionSecurityLevelsDigest(levels), RulesetRevision: revision, ResolvedAt: at.UTC().Format(time.RFC3339Nano)}, nil
}

// ValidateLocalExecutionSecurityEvidence checks the local issuer's own stamp
// and the actual compiled report. Peer omission rules cannot mint local policy.
func ValidateLocalExecutionSecurityEvidence(scopeID string, payload, hostRaw json.RawMessage) error {
	stamp, err := agent.ExecutionSecurityFromOperationalPayload(payload)
	if err != nil {
		return err
	}
	if stamp == nil {
		return &agent.ExecutionSecurityError{Code: agent.ExecutionSecurityUnresolvable, Detail: "local issued session has no execution-security stamp"}
	}
	revision, err := localSecurityRevision(stamp.Levels)
	if err != nil {
		return err
	}
	if stamp.Digest != agent.ExecutionSecurityLevelsDigest(stamp.Levels) || stamp.RulesetRevision != revision || len(stamp.Sources) != len(agent.ExecutionSecurityDimensions()) || stamp.ParentSessionID != "" {
		return &agent.ExecutionSecurityError{Code: agent.ExecutionSecurityUnresolvable, Detail: "local execution-security stamp has invalid own-scope provenance"}
	}
	for _, dimension := range agent.ExecutionSecurityDimensions() {
		if stamp.Sources[dimension] != "local-config:"+scopeID {
			return &agent.ExecutionSecurityError{Code: agent.ExecutionSecurityUnresolvable, Dimension: dimension, Detail: "local execution-security source disagrees with admitted authority"}
		}
	}
	if _, err = time.Parse(time.RFC3339Nano, stamp.ResolvedAt); err != nil {
		return &agent.ExecutionSecurityError{Code: agent.ExecutionSecurityUnresolvable, Detail: "local execution-security stamp has no resolution time"}
	}
	host, err := executioncell.DecodeHostAdaptationReceipt(hostRaw)
	if err != nil {
		return fmt.Errorf("local security host receipt: %w", err)
	}
	var plan agent.PreparedHarness
	if err = json.Unmarshal(host.Plan, &plan); err != nil {
		return err
	}
	if plan.ExecutionSecurity == nil || agent.DigestPreparedHarness(&plan) != host.PlanDigest {
		return &agent.ExecutionSecurityError{Code: agent.ExecutionSecurityReceiptUnmet, Detail: "local host did not retain the actual stamped security report"}
	}
	return agent.ExecutionSecurityReportMeets(plan.ExecutionSecurity, stamp.Levels)
}

func (l *localRuntime) checkCurrentConfiguration() error {
	// A rejected watcher reload cannot silently keep an old own policy usable
	// forever. Authority transitions re-read the selected file and refuse errors.
	current, err := LoadConfig(l.daemon.opts.ConfigPath)
	if err != nil {
		return err
	}
	if err = ValidateLocalExecutionSecurity(current); err != nil {
		return err
	}
	applied := l.daemon.Config()
	if applied == nil || applied.APIVersion != current.APIVersion || applied.Orchestrator.URL != current.Orchestrator.URL || !sameLocalRuntimeConfiguration(applied.LocalRuntime, current.LocalRuntime) {
		return errors.New("local authority configuration changed; wait for a valid applied configuration")
	}
	return nil
}

func sameLocalRuntimeConfiguration(left, right *LocalRuntimeConfig) bool {
	if left == nil || right == nil {
		return left == right
	}
	a, b := *left, *right
	a.Repositories, b.Repositories = nil, nil
	return reflect.DeepEqual(a, b) && slices.Equal(left.Repositories, right.Repositories)
}

// validateCurrentAdmissionPolicy runs under the caller's policy read lease.
// The immutable admitted stamp must still meet every current own-scope minimum;
// a changed policy can hold an admission, but cannot rewrite its authority.
func (l *localRuntime) validateCurrentAdmissionPolicy(payload, host json.RawMessage) error {
	if err := l.checkCurrentConfiguration(); err != nil {
		return err
	}
	if err := ValidateLocalExecutionSecurityEvidence(l.identity.ScopeID, payload, host); err != nil {
		return err
	}
	stamp, err := agent.ExecutionSecurityFromOperationalPayload(payload)
	if err != nil {
		return err
	}
	cfg := l.daemon.Config()
	if err = ValidateLocalExecutionSecurity(cfg); err != nil {
		return err
	}
	return localAdmissionMeetsPolicy(stamp.Levels, cfg.LocalRuntime.ExecutionSecurity.Levels())
}

func localAdmissionMeetsPolicy(admittedLevels, current agent.ExecutionSecurityLevels) error {
	if err := admittedLevels.Validate(); err != nil {
		return err
	}
	if err := current.Validate(); err != nil {
		return err
	}
	for _, dimension := range agent.ExecutionSecurityDimensions() {
		required, _ := agent.ExecutionSecurityLevelIndex(dimension, current.Level(dimension))
		admitted, _ := agent.ExecutionSecurityLevelIndex(dimension, admittedLevels.Level(dimension))
		if admitted < required {
			return &agent.ExecutionSecurityError{Code: agent.ExecutionSecurityUnmet, Dimension: dimension, Level: current.Level(dimension), Detail: "immutable local admission no longer meets the current operator policy; execution held"}
		}
	}
	return nil
}

func (l *localRuntime) registerLaunchPermit(attempt localqueue.AttemptRef) (*localLaunchPermit, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.launchPermits == nil {
		l.launchPermits = map[string]*localLaunchPermit{}
	}
	if l.closed || l.launchPermits[attempt.SessionID] != nil {
		return nil, errors.New("local launch permit is unavailable")
	}
	permit := &localLaunchPermit{attempt: attempt}
	l.launchPermits[attempt.SessionID] = permit
	return permit, nil
}

func (l *localRuntime) retireLaunchPermit(permit *localLaunchPermit) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.launchPermits[permit.attempt.SessionID] == permit {
		delete(l.launchPermits, permit.attempt.SessionID)
	}
}

func (l *localRuntime) matchesLaunchPermit(permit *localLaunchPermit, attempt localqueue.AttemptRef) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return permit != nil && permit.attempt == attempt && l.launchPermits[attempt.SessionID] == permit
}

func (l *localRuntime) currentLaunchPermit(attempt localqueue.AttemptRef) *localLaunchPermit {
	l.mu.Lock()
	defer l.mu.Unlock()
	permit := l.launchPermits[attempt.SessionID]
	if permit == nil || permit.attempt != attempt {
		return nil
	}
	return permit
}
