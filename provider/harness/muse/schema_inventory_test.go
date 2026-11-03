package muse

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/RenseiAI/donmai/agent"
)

// Schema inventory audit: this file accounts for every notification in
// the pinned host export and pins the record-only mapping for each
// additional one.
//
// Provenance: exported from a version-verified host binary
// (Muse Code 1.4.14) with the documented command
// (`muse schema generate-json-schema --out <scratch>`), a 60-second
// process timeout, and auto-update disabled
// (MUSE_NO_AUTO_UPDATE=1); exports were written to a scratch
// directory only and are not committed. Baseline limitation: no
// prior export artifact is retained in the repository, so this
// compares the fresh 1.4.14 export against the committed pin and
// projection (schema_fingerprint.go, types.go) — not a retained
// export-to-export diff.
const (
	auditedBinaryVersion     = "1.4.14"
	auditedSchemaVersion     = 1
	auditedExportStable      = true
	auditedExportFingerprint = "sha256:c94c3d1f2f51e6424074b9ec43eac3eedcf532d3dcd77351ec1383a87d3049c6"
)

// exportedNotifications is every method in the export's
// `notifications` table (45): the complete host→client and
// client→server notification surface.
var exportedNotifications = []string{
	"approval/requested",
	"approval/resolved",
	"approval/reviewing",
	"approval/updated",
	"computerUseSetup/changed",
	"initialized",
	"item/completed",
	"item/delta",
	"item/started",
	"item/updated",
	"monitor/inboxItemAdmitted",
	"monitor/inboxItemDrained",
	"monitor/inboxItemVoided",
	"plugin/changed",
	"plugin/marketplaceChanged",
	"recap/completed",
	"session/approvalModeChanged",
	"session/branchChanged",
	"session/closed",
	"session/contextUsage",
	"session/goalChanged",
	"session/leaveCompleted",
	"session/listChanged",
	"session/modelChanged",
	"session/modelRouteUnserved",
	"session/nameChanged",
	"session/reasoningEffortChanged",
	"session/started",
	"session/statusChanged",
	"session/todoListChanged",
	"session/tokenUsage",
	"session/viewHealthChanged",
	"skill/changed",
	"turn/completed",
	"turn/foregroundCompleted",
	"turn/modelRetryScheduled",
	"turn/retracted",
	"turn/retryScheduled",
	"turn/started",
	"turn/unqueued",
	"usage/changed",
	"userInput/engaged",
	"userInput/requested",
	"userInput/settled",
	"view/gap",
}

// exportedServerRequests is every method in the export's `requests`
// table (2): server-initiated must-answer requests, answered on the
// server-request door in handle.go — never notification dispatch.
var exportedServerRequests = []string{
	"approval/request",
	"userInput/request",
}

// mappedSpecialMethods is every exported notification with a
// dedicated mapper case in event_mapping.go (22). Their behavior is
// pinned by the existing mapper/Spawn/stream tests; this list exists
// so the inventory below accounts for them by name.
var mappedSpecialMethods = []string{
	"approval/requested",
	"approval/resolved",
	"approval/updated",
	"item/completed",
	"item/delta",
	"item/started",
	"item/updated",
	"session/approvalModeChanged",
	"session/closed",
	"session/contextUsage",
	"session/started",
	"session/statusChanged",
	"session/todoListChanged",
	"session/tokenUsage",
	"turn/completed",
	"turn/foregroundCompleted",
	"turn/retracted",
	"turn/retryScheduled",
	"turn/started",
	"turn/unqueued",
	"usage/changed",
	"view/gap",
}

// reservedMethods are exported notifications reserved for dedicated
// mappings (approval/reviewing, session/branchChanged): classified
// here, still yielding the unknown diagnostic until those land.
var reservedMethods = []string{
	"approval/reviewing",
	"session/branchChanged",
}

// clientToServerMethods are exported notifications that travel
// client→server only (initialized closes the handshake;
// userInput/engaged is a prompt-engagement note): never inbound,
// still yielding the unknown diagnostic if one ever arrives.
var clientToServerMethods = []string{
	"initialized",
	"userInput/engaged",
}

// recordSourceRange is the shared synthetic provenance token carried
// by the representative payloads below.
const recordSourceRange = `{"first":{"id":"e1","sequence":1},"last":{"id":"e1","sequence":1},"stream":{"id":"run","kind":"run"}}`

// handleOnlyMethods are record-only methods the Spawn fixture cannot
// deliver: their $defs carry no top-level sessionId, so client
// dispatch drops them before the mapper (the pre-existing
// session/started precedent — production subscribes per session and
// never globally). They are covered at mapper level and handle
// level; changing dispatch to route them is out of scope.
var handleOnlyMethods = map[string]string{
	"session/listChanged": "SessionListChangedParams carries no top-level sessionId",
}

// recordOnlyCases is the record-only allowlist under test: method,
// expected subtype, and a representative redacted payload carrying
// the export's required members with synthetic values ("__SID__" is
// replaced with the live session id; session/listChanged carries no
// top-level sessionId per its $def). Payloads are shape carriers for
// Raw passthrough, not schema-validated instances: the harness never
// parses record-only params.
var recordOnlyCases = []struct {
	method  string
	subtype string
	params  string
}{
	{"computerUseSetup/changed", "computer_use_setup_changed", `{"sessionId":"__SID__","state":{"accessibility":"granted","inputMonitoring":"granted","screenRecording":"missing","setup":"readyForTarget"}}`},
	{"monitor/inboxItemAdmitted", "monitor_inbox_item_admitted", `{"body":"admitted body","eventIndex":7,"eventKind":"tick","itemId":"mon-1","queuedAt":"2026-01-01T00:00:00Z","sessionId":"__SID__","showLines":false,"sourceRange":` + recordSourceRange + `,"summary":"admitted","taskId":"task-1","viewCursor":"cursor-1"}`},
	{"monitor/inboxItemDrained", "monitor_inbox_item_drained", `{"deliveryVisible":true,"drainedAt":"2026-01-01T00:00:00Z","itemId":"mon-1","sessionId":"__SID__","sourceRange":` + recordSourceRange + `,"viewCursor":"cursor-2"}`},
	{"monitor/inboxItemVoided", "monitor_inbox_item_voided", `{"itemId":"mon-1","sessionId":"__SID__","sourceRange":` + recordSourceRange + `,"viewCursor":"cursor-3","voidedAt":"2026-01-01T00:00:00Z"}`},
	{"plugin/changed", "plugin_changed", `{"enabled":true,"id":"plugin-1","sessionId":"__SID__","source":"marketplace","version":"1.0.0"}`},
	{"plugin/marketplaceChanged", "plugin_marketplace_changed", `{"change":"added","name":"market-1","sessionId":"__SID__"}`},
	{"recap/completed", "recap_completed", `{"commandId":"cmd-1","outcome":"completed","sessionId":"__SID__","text":"recap text","title":"recap"}`},
	{"session/goalChanged", "session_goal_changed", `{"goal":{"objective":"ship it","percentComplete":50,"status":"in_progress"},"sessionId":"__SID__","sourceRange":` + recordSourceRange + `,"viewCursor":"cursor-4"}`},
	{"session/leaveCompleted", "session_leave_completed", `{"commandId":"cmd-2","outcome":"released","sessionId":"__SID__"}`},
	{"session/listChanged", "session_list_changed", `{"session":{"sessionId":"other-1","status":"idle"}}`},
	{"session/modelChanged", "session_model_changed", `{"modelId":"model-1","providerId":"meta","sessionId":"__SID__","source":"user","sourceRange":` + recordSourceRange + `,"viewCursor":"cursor-5"}`},
	{"session/modelRouteUnserved", "session_model_route_unserved", `{"commandId":"cmd-3","installedProviderId":"prov-2","modelId":"model-1","providerId":"prov-1","sessionId":"__SID__","sourceRange":` + recordSourceRange + `,"viewCursor":"cursor-6"}`},
	{"session/nameChanged", "session_name_changed", `{"name":"new-name","sessionId":"__SID__","sourceRange":` + recordSourceRange + `,"viewCursor":"cursor-7"}`},
	{"session/reasoningEffortChanged", "session_reasoning_effort_changed", `{"reasoningEffort":"low","sessionId":"__SID__","source":"user","sourceRange":` + recordSourceRange + `,"viewCursor":"cursor-8"}`},
	{"session/viewHealthChanged", "session_view_health_changed", `{"health":"unavailable","sessionId":"__SID__"}`},
	{"skill/changed", "skill_changed", `{"sessionId":"__SID__"}`},
	{"turn/modelRetryScheduled", "turn_model_retry_scheduled", `{"attempt":1,"maxAttempts":3,"nextAttempt":"2026-01-01T00:00:01Z","retryDelayMs":1000,"sessionId":"__SID__","sourceRange":` + recordSourceRange + `,"turnId":"turn-1","viewCursor":"cursor-9"}`},
	{"userInput/requested", "user_input_prompt_opened", `{"itemId":"tool-9","sessionId":"__SID__","toolCallId":"call-9","toolName":"ask_user","turnId":"turn-1","userInputId":"ui-1","viewCursor":"cursor-10"}`},
	{"userInput/settled", "user_input_settled", `{"answers":{},"clarification":false,"decidedAt":"2026-01-01T00:00:00Z","decidedByCommandId":"cmd-4","outcome":"answered","reason":"user replied","sessionId":"__SID__","sourceRange":` + recordSourceRange + `,"userInputId":"ui-1","viewCursor":"cursor-11"}`},
}

// TestSchemaInventory_AccountsForEveryExportedNotification compares
// the audited 1.4.14 export against the committed contract: the pin
// matches the export manifest exactly, the production allowlist
// matches the inventoried record-only set exactly, and every
// exported notification is classified exactly once (special case,
// record-only, reserved for dedicated mappings, or client-to-server). RED controls:
// remove one method from the inventory, or misclassify a server
// request as a notification, and this fails.
func TestSchemaInventory_AccountsForEveryExportedNotification(t *testing.T) {
	if schemaExportFingerprint != auditedExportFingerprint {
		t.Errorf("pin %q != audited %s export fingerprint %q: re-run the schema audit",
			schemaExportFingerprint, auditedBinaryVersion, auditedExportFingerprint)
	}
	if schemaExportVersion != auditedSchemaVersion {
		t.Errorf("schemaExportVersion=%d, want audited %d", schemaExportVersion, auditedSchemaVersion)
	}
	if schemaSurfaceStable() != auditedExportStable {
		t.Errorf("schemaSurfaceStable()=%v, want audited stable=%v", schemaSurfaceStable(), auditedExportStable)
	}

	wantAllowlist := make(map[string]string, len(recordOnlyCases))
	for _, tc := range recordOnlyCases {
		wantAllowlist[tc.method] = tc.subtype
	}
	if !reflect.DeepEqual(recordOnlyNotifications, wantAllowlist) {
		t.Errorf("production allowlist != inventoried record-only set:\nproduction=%v\ninventoried=%v",
			recordOnlyNotifications, wantAllowlist)
	}
	seenSubtypes := map[string]string{}
	for method, subtype := range recordOnlyNotifications {
		if prev, dup := seenSubtypes[subtype]; dup {
			t.Errorf("subtype %q shared by %q and %q: record-only subtypes must be distinct", subtype, prev, method)
		}
		seenSubtypes[subtype] = method
		if subtype == "unknown" || subtype == "user_input_requested" {
			t.Errorf("method %q reuses reserved subtype %q", method, subtype)
		}
	}

	classified := map[string]string{}
	claim := func(method, class string) {
		if prev, dup := classified[method]; dup {
			t.Errorf("method %q classified twice (%s, %s)", method, prev, class)
		}
		classified[method] = class
	}
	for _, m := range mappedSpecialMethods {
		claim(m, "special")
	}
	for m := range wantAllowlist {
		claim(m, "record-only")
	}
	for _, m := range reservedMethods {
		claim(m, "reserved for dedicated mappings")
	}
	for _, m := range clientToServerMethods {
		claim(m, "client-to-server")
	}
	for _, m := range exportedNotifications {
		if _, ok := classified[m]; !ok {
			t.Errorf("exported notification %q has no classification", m)
		}
	}
	for m, class := range classified {
		found := false
		for _, e := range exportedNotifications {
			if e == m {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("classified method %q (%s) is not in the audited export", m, class)
		}
	}
	for _, r := range exportedServerRequests {
		if class, ok := classified[r]; ok {
			t.Errorf("server request %q misclassified as notification (%s)", r, class)
		}
		if _, ok := recordOnlyNotifications[r]; ok {
			t.Errorf("server request %q is allowlisted: requests must stay off the notification door", r)
		}
	}
}

// TestRecordOnly_MapperYieldsNamedNonterminalEvent pins the mapper
// half of the allowlist: one named SystemEvent per method, neutral
// method message, Raw preserved verbatim. RED-on-revert: remove an
// allowlist entry and its case yields unknown here.
func TestRecordOnly_MapperYieldsNamedNonterminalEvent(t *testing.T) {
	for _, tc := range recordOnlyCases {
		t.Run(tc.method, func(t *testing.T) {
			state := newMapperState("m")
			raw := map[string]any{"marker": "raw-" + tc.method}
			events := mapNotification(tc.method, json.RawMessage(tc.params), state, raw)
			if len(events) != 1 {
				t.Fatalf("events=%v, want exactly one", kindsOf(events))
			}
			sys, ok := events[0].(agent.SystemEvent)
			if !ok {
				t.Fatalf("event is %T, want SystemEvent", events[0])
			}
			if sys.Kind() != agent.EventSystem {
				t.Errorf("kind=%v, want nonterminal %v", sys.Kind(), agent.EventSystem)
			}
			if sys.Subtype != tc.subtype {
				t.Errorf("subtype=%q, want %q", sys.Subtype, tc.subtype)
			}
			if want := "Muse notification: " + tc.method; sys.Message != want {
				t.Errorf("message=%q, want %q", sys.Message, want)
			}
			if !reflect.DeepEqual(sys.Raw, raw) {
				t.Errorf("Raw=%v, want preserved %v", sys.Raw, raw)
			}
		})
	}
}

// TestRecordOnly_SpawnYieldsNamedEventThenTerminal drives every
// Spawn-deliverable allowlisted method through the production Spawn
// path on the shared record-only fixture: the named nonterminal
// event carries the sent params as Raw, followed by normal terminal
// completion. Methods in handleOnlyMethods skip here (their $defs
// carry no sessionId, so dispatch drops them) and are covered at
// handle level by TestRecordOnly_HandleLevelCoversSessionIdLess.
func TestRecordOnly_SpawnYieldsNamedEventThenTerminal(t *testing.T) {
	for _, tc := range recordOnlyCases {
		t.Run(tc.method, func(t *testing.T) {
			if reason, ok := handleOnlyMethods[tc.method]; ok {
				t.Skipf("handle-level only: %s", reason)
			}
			p, fh := newTestProvider(t, "record-only")
			fh.mu.Lock()
			fh.recordMethod = tc.method
			fh.recordParams = tc.params
			fh.mu.Unlock()

			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			h, err := p.Spawn(ctx, agent.Spec{Prompt: "do work", Cwd: t.TempDir(), Autonomous: true})
			if err != nil {
				t.Fatalf("Spawn: %v", err)
			}
			events := drainEvents(t, h.Events(), 10*time.Second)

			recordIdx := -1
			for i, ev := range events {
				sys, ok := ev.(agent.SystemEvent)
				if !ok || sys.Subtype != tc.subtype {
					continue
				}
				if recordIdx >= 0 {
					t.Fatalf("subtype %q emitted twice; kinds=%v", tc.subtype, kindsOf(events))
				}
				recordIdx = i
				var wantRaw map[string]any
				sent := strings.ReplaceAll(tc.params, "__SID__", h.SessionID())
				if err := json.Unmarshal([]byte(sent), &wantRaw); err != nil {
					t.Fatalf("fixture params are not valid JSON: %v", err)
				}
				gotRaw, ok := sys.Raw.(map[string]any)
				if !ok {
					t.Fatalf("Raw is %T, want the params object", sys.Raw)
				}
				if !reflect.DeepEqual(gotRaw, wantRaw) {
					t.Errorf("Raw=%v, want sent params %v", gotRaw, wantRaw)
				}
				if want := "Muse notification: " + tc.method; sys.Message != want {
					t.Errorf("message=%q, want %q", sys.Message, want)
				}
			}
			if recordIdx < 0 {
				t.Fatalf("no %q event; kinds=%v", tc.subtype, kindsOf(events))
			}
			var terminals []int
			for i, ev := range events {
				if r, ok := ev.(agent.ResultEvent); ok {
					terminals = append(terminals, i)
					if !r.Success {
						t.Errorf("terminal ResultEvent failed: %+v", r)
					}
				}
			}
			if len(terminals) != 1 || terminals[0] != len(events)-1 {
				t.Fatalf("want exactly one terminal ResultEvent last; kinds=%v", kindsOf(events))
			}
			if recordIdx > terminals[0] {
				t.Errorf("record event sorts after the terminal")
			}
		})
	}
}

// TestRecordOnly_HandleLevelCoversSessionIdLess covers every
// handleOnlyMethods entry through handleNotification: the named
// nonterminal event carries the params as Raw and the stream stays
// open. This is the transport boundary: client dispatch drops
// sessionId-less notifications before the mapper (the pre-existing
// session/started precedent), so these methods cannot traverse
// Spawn, but everything from the handle inward is pinned here.
func TestRecordOnly_HandleLevelCoversSessionIdLess(t *testing.T) {
	if len(handleOnlyMethods) == 0 {
		t.Fatal("handleOnlyMethods is empty: the Spawn test skips nothing, this test covers nothing")
	}
	byMethod := make(map[string]string, len(recordOnlyCases))
	for _, tc := range recordOnlyCases {
		byMethod[tc.method] = tc.subtype
	}
	for method := range handleOnlyMethods {
		t.Run(method, func(t *testing.T) {
			subtype, ok := byMethod[method]
			if !ok {
				t.Fatalf("%q is handle-only but not allowlisted", method)
			}
			var params string
			for _, tc := range recordOnlyCases {
				if tc.method == method {
					params = tc.params
				}
			}
			h := newHandle(nil, agent.Spec{Prompt: "x"}, NewSpawnPlan(agent.Spec{Prompt: "x"}, t.TempDir()), HandleOptions{})
			if done := h.handleNotification(context.Background(), notification{Method: method, Params: json.RawMessage(params)}); done {
				t.Fatalf("%q ended the stream: record-only must be nonterminal", method)
			}
			select {
			case ev := <-h.Events():
				sys, ok := ev.(agent.SystemEvent)
				if !ok {
					t.Fatalf("event is %T, want SystemEvent", ev)
				}
				if sys.Subtype != subtype {
					t.Errorf("subtype=%q, want %q", sys.Subtype, subtype)
				}
				if want := "Muse notification: " + method; sys.Message != want {
					t.Errorf("message=%q, want %q", sys.Message, want)
				}
				var wantRaw map[string]any
				if err := json.Unmarshal([]byte(params), &wantRaw); err != nil {
					t.Fatalf("fixture params are not valid JSON: %v", err)
				}
				gotRaw, ok := sys.Raw.(map[string]any)
				if !ok {
					t.Fatalf("Raw is %T, want the params object", sys.Raw)
				}
				if !reflect.DeepEqual(gotRaw, wantRaw) {
					t.Errorf("Raw=%v, want sent params %v", gotRaw, wantRaw)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("no event emitted")
			}
		})
	}
}

// TestSchemaInventory_OutOfScopeStaysUnknown pins the diagnostic
// contract for everything outside the allowlist: reserved for dedicated mappings and
// client-to-server methods, plus a method absent from the export,
// still yield the unknown diagnostic with Raw preserved. RED
// control: broaden the allowlist fallback to all methods and this
// fails.
func TestSchemaInventory_OutOfScopeStaysUnknown(t *testing.T) {
	for _, method := range []string{
		"approval/reviewing",
		"session/branchChanged",
		"initialized",
		"userInput/engaged",
		"session/noSuchNotification",
	} {
		t.Run(method, func(t *testing.T) {
			state := newMapperState("m")
			raw := map[string]any{"marker": "raw-" + method}
			events := mapNotification(method, json.RawMessage(`{"sessionId":"s"}`), state, raw)
			if len(events) != 1 {
				t.Fatalf("events=%v, want exactly one", kindsOf(events))
			}
			sys, ok := events[0].(agent.SystemEvent)
			if !ok || sys.Subtype != "unknown" {
				t.Errorf("event=%+v, want unknown SystemEvent", events[0])
			}
			if !reflect.DeepEqual(sys.Raw, raw) {
				t.Errorf("Raw=%v, want preserved %v", sys.Raw, raw)
			}
		})
	}
}

// TestSchemaInventory_ServerRequestsStayOffTheNotificationDoor pins
// the door separation: the must-answer server requests are not
// notification allowlist entries, and if one ever reached the
// mapper it would yield unknown rather than a fabricated mapping.
func TestSchemaInventory_ServerRequestsStayOffTheNotificationDoor(t *testing.T) {
	for _, method := range exportedServerRequests {
		t.Run(method, func(t *testing.T) {
			if _, ok := recordOnlyNotifications[method]; ok {
				t.Errorf("%q is allowlisted: requests must stay off the notification door", method)
			}
			events := mapNotification(method, json.RawMessage(`{"sessionId":"s"}`), newMapperState("m"), nil)
			if len(events) != 1 {
				t.Fatalf("events=%v, want exactly one", kindsOf(events))
			}
			sys, ok := events[0].(agent.SystemEvent)
			if !ok || sys.Subtype != "unknown" {
				t.Errorf("event=%+v, want unknown SystemEvent", events[0])
			}
		})
	}
}
