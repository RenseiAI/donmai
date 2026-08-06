package runner

import (
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/RenseiAI/donmai/agent"
	"github.com/RenseiAI/donmai/executioncell"
	"github.com/RenseiAI/donmai/internal/kit"
	"github.com/RenseiAI/donmai/prompt"
	"github.com/RenseiAI/donmai/runtime/workarea"
)

func fullOperationalFixture() QueuedWork {
	qw := exactReceiptQueuedWork("session_operational_fixture")
	qw.QueuedWork = prompt.QueuedWork{
		SessionID: "session_operational_fixture", IssueID: "issue-id", IssueIdentifier: "fixture-2034",
		LinearSessionID: "linear-session", ProviderSessionID: "provider-session", ProjectName: "Donmai",
		OrganizationID: "org-id", Repository: "RenseiAI/donmai", Ref: "refs/heads/main", WorkType: "development",
		PromptContext: "prompt context", Body: "issue body", Title: "issue title", MentionContext: "mention",
		ParentContext: "parent", StagePrompt: "stage prompt", StageID: "development",
		StageBudget:    &prompt.StageBudget{MaxDurationSeconds: 1800, MaxSubAgents: 2, MaxTokens: 10000},
		StageLifecycle: map[string]any{"policy": "strict", "requiresApproval": true}, StageSourceEventID: "event-id",
		SystemPromptOverride: "system override",
		Kits: &kit.ToolchainDemand{
			Kits: []string{"go@1"}, OS: "macos", ToolchainInstall: []string{"go version"},
			PostAcquire: []string{"go mod download"}, PreRelease: []string{"go test ./..."}, Env: map[string]string{"GOFLAGS": "-mod=readonly"},
		},
		DisallowedTools: []string{"Bash(rm:*)"}, AllowedTools: []string{"Read", "Bash(go:*)"},
		McpServers: []agent.MCPServerConfig{{
			Name: "platform", Type: "http", URL: "https://mcp.invalid/session", Args: []string{},
			Env: map[string]string{}, Headers: map[string]string{"Authorization": "Bearer MCP_SECRET_DO_NOT_LEAK"},
		}},
		Skills:      []prompt.SkillSpec{{ID: "go-review", Body: "review carefully", DisallowedTools: []string{"Bash(git push:*)"}}},
		MemoryBlock: "remember the contract", Mode: "batch", InitialPrompt: "initial prompt",
		InterviewBudget:     &prompt.InterviewBudget{MaxWallClockSeconds: 900, IdleGraceSeconds: 60},
		InterviewDefinition: json.RawMessage(`{"version":"v1","questions":["scope"]}`),
		CodeIntel:           &prompt.CodeIntelWork{Repo: "RenseiAI/donmai", Ref: "main", RepoPath: "runner", Tools: []string{"af_code_search_symbols"}},
	}
	qw.ResolvedProfile.Provider = agent.ProviderCodex
	qw.ResolvedProfile.Runner = "codex"
	qw.ResolvedProfile.Effort = agent.EffortHigh
	qw.ResolvedProfile.CredentialID = "credential-ref"
	qw.ResolvedProfile.ProviderConfig = map[string]any{"policy": "strict", "temperature": 0.25}
	qw.Branch = "agent/ren-2034"
	lease := workarea.DefaultTerminalLeaseRequest()
	qw.TerminalWorkareaLease = &lease
	return qw
}

func TestOperationalPayloadArchitectureFixture(t *testing.T) {
	qw := fullOperationalFixture()
	canonical, err := CanonicalOperationalPayload(qw)
	if err != nil {
		t.Fatal(err)
	}
	var document map[string]any
	if err := json.Unmarshal(canonical, &document); err != nil {
		t.Fatal(err)
	}
	actualKeys := make([]string, 0, len(document))
	for key := range document {
		actualKeys = append(actualKeys, key)
	}
	sort.Strings(actualKeys)
	expectedKeys := []string{
		"allowedTools", "body", "branch", "codeIntel", "disallowedTools", "initialPrompt", "interviewBudget",
		"interviewDefinition", "issueId", "issueIdentifier", "kits", "linearSessionId", "mcpServers", "memoryBlock",
		"mentionContext", "mode", "organizationId", "parentContext", "projectName", "promptContext", "providerSessionId",
		"ref", "repository", "resolvedProfile", "sessionId", "skills", "stageBudget", "stageId", "stageLifecycle",
		"stagePrompt", "stageSourceEventId", "systemPromptOverride", "terminalWorkareaLease", "title", "workType",
	}
	if fmt.Sprint(actualKeys) != fmt.Sprint(expectedKeys) {
		t.Fatalf("architecture projection keys = %v, want %v", actualKeys, expectedKeys)
	}
	digest, err := DigestOperationalPayload(qw)
	if err != nil {
		t.Fatal(err)
	}
	const architectureDigest = "7a026971ca9122a365025bb655c87469c639aa582cc1c257f2d906c76df707fe"
	if digest != architectureDigest {
		t.Fatalf("architecture digest = %q, want %q; canonical=%s", digest, architectureDigest, canonical)
	}
}

func TestOperationalPayloadProjectionClassifiesEveryQueuedWorkField(t *testing.T) {
	classifications := map[string]string{
		"QueuedWork": "projected", "ResolvedProfile": "projected", "Branch": "projected", "TerminalWorkareaLease": "projected",
		"AdmissionReceipt": "execution-sidecar", "ClaimReceipt": "execution-sidecar", "EffectiveCell": "execution-sidecar",
		"ExecutionRuntimeBinding": "execution-sidecar", "OperationalPayload": "execution-sidecar", "HostAdaptationReceipt": "execution-sidecar",
		"WorkerID": "daemon-runtime", "AuthToken": "daemon-runtime", "PlatformURL": "daemon-runtime", "Capabilities": "daemon-runtime",
	}
	typeOfQueuedWork := reflect.TypeOf(QueuedWork{})
	for index := 0; index < typeOfQueuedWork.NumField(); index++ {
		name := typeOfQueuedWork.Field(index).Name
		if classifications[name] == "" {
			t.Errorf("QueuedWork field %q has no architecture projection classification", name)
		}
		delete(classifications, name)
	}
	for name := range classifications {
		t.Errorf("stale architecture projection classification for missing QueuedWork field %q", name)
	}

	typeOfPrompt := reflect.TypeOf(prompt.QueuedWork{})
	for index := 0; index < typeOfPrompt.NumField(); index++ {
		field := typeOfPrompt.Field(index)
		if field.PkgPath == "" && field.Tag.Get("json") == "-" {
			t.Errorf("prompt operational field %q is not losslessly JSON-projectable", field.Name)
		}
	}
}

func TestOperationalPayloadPreservesPresentEmpty(t *testing.T) {
	absent := exactReceiptQueuedWork("session_empty_distinction")
	present := absent
	present.AllowedTools = []string{}
	absentDigest, err := DigestOperationalPayload(absent)
	if err != nil {
		t.Fatal(err)
	}
	presentDigest, err := DigestOperationalPayload(present)
	if err != nil {
		t.Fatal(err)
	}
	if absentDigest == presentDigest {
		t.Fatal("absent and present-empty top-level slices produced the same digest")
	}

	nilNested := exactReceiptQueuedWork("session_nested_empty_distinction")
	nilNested.McpServers = []agent.MCPServerConfig{{Name: "mcp", Type: "stdio", Command: "mcp"}}
	emptyNested := nilNested
	emptyNested.McpServers = []agent.MCPServerConfig{{Name: "mcp", Type: "stdio", Command: "mcp", Args: []string{}, Env: map[string]string{}}}
	nilDigest, err := DigestOperationalPayload(nilNested)
	if err != nil {
		t.Fatal(err)
	}
	emptyDigest, err := DigestOperationalPayload(emptyNested)
	if err != nil {
		t.Fatal(err)
	}
	if nilDigest == emptyDigest {
		t.Fatal("absent and present-empty nested MCP fields produced the same digest")
	}
}

type operationalPathPart struct {
	key   string
	index int
	isKey bool
}

func collectOperationalLeaves(value any, path []operationalPathPart, leaves *[][]operationalPathPart) {
	switch typed := value.(type) {
	case map[string]any:
		keys := make([]string, 0, len(typed))
		for key := range typed {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		for _, key := range keys {
			collectOperationalLeaves(typed[key], append(path, operationalPathPart{key: key, isKey: true}), leaves)
		}
	case []any:
		if len(typed) == 0 {
			*leaves = append(*leaves, append([]operationalPathPart(nil), path...))
			return
		}
		for index, child := range typed {
			collectOperationalLeaves(child, append(path, operationalPathPart{index: index}), leaves)
		}
	default:
		*leaves = append(*leaves, append([]operationalPathPart(nil), path...))
	}
}

func operationalPathString(path []operationalPathPart) string {
	var parts []string
	for _, part := range path {
		if part.isKey {
			parts = append(parts, part.key)
		} else {
			parts = append(parts, fmt.Sprintf("[%d]", part.index))
		}
	}
	return strings.Join(parts, ".")
}

func mutateOperationalLeaf(root any, path []operationalPathPart) {
	current := root
	for _, part := range path[:len(path)-1] {
		if part.isKey {
			current = current.(map[string]any)[part.key]
		} else {
			current = current.([]any)[part.index]
		}
	}
	last := path[len(path)-1]
	var value any
	if last.isKey {
		value = current.(map[string]any)[last.key]
	} else {
		value = current.([]any)[last.index]
	}
	var mutated any
	switch typed := value.(type) {
	case string:
		mutated = typed + "-mutated"
	case float64:
		mutated = typed + 1
	case bool:
		mutated = !typed
	case []any:
		mutated = []any{"present-empty-mutated"}
	case map[string]any:
		mutated = map[string]any{"present-empty-mutated": true}
	case nil:
		mutated = "null-mutated"
	default:
		panic(fmt.Sprintf("unsupported leaf %T", value))
	}
	if last.isKey {
		current.(map[string]any)[last.key] = mutated
	} else {
		current.([]any)[last.index] = mutated
	}
}

func TestOperationalPayloadEveryLeafMutationDeniesPreSpawn(t *testing.T) {
	provider := &selectorFakeProvider{name: agent.ProviderCodex, harness: agent.HarnessCodex}
	registry := selectorRegistry(t, provider)
	base := fullOperationalFixture()
	base = attachAdmittedExecutionCell(t, base, exactReceiptCell("harness/v2", "gpt-test", executioncell.SessionAutonomous, nil))
	canonical, err := CanonicalOperationalPayload(base)
	if err != nil {
		t.Fatal(err)
	}
	var original any
	if err := json.Unmarshal(canonical, &original); err != nil {
		t.Fatal(err)
	}
	var leaves [][]operationalPathPart
	collectOperationalLeaves(original, nil, &leaves)
	seenTop := map[string]bool{}
	categories := map[string]bool{}
	for _, path := range leaves {
		pathName := operationalPathString(path)
		if len(path) == 0 || !path[0].isKey {
			t.Fatalf("invalid operational path %q", pathName)
		}
		seenTop[path[0].key] = true
		switch path[0].key {
		case "promptContext", "body", "stagePrompt", "initialPrompt":
			categories["prompt"] = true
		case "repository", "ref", "codeIntel":
			categories["repo"] = true
		case "allowedTools", "disallowedTools":
			categories["tools"] = true
		case "mcpServers":
			categories["MCP"] = true
		case "skills":
			categories["skills"] = true
		case "stageLifecycle", "resolvedProfile":
			categories["policy"] = true
		}

		t.Run(pathName, func(t *testing.T) {
			var mutatedDocument any
			raw, _ := json.Marshal(original)
			if err := json.Unmarshal(raw, &mutatedDocument); err != nil {
				t.Fatal(err)
			}
			mutateOperationalLeaf(mutatedDocument, path)
			mutatedRaw, err := json.Marshal(mutatedDocument)
			if err != nil {
				t.Fatal(err)
			}
			var projection OperationalPayload
			if err := json.Unmarshal(mutatedRaw, &projection); err != nil {
				// Some projected fields (currently the fixed terminal-lease
				// profile) have their own closed wire decoder. Rejection there is
				// an even earlier safe denial than admission preflight.
				if provider.spawnCalls.Load() != 0 {
					t.Fatal("closed operational decoder rejection reached provider spawn")
				}
				return
			}
			qw := QueuedWork{
				QueuedWork: projection.QueuedWork, ResolvedProfile: projection.ResolvedProfile, Branch: projection.Branch,
				TerminalWorkareaLease: projection.TerminalWorkareaLease,
				AdmissionReceipt:      append(json.RawMessage(nil), base.AdmissionReceipt...),
				EffectiveCell:         append(json.RawMessage(nil), base.EffectiveCell...),
			}
			admission, err := registry.PreflightHarness(qw)
			var denial *HarnessAdmissionError
			if admission == nil || !errors.As(err, &denial) {
				t.Fatalf("mutated operational payload was not denied by typed preflight: admission=%+v err=%v", admission, err)
			}
			if provider.spawnCalls.Load() != 0 {
				t.Fatal("mutated operational payload reached provider spawn")
			}
			if strings.Contains(err.Error(), "MCP_SECRET_DO_NOT_LEAK") || strings.Contains(string(denial.Receipt.Bytes()), "MCP_SECRET_DO_NOT_LEAK") {
				t.Fatal("operational-payload denial leaked MCP secret material")
			}
		})
	}
	for top := range original.(map[string]any) {
		if !seenTop[top] {
			t.Errorf("projected field %q had no mutation case", top)
		}
	}
	for _, category := range []string{"prompt", "repo", "tools", "MCP", "skills", "policy"} {
		if !categories[category] {
			t.Errorf("required %s mutation category was not exercised", category)
		}
	}
}
