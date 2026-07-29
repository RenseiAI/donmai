package codeintel

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/RenseiAI/donmai/eval/experiment"
)

func promptExperimentDriverFixture(t *testing.T) (Case, experiment.Definition, map[string]string, string) {
	t.Helper()
	repoDir, sha := initTempRepo(t)
	donmaiDir := writeFakeBinary(t, "donmai")
	const rawUserPrompt = "private raw user prompt must not enter the receipt"
	c := Case{
		ID:             "case-1",
		Input:          CaseInput{TaskType: TaskFindSymbol, Repo: "test/repo", Ref: sha, Prompt: rawUserPrompt},
		ExpectedOutput: json.RawMessage(`{"file":"foo.go","lineRange":[1,10]}`),
		Tags:           []string{tagSuite, "find-symbol", tagVersion},
	}
	definition := experiment.Definition{
		ID: "prompt-v1",
		Arms: []experiment.Arm{
			{ID: "incumbent", SubjectRef: "agent/base", VariantRef: experiment.SHA256VariantRef("private incumbent prompt"), SystemPrompt: "private incumbent prompt"},
			{ID: "candidate", SubjectRef: "agent/base", VariantRef: experiment.SHA256VariantRef("private candidate prompt"), SystemPrompt: "private candidate prompt"},
		},
	}
	return c, definition, map[string]string{"test/repo": repoDir}, filepath.Join(donmaiDir, "donmai")
}

func successfulPromptBridge(t *testing.T, requests *atomic.Int32, beforeResponse func()) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			_, _ = w.Write([]byte(`{"experiment":{"cases":[]}}`))
			return
		}
		if r.Method != http.MethodPost {
			t.Errorf("unexpected bridge method %s", r.Method)
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		if requests != nil {
			requests.Add(1)
		}
		if beforeResponse != nil {
			beforeResponse()
		}
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"runId":"evr_prompt","traceId":"evt_prompt","datasetId":"evd_prompt","gradersRun":["safety/injection-follow-v1"],"gradeResults":[{"graderId":"safety/injection-follow-v1","status":"scored","score":1,"pass":true}]}`))
	}))
}

func readReceiptLines(t *testing.T, path string) []map[string]any {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open receipt ledger: %v", err)
	}
	defer func() { _ = f.Close() }()
	var lines []map[string]any
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		var line map[string]any
		if err := json.Unmarshal(sc.Bytes(), &line); err != nil {
			t.Fatalf("decode receipt line %q: %v", sc.Text(), err)
		}
		lines = append(lines, line)
	}
	if err := sc.Err(); err != nil {
		t.Fatalf("scan receipt ledger: %v", err)
	}
	return lines
}

type contextResetRecordingExecutor struct {
	executor ClaudeExecutor
	specs    []ArmSpec
}

func (e *contextResetRecordingExecutor) Name() string { return e.executor.Name() }

func (*contextResetRecordingExecutor) SupportsPromptExperiments() bool { return true }

func (e *contextResetRecordingExecutor) Execute(ctx context.Context, spec ArmSpec) (Transcript, error) {
	e.specs = append(e.specs, spec)
	return e.executor.Execute(ctx, spec)
}

func TestPromptExperimentReceiptLedgerSanitizedAppendAndReload(t *testing.T) {
	path := filepath.Join(t.TempDir(), "receipts.jsonl")
	ledger, err := OpenPromptExperimentReceiptLedger(path)
	if err != nil {
		t.Fatalf("open ledger: %v", err)
	}
	receipt := PromptExperimentReceipt{
		ExperimentID: "prompt-v1", CaseID: "case-1", Arm: "candidate",
		SubjectRef: "agent/base", VariantRef: experiment.SHA256VariantRef("private prompt"),
		TrialIndex: 1, ReceiptID: "receipt-local-1", InvocationScopeDigest: "sha256:" + strings.Repeat("a", 64),
		CostUSD: 0, CostCompleteness: PromptExperimentCostComplete,
		TurnCount: 2, TokenCounts: TokenCounts{Input: 10, Output: 3, CacheRead: 4},
	}
	if err := ledger.RecordExecutionCompleted(receipt); err != nil {
		t.Fatalf("record execution: %v", err)
	}
	postedReceipt := receipt
	postedReceipt.PostedRunID = "evr_prompt"
	if err := ledger.RecordPlatformPosted(postedReceipt); err != nil {
		t.Fatalf("record platform post: %v", err)
	}
	if err := ledger.Close(); err != nil {
		t.Fatalf("close ledger: %v", err)
	}

	lines := readReceiptLines(t, path)
	if len(lines) != 2 {
		t.Fatalf("receipt lines = %d, want 2", len(lines))
	}
	if lines[0]["event"] != "execution_completed" || lines[1]["event"] != "platform_posted" {
		t.Fatalf("receipt event order = %#v", lines)
	}
	if got, ok := lines[0]["costUsd"]; !ok || got != float64(0) {
		t.Fatalf("explicit zero cost = %v present=%v", got, ok)
	}
	if _, ok := lines[0]["postedRunId"]; ok {
		t.Fatalf("execution_completed must not carry postedRunId: %#v", lines[0])
	}
	if got := lines[1]["postedRunId"]; got != "evr_prompt" {
		t.Fatalf("platform_posted postedRunId = %v, want evr_prompt", got)
	}
	allowed := map[string]bool{
		"event": true, "experimentId": true, "caseId": true, "arm": true,
		"subjectRef": true, "variantRef": true, "trialIndex": true, "receiptId": true,
		"disposition": true, "invocationScopeDigest": true, "costUsd": true,
		"costCompleteness": true, "turnCount": true, "tokenCounts": true, "postedRunId": true,
	}
	for _, line := range lines {
		for key := range line {
			if !allowed[key] {
				t.Fatalf("receipt contains non-safe field %q: %#v", key, line)
			}
		}
	}
	encoded, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), "private prompt") {
		t.Fatalf("posted receipt leaked raw prompt: %s", encoded)
	}

	reloaded, err := OpenPromptExperimentReceiptLedger(path)
	if err != nil {
		t.Fatalf("reload ledger: %v", err)
	}
	defer func() { _ = reloaded.Close() }()
	state, err := reloaded.Lookup(receipt.Identity())
	if err != nil {
		t.Fatalf("lookup: %v", err)
	}
	if state.Status != PromptExperimentReceiptPlatformPosted || state.Receipt.CostUSD != 0 || state.Receipt.PostedRunID != "evr_prompt" {
		t.Fatalf("reloaded state = %+v", state)
	}
}

func TestPromptExperimentReceiptIdentityBindsSafeInvocationScope(t *testing.T) {
	c, definition, _, _ := promptExperimentDriverFixture(t)
	trial := experiment.Trial{
		ExperimentID: definition.ID, CaseID: c.ID, Arm: definition.Arms[0], TrialIndex: 1,
		Prompt: experiment.PromptPlan{
			UserPrompt: c.Input.Prompt, SystemPrompt: definition.Arms[0].SystemPrompt,
			ContextReset: &experiment.ContextReset{AfterTurn: 4, ContinuationPrompt: "private recovery prompt"},
		},
	}
	base := Config{
		OrgID: "org_scope", ProjectID: "proj_scope", DatasetID: "evd_scope",
		Bridge: NewBridge("https://destination.invalid", "secret-token", "/api/evals/ingest"),
		Budget: Budget{MaxTurns: 8, MaxTokens: 1000}, RepoRoots: map[string]string{c.Input.Repo: "/private/local/path"},
	}
	graders := []string{"safety/injection-follow-v1", "quality/task-v1"}
	identity, err := promptExperimentReceiptIdentity(base, c, trial, graders)
	if err != nil {
		t.Fatal(err)
	}
	if identity.InvocationScopeDigest == "" {
		t.Fatal("invocation scope digest is empty")
	}

	cloneConfig := func() Config {
		cfg := base
		bridge := *base.Bridge
		cfg.Bridge = &bridge
		return cfg
	}
	tests := []struct {
		name   string
		mutate func(*Config, *Case, *experiment.Trial, *[]string)
	}{
		{name: "org", mutate: func(cfg *Config, _ *Case, _ *experiment.Trial, _ *[]string) { cfg.OrgID = "org_other" }},
		{name: "project", mutate: func(cfg *Config, _ *Case, _ *experiment.Trial, _ *[]string) { cfg.ProjectID = "proj_other" }},
		{name: "dataset", mutate: func(cfg *Config, _ *Case, _ *experiment.Trial, _ *[]string) { cfg.DatasetID = "evd_other" }},
		{name: "destination", mutate: func(cfg *Config, _ *Case, _ *experiment.Trial, _ *[]string) {
			cfg.Bridge.BaseURL = "https://other.invalid"
		}},
		{name: "max-turns", mutate: func(cfg *Config, _ *Case, _ *experiment.Trial, _ *[]string) { cfg.Budget.MaxTurns++ }},
		{name: "max-tokens", mutate: func(cfg *Config, _ *Case, _ *experiment.Trial, _ *[]string) { cfg.Budget.MaxTokens++ }},
		{name: "graders", mutate: func(_ *Config, _ *Case, _ *experiment.Trial, ids *[]string) { *ids = []string{"quality/other-v1"} }},
		{name: "repo", mutate: func(_ *Config, c *Case, _ *experiment.Trial, _ *[]string) { c.Input.Repo = "other/repo" }},
		{name: "ref", mutate: func(_ *Config, c *Case, _ *experiment.Trial, _ *[]string) { c.Input.Ref = "cafebabe" }},
		{name: "case-content", mutate: func(_ *Config, c *Case, _ *experiment.Trial, _ *[]string) {
			c.Input.Prompt = "different private prompt"
		}},
		{name: "planned-system-prompt", mutate: func(_ *Config, _ *Case, tr *experiment.Trial, _ *[]string) {
			tr.Prompt.SystemPrompt = "different private system prompt"
		}},
		{name: "continuation-prompt", mutate: func(_ *Config, _ *Case, tr *experiment.Trial, _ *[]string) {
			tr.Prompt.ContextReset.ContinuationPrompt = "different private recovery"
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg, changedCase, changedTrial, changedGraders := cloneConfig(), c, trial, append([]string(nil), graders...)
			reset := *trial.Prompt.ContextReset
			changedTrial.Prompt.ContextReset = &reset
			tt.mutate(&cfg, &changedCase, &changedTrial, &changedGraders)
			changed, err := promptExperimentReceiptIdentity(cfg, changedCase, changedTrial, changedGraders)
			if err != nil {
				t.Fatal(err)
			}
			if changed.InvocationScopeDigest == identity.InvocationScopeDigest || changed.ReceiptID == identity.ReceiptID {
				t.Fatalf("scope mutation did not change identity: base=%+v changed=%+v", identity, changed)
			}
		})
	}

	reordered, err := promptExperimentReceiptIdentity(base, c, trial, []string{graders[1], graders[0]})
	if err != nil {
		t.Fatal(err)
	}
	if reordered != identity {
		t.Fatalf("grader set order changed identity: base=%+v reordered=%+v", identity, reordered)
	}
	tokenOnly := cloneConfig()
	tokenOnly.Bridge.Token = "different-secret-token"
	withoutToken, err := promptExperimentReceiptIdentity(tokenOnly, c, trial, graders)
	if err != nil {
		t.Fatal(err)
	}
	if withoutToken != identity {
		t.Fatalf("credential changed receipt identity: base=%+v changed=%+v", identity, withoutToken)
	}

	trialTwo := trial
	trialTwo.TrialIndex = 2
	continued, err := promptExperimentReceiptIdentity(base, c, trialTwo, graders)
	if err != nil {
		t.Fatal(err)
	}
	if continued.InvocationScopeDigest != identity.InvocationScopeDigest {
		t.Fatalf("trial index changed invocation scope: base=%+v continued=%+v", identity, continued)
	}
	if continued.ReceiptID == identity.ReceiptID || continued.TrialIndex != 2 {
		t.Fatalf("trial 2 did not receive a distinct deterministic receipt: base=%+v continued=%+v", identity, continued)
	}
}

func TestPromptExperimentReceiptLedgerExcludesConcurrentProcessAndReleasesLock(t *testing.T) {
	path := filepath.Join(t.TempDir(), "receipts.jsonl")
	first, err := OpenPromptExperimentReceiptLedger(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := OpenPromptExperimentReceiptLedger(path); err == nil || !strings.Contains(err.Error(), "locked") {
		t.Fatalf("concurrent open error = %v, want fail-closed lock", err)
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	second, err := OpenPromptExperimentReceiptLedger(path)
	if err != nil {
		t.Fatalf("reopen after Close: %v", err)
	}
	if err := second.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestPromptExperimentReceiptLedgerRequiresPostedRunIDAcknowledgement(t *testing.T) {
	path := filepath.Join(t.TempDir(), "receipts.jsonl")
	ledger, err := OpenPromptExperimentReceiptLedger(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ledger.Close() }()
	receipt := PromptExperimentReceipt{
		ExperimentID: "prompt-v1", CaseID: "case-1", Arm: "candidate",
		SubjectRef: "agent/base", VariantRef: experiment.SHA256VariantRef("private prompt"),
		TrialIndex: 1, ReceiptID: "receipt-local-1", InvocationScopeDigest: "sha256:" + strings.Repeat("a", 64),
		CostUSD: 0.1, CostCompleteness: PromptExperimentCostComplete,
	}
	if err := ledger.RecordExecutionCompleted(receipt); err != nil {
		t.Fatal(err)
	}
	if err := ledger.RecordPlatformPosted(receipt); err == nil || !strings.Contains(err.Error(), "posted run id") {
		t.Fatalf("platform_posted without run id error = %v", err)
	}
	tampered := receipt
	tampered.PostedRunID = "evr_prompt"
	tampered.CostUSD = 0.2
	if err := ledger.RecordPlatformPosted(tampered); err == nil || !strings.Contains(err.Error(), "does not match execution evidence") {
		t.Fatalf("platform_posted with changed execution evidence error = %v", err)
	}

	invalidExecution := receipt
	invalidExecution.ReceiptID = "receipt-local-2"
	invalidExecution.TrialIndex = 2
	invalidExecution.PostedRunID = "evr_must_not_be_on_execution"
	if err := ledger.RecordExecutionCompleted(invalidExecution); err == nil || !strings.Contains(err.Error(), "posted run id") {
		t.Fatalf("execution_completed with run id error = %v", err)
	}
}

func TestDriverPromptExperimentReceiptDurableBeforeBridgeFailure(t *testing.T) {
	c, definition, roots, donmaiBin := promptExperimentDriverFixture(t)
	path := filepath.Join(t.TempDir(), "receipts.jsonl")
	ledger, err := OpenPromptExperimentReceiptLedger(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ledger.Close() }()

	var sawDurableBeforePost atomic.Bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			_, _ = w.Write([]byte(`{"experiment":{"cases":[]}}`))
			return
		}
		lines := readReceiptLines(t, path)
		if len(lines) == 1 && lines[0]["event"] == "execution_completed" {
			sawDurableBeforePost.Store(true)
		}
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte("bridge failed"))
	}))
	defer server.Close()

	exec := &promptRecordingExecutor{costs: []float64{0.125}}
	exec.onExecute = func(call int) error {
		if call != 1 {
			return nil
		}
		if info, statErr := os.Stat(path); statErr != nil || info.Size() != 0 {
			return fmt.Errorf("receipt existed before provider completion: stat=%v size=%d", statErr, func() int64 {
				if info == nil {
					return -1
				}
				return info.Size()
			}())
		}
		return nil
	}
	d, err := NewDriver(Config{
		Trials: 1, DonmaiBin: donmaiBin, RepoRoots: roots, WorkareaParent: t.TempDir(),
		Executor: exec, Bridge: NewBridge(server.URL, "secret-platform-token", ""), PromptReceiptJournal: ledger,
		DatasetID: "evd_prompt", ProjectID: "proj_test",
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = d.RunPromptExperiment(context.Background(), []Case{c}, definition, []string{"safety/injection-follow-v1"})
	if err == nil || !strings.Contains(err.Error(), "bridge post") {
		t.Fatalf("error = %v, want bridge failure", err)
	}
	if !sawDurableBeforePost.Load() {
		t.Fatal("bridge ran before execution_completed receipt was durable")
	}
	lines := readReceiptLines(t, path)
	if len(lines) != 1 || lines[0]["event"] != "execution_completed" || lines[0]["costUsd"] != 0.125 {
		t.Fatalf("durable receipt after bridge failure = %#v", lines)
	}
	encoded, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{
		c.Input.Prompt, c.Input.Repo, c.Input.Ref, definition.Arms[0].SystemPrompt,
		roots["test/repo"], server.URL, "secret-platform-token", "foo.go:3",
	} {
		if strings.Contains(string(encoded), forbidden) {
			t.Fatalf("receipt leaked forbidden value %q: %s", forbidden, encoded)
		}
	}
}

func TestDriverPromptExperimentCostBearingExecutionErrorIsDurableAndBlocksRetry(t *testing.T) {
	c, definition, roots, donmaiBin := promptExperimentDriverFixture(t)
	path := filepath.Join(t.TempDir(), "receipts.jsonl")
	ledger, err := OpenPromptExperimentReceiptLedger(path)
	if err != nil {
		t.Fatal(err)
	}
	var requests atomic.Int32
	server := successfulPromptBridge(t, &requests, nil)
	defer server.Close()

	first := &promptRecordingExecutor{failAt: 1, failWithCost: true}
	d, err := NewDriver(Config{
		Trials: 1, DonmaiBin: donmaiBin, RepoRoots: roots, WorkareaParent: t.TempDir(), Executor: first,
		Bridge: NewBridge(server.URL, "", ""), PromptReceiptJournal: ledger, DatasetID: "evd_prompt",
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = d.RunPromptExperiment(context.Background(), []Case{c}, definition, []string{"safety/injection-follow-v1"})
	if err == nil || !strings.Contains(err.Error(), "scripted provider failure") {
		t.Fatalf("error = %v, want provider failure", err)
	}
	if requests.Load() != 0 {
		t.Fatalf("bridge requests = %d, want 0", requests.Load())
	}
	if err := ledger.Close(); err != nil {
		t.Fatal(err)
	}
	lines := readReceiptLines(t, path)
	if len(lines) != 1 || lines[0]["event"] != "execution_completed" ||
		lines[0]["disposition"] != PromptExperimentDispositionExecutionError || lines[0]["costUsd"] != 0.125 {
		t.Fatalf("cost-bearing execution error receipt = %#v", lines)
	}
	encoded, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{"scripted provider failure", "private stderr", "private partial output"} {
		if strings.Contains(string(encoded), forbidden) {
			t.Fatalf("execution-error receipt leaked %q: %s", forbidden, encoded)
		}
	}

	reloaded, err := OpenPromptExperimentReceiptLedger(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = reloaded.Close() }()
	second := &promptRecordingExecutor{}
	d, err = NewDriver(Config{
		Trials: 1, DonmaiBin: donmaiBin, RepoRoots: roots, WorkareaParent: t.TempDir(), Executor: second,
		Bridge: NewBridge(server.URL, "", ""), PromptReceiptJournal: reloaded, DatasetID: "evd_prompt",
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = d.RunPromptExperiment(context.Background(), []Case{c}, definition, []string{"safety/injection-follow-v1"})
	if err == nil || !strings.Contains(err.Error(), "execution_completed") || !strings.Contains(err.Error(), "not platform_posted") {
		t.Fatalf("retry error = %v, want unposted preflight block", err)
	}
	if len(second.specs) != 0 {
		t.Fatalf("retry executed provider %d time(s)", len(second.specs))
	}
}

func TestDriverPromptExperimentIncompleteCostIsDurableAndBlocksRetry(t *testing.T) {
	for _, tt := range []struct {
		name             string
		exec             *promptRecordingExecutor
		wantCompleteness PromptExperimentCostCompleteness
		wantCost         float64
	}{
		{name: "partial", exec: &promptRecordingExecutor{costs: []float64{0.075}, incompleteCost: true}, wantCompleteness: PromptExperimentCostPartial, wantCost: 0.075},
		{name: "missing", exec: &promptRecordingExecutor{omitCost: true}, wantCompleteness: PromptExperimentCostMissing, wantCost: 0},
	} {
		t.Run(tt.name, func(t *testing.T) {
			c, definition, roots, donmaiBin := promptExperimentDriverFixture(t)
			path := filepath.Join(t.TempDir(), "receipts.jsonl")
			ledger, err := OpenPromptExperimentReceiptLedger(path)
			if err != nil {
				t.Fatal(err)
			}
			var requests atomic.Int32
			server := successfulPromptBridge(t, &requests, nil)
			defer server.Close()
			d, err := NewDriver(Config{
				Trials: 1, DonmaiBin: donmaiBin, RepoRoots: roots, WorkareaParent: t.TempDir(), Executor: tt.exec,
				Bridge: NewBridge(server.URL, "", ""), PromptReceiptJournal: ledger,
				OrgID: "org_scope", ProjectID: "proj_scope", DatasetID: "evd_prompt",
			})
			if err != nil {
				t.Fatal(err)
			}
			_, err = d.RunPromptExperiment(context.Background(), []Case{c}, definition, []string{"safety/injection-follow-v1"})
			if err == nil || !strings.Contains(err.Error(), "complete provider cost") {
				t.Fatalf("error = %v, want incomplete-cost failure", err)
			}
			if requests.Load() != 0 {
				t.Fatalf("bridge requests = %d, want 0", requests.Load())
			}
			if err := ledger.Close(); err != nil {
				t.Fatal(err)
			}
			lines := readReceiptLines(t, path)
			if len(lines) != 1 || lines[0]["event"] != "execution_completed" ||
				lines[0]["costCompleteness"] != string(tt.wantCompleteness) || lines[0]["costUsd"] != tt.wantCost {
				t.Fatalf("incomplete execution receipt = %#v", lines)
			}

			reloaded, err := OpenPromptExperimentReceiptLedger(path)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = reloaded.Close() }()
			retry := &promptRecordingExecutor{}
			d, err = NewDriver(Config{
				Trials: 1, DonmaiBin: donmaiBin, RepoRoots: roots, WorkareaParent: t.TempDir(), Executor: retry,
				Bridge: NewBridge(server.URL, "", ""), PromptReceiptJournal: reloaded,
				OrgID: "org_scope", ProjectID: "proj_scope", DatasetID: "evd_prompt",
			})
			if err != nil {
				t.Fatal(err)
			}
			_, err = d.RunPromptExperiment(context.Background(), []Case{c}, definition, []string{"safety/injection-follow-v1"})
			if err == nil || !strings.Contains(err.Error(), "execution_completed") {
				t.Fatalf("retry error = %v, want durable preflight block", err)
			}
			if len(retry.specs) != 0 {
				t.Fatalf("retry executed provider %d time(s)", len(retry.specs))
			}
		})
	}
}

func TestDriverPromptExperimentPostsBothArmsAfterGradeableResetTokenExhaustion(t *testing.T) {
	c, definition, roots, donmaiBin := promptExperimentDriverFixture(t)
	definition.Perturbations = []experiment.Perturbation{
		experiment.ContextResetAtTurn(4, "recover from durable state and finish"),
	}
	phaseOneIncumbent := strings.Join([]string{
		`{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"checkpoint"}],"usage":{"input_tokens":50,"output_tokens":10,"cache_read_input_tokens":20}}}`,
		`{"type":"result","subtype":"error_max_turns","is_error":true,"result":"","total_cost_usd":0.1}`,
	}, "\n")
	phaseTwoIncumbent := strings.Join([]string{
		`{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"foo.go:3"}],"usage":{"input_tokens":15,"output_tokens":4,"cache_read_input_tokens":2}}}`,
		`{"type":"result","subtype":"success","is_error":false,"result":"foo.go:3","total_cost_usd":0.1,"usage":{"input_tokens":15,"output_tokens":4,"cache_read_input_tokens":2}}`,
	}, "\n")
	phaseOneCandidate := strings.Join([]string{
		`{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"checkpoint"}],"usage":{"input_tokens":10,"output_tokens":2,"cache_read_input_tokens":3}}}`,
		`{"type":"result","subtype":"error_max_turns","is_error":true,"result":"","total_cost_usd":0.05}`,
	}, "\n")
	phaseTwoCandidate := strings.Join([]string{
		`{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"foo.go:3"}],"usage":{"input_tokens":10,"output_tokens":2,"cache_read_input_tokens":3}}}`,
		`{"type":"result","subtype":"success","is_error":false,"result":"foo.go:3","total_cost_usd":0.05,"usage":{"input_tokens":10,"output_tokens":2,"cache_read_input_tokens":3}}`,
	}, "\n")
	spawner := &queuedSpawn{steps: []spawnStep{
		{stream: phaseOneIncumbent},
		{stream: phaseTwoIncumbent},
		{stream: phaseOneCandidate},
		{stream: phaseTwoCandidate},
	}}
	exec := &contextResetRecordingExecutor{executor: newClaudeExecutorWithSpawner(spawner.spawn)}
	path := filepath.Join(t.TempDir(), "receipts.jsonl")
	ledger, err := OpenPromptExperimentReceiptLedger(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ledger.Close() }()
	var postedOutputs []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			_, _ = w.Write([]byte(`{"experiment":{"cases":[]}}`))
			return
		}
		var ingest IngestRequest
		if err := json.NewDecoder(r.Body).Decode(&ingest); err != nil {
			t.Errorf("decode ingest request: %v", err)
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		output, ok := ingest.OutputPayload.(string)
		if !ok {
			t.Errorf("output payload type = %T, want string", ingest.OutputPayload)
		}
		postedOutputs = append(postedOutputs, output)
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"runId":"evr_prompt","traceId":"evt_prompt","datasetId":"evd_prompt","gradersRun":["safety/injection-follow-v1"],"gradeResults":[{"graderId":"safety/injection-follow-v1","status":"scored","score":1,"pass":true}]}`))
	}))
	defer server.Close()
	d, err := NewDriver(Config{
		Trials: 1, DonmaiBin: donmaiBin, RepoRoots: roots, WorkareaParent: t.TempDir(),
		Executor: exec, Bridge: NewBridge(server.URL, "", ""), PromptReceiptJournal: ledger,
		DatasetID: "evd_prompt", ProjectID: "proj_test", Budget: Budget{MaxTurns: 8, MaxTokens: 100},
	})
	if err != nil {
		t.Fatal(err)
	}
	report, err := d.RunPromptExperiment(context.Background(), []Case{c}, definition, []string{"safety/injection-follow-v1"})
	if err != nil {
		t.Fatalf("prompt experiment: %v", err)
	}
	if len(report.Outcomes) != 2 || len(exec.specs) != 2 || spawner.calls != 4 {
		t.Fatalf("outcomes=%d executions=%d phase spawns=%d, want 2/2/4", len(report.Outcomes), len(exec.specs), spawner.calls)
	}
	if report.Outcomes[0].Result.Pass {
		t.Fatal("over-budget incumbent must be graded false after its terminal answer is cleared")
	}
	if !report.Outcomes[1].Result.Pass {
		t.Fatal("candidate should still execute and pass")
	}
	if len(postedOutputs) != 2 {
		t.Fatalf("bridge requests = %d, want 2", len(postedOutputs))
	}
	if postedOutputs[0] != "" {
		t.Fatalf("over-budget incumbent output posted as %q, want empty", postedOutputs[0])
	}
	if postedOutputs[1] != "foo.go:3" {
		t.Fatalf("candidate output posted as %q, want foo.go:3", postedOutputs[1])
	}
	lines := readReceiptLines(t, path)
	if len(lines) != 4 {
		t.Fatalf("receipt lines = %d, want two completed+posted pairs: %#v", len(lines), lines)
	}
	for i, line := range lines {
		wantEvent := "execution_completed"
		if i%2 == 1 {
			wantEvent = "platform_posted"
		}
		if line["event"] != wantEvent || line["disposition"] != PromptExperimentDispositionCompleted || line["costCompleteness"] != string(PromptExperimentCostComplete) {
			t.Fatalf("receipt line %d = %#v, want %s completed with complete cost", i, line, wantEvent)
		}
	}
}

func TestDriverPromptExperimentLaterTrialFailureKeepsEarlierReceipts(t *testing.T) {
	c, definition, roots, donmaiBin := promptExperimentDriverFixture(t)
	path := filepath.Join(t.TempDir(), "receipts.jsonl")
	ledger, err := OpenPromptExperimentReceiptLedger(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ledger.Close() }()
	server := successfulPromptBridge(t, nil, nil)
	defer server.Close()

	exec := &promptRecordingExecutor{costs: []float64{0, 0.25}, failAt: 3}
	d, err := NewDriver(Config{
		Trials: 2, DonmaiBin: donmaiBin, RepoRoots: roots, WorkareaParent: t.TempDir(),
		Executor: exec, Bridge: NewBridge(server.URL, "", ""), PromptReceiptJournal: ledger,
		DatasetID: "evd_prompt", ProjectID: "proj_test",
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = d.RunPromptExperiment(context.Background(), []Case{c}, definition, []string{"safety/injection-follow-v1"})
	if err == nil || !strings.Contains(err.Error(), "scripted provider failure") {
		t.Fatalf("error = %v, want later provider failure", err)
	}
	lines := readReceiptLines(t, path)
	if len(lines) != 5 {
		t.Fatalf("receipt lines = %d, want two completed+posted pairs plus failed execution: %#v", len(lines), lines)
	}
	if lines[0]["costUsd"] != float64(0) || lines[2]["costUsd"] != 0.25 {
		t.Fatalf("explicit zero/positive costs not retained: %#v", lines)
	}
	if lines[4]["event"] != "execution_completed" || lines[4]["disposition"] != PromptExperimentDispositionExecutionError ||
		lines[4]["costCompleteness"] != string(PromptExperimentCostMissing) {
		t.Fatalf("failed execution uncertainty not retained: %#v", lines[4])
	}
}

func TestDriverPromptExperimentPostedIdentitySkipsProviderExecution(t *testing.T) {
	c, definition, roots, donmaiBin := promptExperimentDriverFixture(t)
	path := filepath.Join(t.TempDir(), "receipts.jsonl")
	var requests atomic.Int32
	server := successfulPromptBridge(t, &requests, nil)
	defer server.Close()

	run := func(exec *promptRecordingExecutor) (experiment.Report[RunRecord], error) {
		ledger, err := OpenPromptExperimentReceiptLedger(path)
		if err != nil {
			return experiment.Report[RunRecord]{}, err
		}
		defer func() { _ = ledger.Close() }()
		d, err := NewDriver(Config{
			Trials: 1, DonmaiBin: donmaiBin, RepoRoots: roots, WorkareaParent: t.TempDir(),
			Executor: exec, Bridge: NewBridge(server.URL, "", ""), PromptReceiptJournal: ledger,
			DatasetID: "evd_prompt", ProjectID: "proj_test",
		})
		if err != nil {
			return experiment.Report[RunRecord]{}, err
		}
		report, err := d.RunPromptExperiment(context.Background(), []Case{c}, definition, []string{"safety/injection-follow-v1"})
		if err == nil && len(report.Outcomes) != 2 {
			return report, fmt.Errorf("outcomes = %d, want 2", len(report.Outcomes))
		}
		return report, err
	}
	first := &promptRecordingExecutor{costs: []float64{0.1, 0.2}}
	firstReport, err := run(first)
	if err != nil {
		t.Fatalf("first run: %v", err)
	}
	second := &promptRecordingExecutor{}
	replayReport, err := run(second)
	if err != nil {
		t.Fatalf("replay run: %v", err)
	}
	for name, report := range map[string]experiment.Report[RunRecord]{"first": firstReport, "replay": replayReport} {
		for _, outcome := range report.Outcomes {
			if outcome.Result.PostedRunID != "evr_prompt" {
				t.Fatalf("%s outcome %s/%s postedRunId = %q, want evr_prompt", name, outcome.Trial.CaseID, outcome.Trial.Arm.ID, outcome.Result.PostedRunID)
			}
		}
	}
	if len(second.specs) != 0 {
		t.Fatalf("posted replay re-executed provider %d time(s)", len(second.specs))
	}
	if requests.Load() != 2 {
		t.Fatalf("bridge requests = %d, want only first run's 2", requests.Load())
	}
}

func TestDriverPromptExperimentUnpostedIdentityBlocksBeforeExecution(t *testing.T) {
	c, definition, roots, donmaiBin := promptExperimentDriverFixture(t)
	path := filepath.Join(t.TempDir(), "receipts.jsonl")
	failedBridge := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			_, _ = w.Write([]byte(`{"experiment":{"cases":[]}}`))
			return
		}
		http.Error(w, "failed", http.StatusInternalServerError)
	}))
	ledger, err := OpenPromptExperimentReceiptLedger(path)
	if err != nil {
		t.Fatal(err)
	}
	first := &promptRecordingExecutor{costs: []float64{0.1}}
	d, err := NewDriver(Config{
		Trials: 1, DonmaiBin: donmaiBin, RepoRoots: roots, WorkareaParent: t.TempDir(), Executor: first,
		Bridge: NewBridge(failedBridge.URL, "", ""), PromptReceiptJournal: ledger, DatasetID: "evd_prompt",
	})
	if err != nil {
		t.Fatal(err)
	}
	_, _ = d.RunPromptExperiment(context.Background(), []Case{c}, definition, []string{"safety/injection-follow-v1"})
	_ = ledger.Close()
	failedBridge.Close()

	reloaded, err := OpenPromptExperimentReceiptLedger(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = reloaded.Close() }()
	second := &promptRecordingExecutor{}
	d, err = NewDriver(Config{
		Trials: 1, DonmaiBin: donmaiBin, RepoRoots: roots, WorkareaParent: t.TempDir(), Executor: second,
		Bridge: NewBridge(failedBridge.URL, "", ""), PromptReceiptJournal: reloaded, DatasetID: "evd_prompt",
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = d.RunPromptExperiment(context.Background(), []Case{c}, definition, []string{"safety/injection-follow-v1"})
	if err == nil || !strings.Contains(err.Error(), "execution_completed") || !strings.Contains(err.Error(), "not platform_posted") {
		t.Fatalf("error = %v, want unposted preflight block", err)
	}
	if len(second.specs) != 0 {
		t.Fatalf("unposted retry executed provider %d time(s)", len(second.specs))
	}
}

func TestDriverPromptExperimentCanContinueAtDistinctTrialAfterUnpostedFailure(t *testing.T) {
	c, definition, roots, donmaiBin := promptExperimentDriverFixture(t)
	path := filepath.Join(t.TempDir(), "receipts.jsonl")
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			_, _ = w.Write([]byte(`{"experiment":{"cases":[]}}`))
			return
		}
		if requests.Add(1) == 1 {
			http.Error(w, "failed", http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"runId":"evr_prompt","traceId":"evt_prompt","datasetId":"evd_prompt","gradersRun":["safety/injection-follow-v1"],"gradeResults":[{"graderId":"safety/injection-follow-v1","status":"scored","score":1,"pass":true}]}`))
	}))
	defer server.Close()

	ledger, err := OpenPromptExperimentReceiptLedger(path)
	if err != nil {
		t.Fatal(err)
	}
	first := &promptRecordingExecutor{costs: []float64{0.1}}
	d, err := NewDriver(Config{
		Trials: 1, DonmaiBin: donmaiBin, RepoRoots: roots, WorkareaParent: t.TempDir(), Executor: first,
		Bridge: NewBridge(server.URL, "", ""), PromptReceiptJournal: ledger, DatasetID: "evd_prompt",
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = d.RunPromptExperiment(context.Background(), []Case{c}, definition, []string{"safety/injection-follow-v1"})
	if err == nil || !strings.Contains(err.Error(), "bridge post") {
		t.Fatalf("trial 1 error = %v, want bridge failure", err)
	}
	if err := ledger.Close(); err != nil {
		t.Fatal(err)
	}

	reloaded, err := OpenPromptExperimentReceiptLedger(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = reloaded.Close() }()
	continued := &promptRecordingExecutor{costs: []float64{0.2, 0.3}}
	d, err = NewDriver(Config{
		Trials: 1, TrialStart: 2, DonmaiBin: donmaiBin, RepoRoots: roots, WorkareaParent: t.TempDir(), Executor: continued,
		Bridge: NewBridge(server.URL, "", ""), PromptReceiptJournal: reloaded, DatasetID: "evd_prompt",
	})
	if err != nil {
		t.Fatal(err)
	}
	report, err := d.RunPromptExperiment(context.Background(), []Case{c}, definition, []string{"safety/injection-follow-v1"})
	if err != nil {
		t.Fatalf("trial 2 continuation: %v", err)
	}
	if len(report.Outcomes) != 2 || len(continued.specs) != 2 {
		t.Fatalf("trial 2 continuation outcomes=%d executions=%d, want 2/2", len(report.Outcomes), len(continued.specs))
	}
	for _, outcome := range report.Outcomes {
		if outcome.Trial.TrialIndex != 2 {
			t.Fatalf("continued trial index = %d, want 2", outcome.Trial.TrialIndex)
		}
	}
	lines := readReceiptLines(t, path)
	if len(lines) != 5 || lines[0]["trialIndex"] != float64(1) {
		t.Fatalf("continued receipt ledger = %#v, want blocked trial 1 plus two trial-2 posted pairs", lines)
	}
	for _, line := range lines[1:] {
		if line["trialIndex"] != float64(2) {
			t.Fatalf("continuation mutated or replayed trial identity: %#v", lines)
		}
	}
}

func TestDriverPromptExperimentReceiptWriteFailureStopsBeforeBridgeOrNextTrial(t *testing.T) {
	c, definition, roots, donmaiBin := promptExperimentDriverFixture(t)
	journal := &failingPromptReceiptJournal{}
	var requests atomic.Int32
	server := successfulPromptBridge(t, &requests, nil)
	defer server.Close()
	exec := &promptRecordingExecutor{}
	d, err := NewDriver(Config{
		Trials: 2, DonmaiBin: donmaiBin, RepoRoots: roots, WorkareaParent: t.TempDir(), Executor: exec,
		Bridge: NewBridge(server.URL, "", ""), PromptReceiptJournal: journal, DatasetID: "evd_prompt",
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = d.RunPromptExperiment(context.Background(), []Case{c}, definition, []string{"safety/injection-follow-v1"})
	if err == nil || !strings.Contains(err.Error(), "write execution_completed receipt") {
		t.Fatalf("error = %v, want receipt-write failure", err)
	}
	if len(exec.specs) != 1 || requests.Load() != 0 {
		t.Fatalf("after receipt failure executions=%d bridgeRequests=%d, want 1/0", len(exec.specs), requests.Load())
	}
}

type failingPromptReceiptJournal struct{}

func (*failingPromptReceiptJournal) Lookup(PromptExperimentReceiptIdentity) (PromptExperimentReceiptState, error) {
	return PromptExperimentReceiptState{}, nil
}

func (*failingPromptReceiptJournal) RecordExecutionCompleted(PromptExperimentReceipt) error {
	return fmt.Errorf("disk full")
}

func (*failingPromptReceiptJournal) RecordPlatformPosted(PromptExperimentReceipt) error { return nil }
