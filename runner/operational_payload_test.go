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
		SessionID: "session_operational_fixture", SessionName: "chief-of-staff", IssueID: "issue-id", IssueIdentifier: "fixture-2034",
		LinearSessionID: "linear-session", ProviderSessionID: "provider-session", ProjectName: "Donmai",
		OrganizationID: "org-id", Repository: "RenseiAI/donmai", Ref: "refs/heads/main", WorkType: "development",
		PromptContext: "prompt context", Body: "issue body", Title: "issue title", MentionContext: "mention",
		ParentContext: "parent", StagePrompt: "stage prompt", StageID: "development",
		StageBudget:    &prompt.StageBudget{MaxDurationSeconds: 1800, MaxSubAgents: subAgentCap(2), MaxTokens: 10000},
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
	qw.Env = map[string]string{"PUBLIC_SESSION_FEATURE": "enabled"}
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
		"allowedTools", "body", "branch", "codeIntel", "disallowedTools", "env", "initialPrompt", "interviewBudget",
		"interviewDefinition", "issueId", "issueIdentifier", "kits", "linearSessionId", "mcpServers", "memoryBlock",
		"mentionContext", "mode", "organizationId", "parentContext", "projectName", "promptContext", "providerSessionId",
		"ref", "repository", "resolvedProfile", "sessionId", "sessionName", "skills", "stageBudget", "stageId", "stageLifecycle",
		"stagePrompt", "stageSourceEventId", "systemPromptOverride", "terminalWorkareaLease", "title", "workType",
	}
	if fmt.Sprint(actualKeys) != fmt.Sprint(expectedKeys) {
		t.Fatalf("architecture projection keys = %v, want %v", actualKeys, expectedKeys)
	}
	digest, err := DigestOperationalPayload(qw)
	if err != nil {
		t.Fatal(err)
	}
	const architectureDigest = "be9627f0f0e80240961ca18b1c48684291e90542a91058b62ac734976f035f4e"
	if digest != architectureDigest {
		t.Fatalf("architecture digest = %q, want %q; canonical=%s", digest, architectureDigest, canonical)
	}
}

func TestOperationalEnvironmentIsDigestBoundAndDefensivelyProjected(t *testing.T) {
	base := exactReceiptQueuedWork("operational-env-binding")
	absent, err := CanonicalOperationalPayload(base)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(absent), `"env"`) {
		t.Fatalf("absent environment changed the legacy operational shape: %s", absent)
	}
	withoutEnvironment, err := DigestOperationalPayload(base)
	if err != nil {
		t.Fatal(err)
	}
	base.Env = map[string]string{
		"PUBLIC_SERVICE_ORIGIN": "https://service.example.invalid",
		"SESSION_FEATURES":      `["draft"]`,
	}
	base.AuthToken = "worker-runtime-bearer"
	base.McpAuthToken = "session-runtime-bearer"
	withEnvironment, err := DigestOperationalPayload(base)
	if err != nil {
		t.Fatal(err)
	}
	if withEnvironment == withoutEnvironment {
		t.Fatal("operational environment did not change the admission digest")
	}
	projected := ProjectOperationalPayload(base)
	if !reflect.DeepEqual(projected.Env, base.Env) {
		t.Fatalf("projected env = %#v, want %#v", projected.Env, base.Env)
	}
	projected.Env["SESSION_FEATURES"] = "changed"
	if base.Env["SESSION_FEATURES"] != `["draft"]` {
		t.Fatal("projected environment aliases queued work")
	}
	canonical, err := CanonicalOperationalPayload(base)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(canonical), base.AuthToken) ||
		strings.Contains(string(canonical), base.McpAuthToken) {
		t.Fatal("runtime bearer entered canonical operational environment")
	}
}

func TestOperationalPayloadProjectionClassifiesEveryQueuedWorkField(t *testing.T) {
	classifications := map[string]string{
		"QueuedWork": "projected", "RepositoryDeclaration": "projected", "ResolvedProfile": "projected", "Branch": "projected", "TerminalWorkareaLease": "projected", "ContextPack": "projected",
		"WorkareaMode": "projected", "ParentWorkareaID": "projected", "RepositoryFilter": "projected", "CacheSeedID": "projected",
		// The dispatched pull request decides which commits the workarea
		// materializes, so it is admission-bound like every other workarea
		// intent: a mirror that could name a different head than the receipted
		// payload would defeat the head verification entirely.
		"PullRequest":       "projected",
		"PermissionProfile": "projected", "Env": "projected",
		// The per-seat resource budget changes what the harness executes
		// (the worker-cap env the runner overlays on the harness Spec.Env),
		// so it is admission-bound like Branch and Env: two seats that
		// verify the same receipt must run under the same constraints.
		"SeatBudget": "projected",
		// The stamped execution-security levels decide how much the session
		// may do; they are admission-bound like every other intent.
		"ExecutionSecurity": "projected",
		// A request to start a human-controlled session in bypass changes
		// the permission mode the harness starts in, so it is admission-bound
		// like the levels it depends on.
		"StartPermissionMode": "projected",
		"AdmissionReceipt":    "execution-sidecar", "ClaimReceipt": "execution-sidecar", "EffectiveCell": "execution-sidecar",
		"ExecutionRuntimeBinding": "execution-sidecar", "OperationalPayload": "execution-sidecar", "HostAdaptationReceipt": "execution-sidecar",
		"toolLifecycleProfileID": "execution-sidecar",
		// Trusted registry policy; local admission binds it in producer evidence,
		// never as a caller-selectable operational payload field.
		"runtimeTransport": "daemon-runtime",
		"WorkerID":         "daemon-runtime", "AuthToken": "daemon-runtime", "PlatformURL": "daemon-runtime", "Capabilities": "daemon-runtime",
		// The session-scoped MCP bearer and its advisory expiry are runtime
		// credentials, classified beside AuthToken: the daemon supplies them
		// per spawn and they must stay OUT of the canonical projection, which
		// is digested and compared across the wire.
		"McpAuthToken": "daemon-runtime", "McpAuthTokenExpiresAt": "daemon-runtime",
		// Commit credit is work-source metadata restored from the validated
		// session detail, like the MCP bearer; it does not select execution.
		"CoAuthorName": "daemon-runtime", "CoAuthorEmail": "daemon-runtime",
		// RetainRecording is a LOCAL OPERATOR decision (the standalone
		// --keep-recording flag) with json:"-" — it never rides the wire and
		// must never influence admission evidence, so it stays out of the
		// canonical projection alongside the other daemon-runtime annotations.
		"RetainRecording": "daemon-runtime",
		// DeferFailureTransition is a per-work policy flag that gates only
		// the post-session tracker transition, not admission: like
		// Capabilities (which gates the merge-queue deferral), it is
		// supplied per spawn by the daemon/detail path and stays out of
		// the canonical admission projection.
		"DeferFailureTransition": "daemon-runtime",
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

func TestOperationalPayloadSeatBudgetIsDigestBoundAndDefensivelyProjected(t *testing.T) {
	base := fullOperationalFixture()
	absent, err := CanonicalOperationalPayload(base)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(absent), "seatBudget") {
		t.Fatalf("absent budget changed the legacy operational shape: %s", absent)
	}
	budgeted := base
	budgeted.SeatBudget = &SeatBudget{Mode: "best-effort", CPUs: 2, MemoryMB: 4096, Detail: "caps"}
	withBudget, err := CanonicalOperationalPayload(budgeted)
	if err != nil {
		t.Fatal(err)
	}
	if string(withBudget) == string(absent) {
		t.Fatal("seat budget did not change the admission digest: two seats could verify one receipt yet run under different constraints")
	}
	swapped := budgeted
	swapped.SeatBudget = &SeatBudget{Mode: "enforced", CPUs: 8, MemoryMB: 8192, Detail: "scope"}
	swappedCanonical, err := CanonicalOperationalPayload(swapped)
	if err != nil {
		t.Fatal(err)
	}
	if string(swappedCanonical) == string(withBudget) {
		t.Fatal("seat budget values did not change the admission digest")
	}
	// The projection is a defensive copy: mutating it must not move the
	// queued work it was projected from.
	projected := ProjectOperationalPayload(budgeted)
	if projected.SeatBudget == nil || projected.SeatBudget.CPUs != 2 || projected.SeatBudget.Mode != "best-effort" {
		t.Fatalf("projected seat budget = %+v; want best-effort 2 cpu", projected.SeatBudget)
	}
	projected.SeatBudget.CPUs = 99
	if budgeted.SeatBudget.CPUs != 2 {
		t.Fatal("projected seat budget aliases queued work")
	}
	if ProjectOperationalPayload(base).SeatBudget != nil {
		t.Fatal("absent seat budget projected non-nil; want nil (legacy shape preserved)")
	}
	if digest, err := DigestOperationalPayload(budgeted); err != nil {
		t.Fatal(err)
	} else if digest == "" {
		t.Fatal("empty seat-budget digest")
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

// TestOperationalPayloadDeliveryPolicyIsDigestBound pins that the
// dispatch-declared delivery policy rides the admission digest: it
// decides what counts as delivered work, so two runs that verify one
// receipt must run under the same rule. Absent stays absent (legacy
// shape preserved).
func TestOperationalPayloadDeliveryPolicyIsDigestBound(t *testing.T) {
	base := exactReceiptQueuedWork("operational-delivery-binding")
	absent, err := CanonicalOperationalPayload(base)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(absent), `"delivery"`) {
		t.Fatalf("absent policy changed the legacy operational shape: %s", absent)
	}
	withoutPolicy, err := DigestOperationalPayload(base)
	if err != nil {
		t.Fatal(err)
	}
	withDraft := base
	withDraft.Delivery = &prompt.DeliveryPolicy{AllowDraft: true}
	withDraftDigest, err := DigestOperationalPayload(withDraft)
	if err != nil {
		t.Fatal(err)
	}
	if withDraftDigest == withoutPolicy {
		t.Fatal("delivery policy did not change the admission digest: two runs could verify one receipt yet run under different delivery rules")
	}
	withMerges := base
	withMerges.Delivery = &prompt.DeliveryPolicy{AllowMergeCommits: true}
	withMergesDigest, err := DigestOperationalPayload(withMerges)
	if err != nil {
		t.Fatal(err)
	}
	if withMergesDigest == withoutPolicy || withMergesDigest == withDraftDigest {
		t.Fatal("delivery policy values did not change the admission digest")
	}
	withClean := base
	withClean.Delivery = &prompt.DeliveryPolicy{AllowCleanMerge: true}
	withCleanDigest, err := DigestOperationalPayload(withClean)
	if err != nil {
		t.Fatal(err)
	}
	if withCleanDigest == withoutPolicy || withCleanDigest == withDraftDigest || withCleanDigest == withMergesDigest {
		t.Fatal("clean-merge intent did not change the admission digest")
	}
	projected := ProjectOperationalPayload(withDraft)
	if projected.Delivery == nil || !projected.Delivery.AllowDraft || projected.Delivery.AllowMergeCommits {
		t.Fatalf("projected delivery = %+v; want the stamped policy", projected.Delivery)
	}
	projected.Delivery.AllowDraft = false
	if !withDraft.Delivery.AllowDraft {
		t.Fatal("projected delivery aliases queued work")
	}
	if ProjectOperationalPayload(base).Delivery != nil {
		t.Fatal("absent delivery policy projected non-nil; want nil (legacy shape preserved)")
	}
}

// TestOperationalPayloadStartPermissionModeIsDigestBound pins that the
// start-mode request is part of the admission projection when present, is
// absent from it (keeping the pre-field digest) when not, and that a
// producer hashing the JSON it sends and a verifier hashing the typed
// projection it decoded agree with and without the field.
func TestOperationalPayloadStartPermissionModeIsDigestBound(t *testing.T) {
	t.Parallel()

	absent := fullOperationalFixture()
	absentCanonical, err := CanonicalOperationalPayload(absent)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(absentCanonical), "startPermissionMode") {
		t.Fatalf("absent request changed the legacy operational shape: %s", absentCanonical)
	}
	requested := absent
	requested.StartPermissionMode = StartPermissionModeBypass
	requestedCanonical, err := CanonicalOperationalPayload(requested)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(requestedCanonical), `"startPermissionMode":"bypass"`) {
		t.Fatalf("requested start mode missing from the operational projection: %s", requestedCanonical)
	}
	if string(requestedCanonical) == string(absentCanonical) {
		t.Fatal("start-mode request did not change the admission digest")
	}

	for _, tc := range []struct {
		name string
		mode StartPermissionMode
	}{
		{name: "without field"},
		{name: "with bypass", mode: StartPermissionModeBypass},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			typed := QueuedWork{QueuedWork: prompt.QueuedWork{SessionID: "session-parity", Mode: "interactive", Title: "parity"}, StartPermissionMode: tc.mode}
			typedDigest, err := DigestOperationalPayload(typed)
			if err != nil {
				t.Fatal(err)
			}
			sent, err := json.Marshal(ProjectOperationalPayload(typed))
			if err != nil {
				t.Fatal(err)
			}
			producerDigest, err := DigestOperationalPayload(QueuedWork{OperationalPayload: sent})
			if err != nil {
				t.Fatal(err)
			}
			if producerDigest != typedDigest {
				t.Fatalf("producer digest %s != verifier digest %s for %s", producerDigest, typedDigest, sent)
			}
			var decoded QueuedWork
			if err := json.Unmarshal(sent, &decoded); err != nil {
				t.Fatalf("decode sent payload: %v", err)
			}
			if decoded.StartPermissionMode != tc.mode {
				t.Fatalf("decoded startPermissionMode = %q, want %q", decoded.StartPermissionMode, tc.mode)
			}
			decodedDigest, err := DigestOperationalPayload(decoded)
			if err != nil {
				t.Fatal(err)
			}
			if decodedDigest != typedDigest {
				t.Fatalf("decoded projection digest %s != sent digest %s", decodedDigest, typedDigest)
			}
		})
	}
}

// TestQueuedWorkStartPermissionModeClosedDecoder pins the wire contract of
// startPermissionMode: "bypass" is the only value, absence and JSON null
// read as no request, and every other present value fails the work item's
// decode the way a malformed executionSecurity section does.
func TestQueuedWorkStartPermissionModeClosedDecoder(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		payload string
		want    StartPermissionMode
		wantErr bool
	}{
		{name: "bypass", payload: `{"sessionId":"s","startPermissionMode":"bypass"}`, want: StartPermissionModeBypass},
		{name: "absent (older control plane)", payload: `{"sessionId":"s"}`},
		{name: "null", payload: `{"sessionId":"s","startPermissionMode":null}`},
		{name: "empty string", payload: `{"sessionId":"s","startPermissionMode":""}`, wantErr: true},
		{name: "unknown mode", payload: `{"sessionId":"s","startPermissionMode":"acceptEdits"}`, wantErr: true},
		{name: "wrong case", payload: `{"sessionId":"s","startPermissionMode":"Bypass"}`, wantErr: true},
		{name: "not a string", payload: `{"sessionId":"s","startPermissionMode":true}`, wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var qw QueuedWork
			err := json.Unmarshal([]byte(tc.payload), &qw)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("decode accepted %s as %q; want refusal", tc.payload, qw.StartPermissionMode)
				}
				if !strings.Contains(err.Error(), "startPermissionMode") {
					t.Fatalf("refusal does not name the field: %v", err)
				}
				if agent.ExecutionSecurityErrorCode(err) != agent.ExecutionSecurityUnresolvable {
					t.Fatalf("refusal = %v, want the typed execution_security_unresolvable refusal", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("decode %s: %v", tc.payload, err)
			}
			if qw.StartPermissionMode != tc.want {
				t.Fatalf("startPermissionMode = %q, want %q", qw.StartPermissionMode, tc.want)
			}
			if qw.SessionID != "s" {
				t.Fatalf("sessionId = %q, want s (the embedded payload must still decode)", qw.SessionID)
			}
		})
	}
}
