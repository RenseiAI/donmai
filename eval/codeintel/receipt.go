package codeintel

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"sync"

	"github.com/RenseiAI/donmai/eval/experiment"
)

const (
	promptReceiptEventExecutionCompleted = "execution_completed"
	promptReceiptEventPlatformPosted     = "platform_posted"

	// PromptExperimentDispositionCompleted identifies a successful provider execution.
	PromptExperimentDispositionCompleted = "completed"
	// PromptExperimentDispositionExecutionError identifies a provider execution that returned an error.
	PromptExperimentDispositionExecutionError = "execution_error"
)

// PromptExperimentCostCompleteness records whether the known cost is a final
// provider total, a partial known amount, or entirely unavailable.
type PromptExperimentCostCompleteness string

const (
	// PromptExperimentCostComplete means every started phase reported its terminal provider total.
	PromptExperimentCostComplete PromptExperimentCostCompleteness = "complete"
	// PromptExperimentCostPartial means some provider cost is known but the terminal total is incomplete.
	PromptExperimentCostPartial PromptExperimentCostCompleteness = "partial"
	// PromptExperimentCostMissing means no provider cost was reported.
	PromptExperimentCostMissing PromptExperimentCostCompleteness = "missing"
)

// PromptExperimentReceiptStatus is the latest durable journal state for one
// immutable experiment trial identity.
type PromptExperimentReceiptStatus string

const (
	// PromptExperimentReceiptMissing means no durable event exists for the trial.
	PromptExperimentReceiptMissing PromptExperimentReceiptStatus = ""
	// PromptExperimentReceiptExecutionCompleted means provider evidence is durable but not acknowledged by the platform.
	PromptExperimentReceiptExecutionCompleted PromptExperimentReceiptStatus = promptReceiptEventExecutionCompleted
	// PromptExperimentReceiptPlatformPosted means the platform acknowledgement is also durable.
	PromptExperimentReceiptPlatformPosted PromptExperimentReceiptStatus = promptReceiptEventPlatformPosted
)

// PromptExperimentReceiptIdentity names one immutable case x arm x trial. It
// contains only safe identity fields; prompt bytes, filesystem paths, bearer
// tokens, and provider output are deliberately absent.
type PromptExperimentReceiptIdentity struct {
	ExperimentID          string
	CaseID                string
	Arm                   Arm
	SubjectRef            string
	VariantRef            string
	TrialIndex            int
	InvocationScopeDigest string
	ReceiptID             string
}

// PromptExperimentReceipt is sanitized execution evidence appended to the
// local JSONL journal whenever a prompt provider execution returns, including
// partial or missing-cost outcomes.
type PromptExperimentReceipt struct {
	ExperimentID          string `json:"experimentId"`
	CaseID                string `json:"caseId"`
	Arm                   Arm    `json:"arm"`
	SubjectRef            string `json:"subjectRef"`
	VariantRef            string `json:"variantRef"`
	TrialIndex            int    `json:"trialIndex"`
	InvocationScopeDigest string `json:"invocationScopeDigest"`
	ReceiptID             string `json:"receiptId"`
	Disposition           string `json:"disposition"`
	// PostedRunID is the sanitized platform acknowledgement. It is absent from
	// execution_completed and required only on platform_posted.
	PostedRunID      string                           `json:"postedRunId,omitempty"`
	CostUSD          float64                          `json:"costUsd"`
	CostCompleteness PromptExperimentCostCompleteness `json:"costCompleteness"`
	TurnCount        int                              `json:"turnCount"`
	TokenCounts      TokenCounts                      `json:"tokenCounts"`
}

// Identity returns the immutable lookup key carried by the receipt.
func (r PromptExperimentReceipt) Identity() PromptExperimentReceiptIdentity {
	return PromptExperimentReceiptIdentity{
		ExperimentID:          r.ExperimentID,
		CaseID:                r.CaseID,
		Arm:                   r.Arm,
		SubjectRef:            r.SubjectRef,
		VariantRef:            r.VariantRef,
		TrialIndex:            r.TrialIndex,
		InvocationScopeDigest: r.InvocationScopeDigest,
		ReceiptID:             r.ReceiptID,
	}
}

// PromptExperimentReceiptState is the indexed durable state for one trial.
type PromptExperimentReceiptState struct {
	Status  PromptExperimentReceiptStatus
	Receipt PromptExperimentReceipt
}

// PromptExperimentReceiptJournal is the ordering/idempotency seam used by the
// driver. Implementations must make each Record call durable before returning.
type PromptExperimentReceiptJournal interface {
	Lookup(PromptExperimentReceiptIdentity) (PromptExperimentReceiptState, error)
	RecordExecutionCompleted(PromptExperimentReceipt) error
	RecordPlatformPosted(PromptExperimentReceipt) error
}

type promptExperimentReceiptLine struct {
	Event string `json:"event"`
	PromptExperimentReceipt
}

// PromptExperimentReceiptLedger is an append-only, fsync-per-event JSONL
// journal. It holds an exclusive companion lock while open and indexes existing
// lines so retries can refuse duplicate spend before provider execution.
type PromptExperimentReceiptLedger struct {
	mu       sync.Mutex
	root     *os.Root
	file     *os.File
	path     string
	lockName string
	states   map[string]PromptExperimentReceiptState
	closed   bool
}

// OpenPromptExperimentReceiptLedger opens path once in append mode and indexes
// every existing event. Malformed, conflicting, or out-of-order history fails
// closed before a live driver is created.
func OpenPromptExperimentReceiptLedger(path string) (*PromptExperimentReceiptLedger, error) {
	path = strings.TrimSpace(path)
	if path == "" {
		return nil, fmt.Errorf("prompt receipt ledger path is required")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("create prompt receipt ledger directory: %w", err)
	}
	root, err := os.OpenRoot(filepath.Dir(path))
	if err != nil {
		return nil, fmt.Errorf("open prompt receipt ledger directory for %q: %w", path, err)
	}
	fileName := filepath.Base(path)
	lockName := fileName + ".lock"
	lockPath := path + ".lock"
	lock, err := root.OpenFile(lockName, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		_ = root.Close()
		if os.IsExist(err) {
			return nil, fmt.Errorf("prompt receipt ledger %q is locked by another process", path)
		}
		return nil, fmt.Errorf("create prompt receipt ledger lock %q: %w", lockPath, err)
	}
	if err := lock.Close(); err != nil {
		return nil, errors.Join(
			fmt.Errorf("close prompt receipt ledger lock %q: %w", lockPath, err),
			releasePromptReceiptLock(root, lockName, lockPath),
			root.Close(),
		)
	}

	f, err := root.OpenFile(fileName, os.O_CREATE|os.O_RDWR|os.O_APPEND, 0o600)
	if err != nil {
		return nil, errors.Join(
			fmt.Errorf("open prompt receipt ledger %q: %w", path, err),
			releasePromptReceiptLock(root, lockName, lockPath),
			root.Close(),
		)
	}
	if err := f.Chmod(0o600); err != nil {
		return nil, errors.Join(
			fmt.Errorf("secure prompt receipt ledger %q: %w", path, err),
			f.Close(), releasePromptReceiptLock(root, lockName, lockPath), root.Close(),
		)
	}
	ledger := &PromptExperimentReceiptLedger{
		root: root, file: f, path: path, lockName: lockName,
		states: map[string]PromptExperimentReceiptState{},
	}
	if err := ledger.load(); err != nil {
		return nil, errors.Join(
			err,
			f.Close(),
			releasePromptReceiptLock(root, lockName, lockPath),
			root.Close(),
		)
	}
	return ledger, nil
}

func releasePromptReceiptLock(root *os.Root, name, displayPath string) error {
	if root == nil || name == "" {
		return nil
	}
	if err := root.Remove(name); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("remove prompt receipt ledger lock %q: %w", displayPath, err)
	}
	return nil
}

func (l *PromptExperimentReceiptLedger) load() error {
	if _, err := l.file.Seek(0, io.SeekStart); err != nil {
		return fmt.Errorf("seek prompt receipt ledger %q: %w", l.path, err)
	}
	sc := bufio.NewScanner(l.file)
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
	lineNo := 0
	for sc.Scan() {
		lineNo++
		if len(bytes.TrimSpace(sc.Bytes())) == 0 {
			continue
		}
		var line promptExperimentReceiptLine
		dec := json.NewDecoder(bytes.NewReader(sc.Bytes()))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&line); err != nil {
			return fmt.Errorf("decode prompt receipt ledger %q line %d: %w", l.path, lineNo, err)
		}
		if err := requireReceiptJSONEOF(dec); err != nil {
			return fmt.Errorf("decode prompt receipt ledger %q line %d: %w", l.path, lineNo, err)
		}
		if err := validatePromptExperimentReceipt(line.PromptExperimentReceipt); err != nil {
			return fmt.Errorf("validate prompt receipt ledger %q line %d: %w", l.path, lineNo, err)
		}
		if err := l.applyLoaded(line); err != nil {
			return fmt.Errorf("index prompt receipt ledger %q line %d: %w", l.path, lineNo, err)
		}
	}
	if err := sc.Err(); err != nil {
		return fmt.Errorf("scan prompt receipt ledger %q: %w", l.path, err)
	}
	return nil
}

func requireReceiptJSONEOF(dec *json.Decoder) error {
	var extra any
	if err := dec.Decode(&extra); err == io.EOF {
		return nil
	} else if err != nil {
		return err
	}
	return fmt.Errorf("multiple JSON values are not allowed")
}

func (l *PromptExperimentReceiptLedger) applyLoaded(line promptExperimentReceiptLine) error {
	id := line.ReceiptID
	current, exists := l.states[id]
	switch line.Event {
	case promptReceiptEventExecutionCompleted:
		if line.PostedRunID != "" {
			return fmt.Errorf("execution_completed receipt %q must not include a posted run id", id)
		}
		if exists {
			return fmt.Errorf("duplicate execution_completed event for receipt %q", id)
		}
		l.states[id] = PromptExperimentReceiptState{Status: PromptExperimentReceiptExecutionCompleted, Receipt: line.PromptExperimentReceipt}
	case promptReceiptEventPlatformPosted:
		if !exists || current.Status != PromptExperimentReceiptExecutionCompleted {
			return fmt.Errorf("platform_posted event for receipt %q has no preceding execution_completed event", id)
		}
		if strings.TrimSpace(line.PostedRunID) == "" {
			return fmt.Errorf("platform_posted receipt %q requires a non-empty posted run id", id)
		}
		if line.CostCompleteness != PromptExperimentCostComplete {
			return fmt.Errorf("platform_posted receipt %q requires complete provider cost", id)
		}
		if line.Disposition != PromptExperimentDispositionCompleted {
			return fmt.Errorf("platform_posted receipt %q has disposition %q", id, line.Disposition)
		}
		if !samePromptExecutionEvidence(current.Receipt, line.PromptExperimentReceipt) {
			return fmt.Errorf("platform_posted receipt %q does not match execution_completed evidence", id)
		}
		l.states[id] = PromptExperimentReceiptState{Status: PromptExperimentReceiptPlatformPosted, Receipt: line.PromptExperimentReceipt}
	default:
		return fmt.Errorf("unknown receipt event %q", line.Event)
	}
	return nil
}

// Lookup returns the current state for identity. A reused receipt id with
// different immutable fields is rejected rather than treated as a replay.
func (l *PromptExperimentReceiptLedger) Lookup(identity PromptExperimentReceiptIdentity) (PromptExperimentReceiptState, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.closed {
		return PromptExperimentReceiptState{}, fmt.Errorf("prompt receipt ledger is closed")
	}
	state, ok := l.states[identity.ReceiptID]
	if !ok {
		return PromptExperimentReceiptState{Status: PromptExperimentReceiptMissing}, nil
	}
	if !reflect.DeepEqual(state.Receipt.Identity(), identity) {
		return PromptExperimentReceiptState{}, fmt.Errorf("receipt id %q is already bound to different immutable trial identity", identity.ReceiptID)
	}
	return state, nil
}

// RecordExecutionCompleted appends and fsyncs provider execution evidence.
func (l *PromptExperimentReceiptLedger) RecordExecutionCompleted(receipt PromptExperimentReceipt) error {
	if receipt.Disposition == "" {
		receipt.Disposition = PromptExperimentDispositionCompleted
	}
	if receipt.PostedRunID != "" {
		return fmt.Errorf("execution_completed receipt %q must not include a posted run id", receipt.ReceiptID)
	}
	if err := validatePromptExperimentReceipt(receipt); err != nil {
		return err
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.closed {
		return fmt.Errorf("prompt receipt ledger is closed")
	}
	if _, exists := l.states[receipt.ReceiptID]; exists {
		return fmt.Errorf("receipt %q already exists", receipt.ReceiptID)
	}
	if err := l.appendAndSync(promptExperimentReceiptLine{Event: promptReceiptEventExecutionCompleted, PromptExperimentReceipt: receipt}); err != nil {
		return err
	}
	l.states[receipt.ReceiptID] = PromptExperimentReceiptState{Status: PromptExperimentReceiptExecutionCompleted, Receipt: receipt}
	return nil
}

// RecordPlatformPosted appends and fsyncs the acknowledgement that the bridge
// accepted the exact execution receipt.
func (l *PromptExperimentReceiptLedger) RecordPlatformPosted(receipt PromptExperimentReceipt) error {
	if receipt.Disposition == "" {
		receipt.Disposition = PromptExperimentDispositionCompleted
	}
	if err := validatePromptExperimentReceipt(receipt); err != nil {
		return err
	}
	if receipt.Disposition != PromptExperimentDispositionCompleted {
		return fmt.Errorf("platform-posted receipt %q has disposition %q", receipt.ReceiptID, receipt.Disposition)
	}
	if strings.TrimSpace(receipt.PostedRunID) == "" {
		return fmt.Errorf("platform_posted receipt %q requires a non-empty posted run id", receipt.ReceiptID)
	}
	if receipt.CostCompleteness != PromptExperimentCostComplete {
		return fmt.Errorf("platform_posted receipt %q requires complete provider cost", receipt.ReceiptID)
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.closed {
		return fmt.Errorf("prompt receipt ledger is closed")
	}
	state, exists := l.states[receipt.ReceiptID]
	if !exists || state.Status != PromptExperimentReceiptExecutionCompleted {
		return fmt.Errorf("receipt %q is not execution_completed", receipt.ReceiptID)
	}
	if !samePromptExecutionEvidence(state.Receipt, receipt) {
		return fmt.Errorf("platform-posted receipt %q does not match execution evidence", receipt.ReceiptID)
	}
	if err := l.appendAndSync(promptExperimentReceiptLine{Event: promptReceiptEventPlatformPosted, PromptExperimentReceipt: receipt}); err != nil {
		return err
	}
	l.states[receipt.ReceiptID] = PromptExperimentReceiptState{Status: PromptExperimentReceiptPlatformPosted, Receipt: receipt}
	return nil
}

func samePromptExecutionEvidence(a, b PromptExperimentReceipt) bool {
	a.PostedRunID = ""
	b.PostedRunID = ""
	return reflect.DeepEqual(a, b)
}

func (l *PromptExperimentReceiptLedger) appendAndSync(line promptExperimentReceiptLine) error {
	body, err := json.Marshal(line)
	if err != nil {
		return fmt.Errorf("marshal prompt receipt: %w", err)
	}
	body = append(body, '\n')
	if _, err := l.file.Write(body); err != nil {
		return fmt.Errorf("append prompt receipt ledger %q: %w", l.path, err)
	}
	if err := l.file.Sync(); err != nil {
		return fmt.Errorf("fsync prompt receipt ledger %q: %w", l.path, err)
	}
	return nil
}

// Close closes the ledger. Every successful Record call has already fsynced.
func (l *PromptExperimentReceiptLedger) Close() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	var closeErr error
	if !l.closed {
		l.closed = true
		if err := l.file.Close(); err != nil {
			closeErr = fmt.Errorf("close prompt receipt ledger %q: %w", l.path, err)
		}
	}
	var lockErr, rootErr error
	if l.root != nil {
		lockErr = releasePromptReceiptLock(l.root, l.lockName, l.path+".lock")
		rootErr = l.root.Close()
		l.root = nil
		if lockErr == nil {
			l.lockName = ""
		}
	}
	return errors.Join(closeErr, lockErr, rootErr)
}

func validatePromptExperimentReceipt(receipt PromptExperimentReceipt) error {
	if strings.TrimSpace(receipt.ExperimentID) == "" || strings.TrimSpace(receipt.CaseID) == "" || strings.TrimSpace(string(receipt.Arm)) == "" ||
		strings.TrimSpace(receipt.SubjectRef) == "" || strings.TrimSpace(receipt.VariantRef) == "" || strings.TrimSpace(receipt.ReceiptID) == "" ||
		!isSHA256Digest(receipt.InvocationScopeDigest) {
		return fmt.Errorf("prompt receipt identity fields and invocation scope digest are required")
	}
	if receipt.TrialIndex <= 0 {
		return fmt.Errorf("prompt receipt trial index must be positive")
	}
	if receipt.CostUSD < 0 || receipt.TurnCount < 0 || receipt.TokenCounts.Input < 0 || receipt.TokenCounts.Output < 0 || receipt.TokenCounts.CacheRead < 0 {
		return fmt.Errorf("prompt receipt usage fields must be non-negative")
	}
	switch receipt.CostCompleteness {
	case PromptExperimentCostComplete, PromptExperimentCostPartial:
	case PromptExperimentCostMissing:
		if receipt.CostUSD != 0 {
			return fmt.Errorf("prompt receipt with missing cost must record costUsd zero")
		}
	default:
		return fmt.Errorf("prompt receipt cost completeness %q is invalid", receipt.CostCompleteness)
	}
	switch receipt.Disposition {
	case PromptExperimentDispositionCompleted, PromptExperimentDispositionExecutionError:
	default:
		return fmt.Errorf("prompt receipt disposition %q is invalid", receipt.Disposition)
	}
	return nil
}

func isSHA256Digest(value string) bool {
	const prefix = "sha256:"
	if !strings.HasPrefix(value, prefix) || len(value) != len(prefix)+sha256.Size*2 {
		return false
	}
	_, err := hex.DecodeString(strings.TrimPrefix(value, prefix))
	return err == nil
}

func promptExperimentReceiptIdentity(cfg Config, c Case, trial experiment.Trial, graderIDs []string) (PromptExperimentReceiptIdentity, error) {
	graders := append([]string(nil), graderIDs...)
	sort.Strings(graders)
	destination := ""
	if cfg.Bridge != nil {
		destination = strings.TrimRight(cfg.Bridge.BaseURL, "/") + cfg.Bridge.Path
	}
	scopeBody, err := json.Marshal(struct {
		OrgID       string   `json:"orgId"`
		ProjectID   string   `json:"projectId"`
		DatasetID   string   `json:"datasetId"`
		Destination string   `json:"destination"`
		GraderIDs   []string `json:"graderIds"`
		Repo        string   `json:"repo"`
		Ref         string   `json:"ref"`
		MaxTurns    int      `json:"maxTurns"`
		MaxTokens   int64    `json:"maxTokens"`
		Case        Case     `json:"case"`
		Prompt      struct {
			UserPrompt    string                   `json:"userPrompt"`
			SystemPrompt  string                   `json:"systemPrompt"`
			ContextReset  *experiment.ContextReset `json:"contextReset,omitempty"`
			Perturbations []string                 `json:"perturbations,omitempty"`
		} `json:"prompt"`
	}{
		OrgID: cfg.OrgID, ProjectID: cfg.ProjectID, DatasetID: cfg.DatasetID,
		Destination: destination, GraderIDs: graders, Repo: c.Input.Repo, Ref: c.Input.Ref,
		MaxTurns: cfg.Budget.MaxTurns, MaxTokens: cfg.Budget.MaxTokens, Case: c,
		Prompt: struct {
			UserPrompt    string                   `json:"userPrompt"`
			SystemPrompt  string                   `json:"systemPrompt"`
			ContextReset  *experiment.ContextReset `json:"contextReset,omitempty"`
			Perturbations []string                 `json:"perturbations,omitempty"`
		}{
			UserPrompt: trial.Prompt.UserPrompt, SystemPrompt: trial.Prompt.SystemPrompt,
			ContextReset: trial.Prompt.ContextReset, Perturbations: trial.Prompt.Perturbations,
		},
	})
	if err != nil {
		return PromptExperimentReceiptIdentity{}, fmt.Errorf("hash prompt invocation scope: %w", err)
	}
	scopeSum := sha256.Sum256(scopeBody)
	identity := PromptExperimentReceiptIdentity{
		ExperimentID: trial.ExperimentID, CaseID: trial.CaseID, Arm: Arm(trial.Arm.ID),
		SubjectRef: trial.Arm.SubjectRef, VariantRef: trial.Arm.VariantRef, TrialIndex: trial.TrialIndex,
		InvocationScopeDigest: "sha256:" + hex.EncodeToString(scopeSum[:]),
	}
	body, err := json.Marshal(struct {
		ExperimentID          string `json:"experimentId"`
		CaseID                string `json:"caseId"`
		Arm                   Arm    `json:"arm"`
		SubjectRef            string `json:"subjectRef"`
		VariantRef            string `json:"variantRef"`
		TrialIndex            int    `json:"trialIndex"`
		InvocationScopeDigest string `json:"invocationScopeDigest"`
	}{
		identity.ExperimentID, identity.CaseID, identity.Arm, identity.SubjectRef,
		identity.VariantRef, identity.TrialIndex, identity.InvocationScopeDigest,
	})
	if err != nil {
		return PromptExperimentReceiptIdentity{}, fmt.Errorf("hash prompt receipt identity: %w", err)
	}
	sum := sha256.Sum256(body)
	identity.ReceiptID = "receipt-" + hex.EncodeToString(sum[:])
	return identity, nil
}
