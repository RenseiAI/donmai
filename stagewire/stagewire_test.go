package stagewire

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

// fullFieldsJSON is a work item carrying every 1.0 session field, in the
// shape a control plane emits.
const fullFieldsJSON = `{
	"stage": {"id": "stg_1", "key": "backlog", "name": "Backlog", "role": "intake", "runRole": "inflight"},
	"card": {"id": "card_1", "handle": "builder", "contract": {"pr": true, "verdict": false, "workResult": true}},
	"transitions": {
		"start": {"stageKey": "in-progress", "writeStatus": "state-started"},
		"success": {"stageKey": "review", "writeStatus": "state-review"},
		"failure": "stay",
		"blocked": {"stageKey": "icebox", "writeStatus": "state-icebox"},
		"close": {"stageKey": "closed", "writeStatus": "state-closed"}
	},
	"retry": {"retries": 2, "backoffSeconds": 60},
	"stageContext": {
		"stage": {"key": "backlog", "name": "Backlog", "role": "intake"},
		"sourceStatus": "Backlog",
		"card": {"id": "card_1", "handle": "builder"},
		"reason": "default",
		"parent": {"identifier": "PRJ-1", "title": "Parent work"},
		"children": [{"identifier": "PRJ-2", "status": "Done"}, {"identifier": "PRJ-3", "status": null}]
	},
	"dispatchReason": "mention"
}`

func strPtr(s string) *string { return &s }

func wantFullFields() Fields {
	return Fields{
		Stage: &Stage{ID: "stg_1", Key: "backlog", Name: "Backlog", Role: RoleIntake, RunRole: RoleInflight},
		Card:  &Card{ID: "card_1", Handle: "builder", Contract: &Contract{PR: true, WorkResult: true}},
		Transitions: &Transitions{
			Start:   &Transition{StageKey: "in-progress", WriteStatus: "state-started"},
			Success: &Transition{StageKey: "review", WriteStatus: "state-review"},
			Failure: &Transition{},
			Blocked: &Transition{StageKey: "icebox", WriteStatus: "state-icebox"},
			Close:   &Transition{StageKey: "closed", WriteStatus: "state-closed"},
		},
		Retry: &Retry{Retries: 2, BackoffSeconds: 60},
		StageContext: &StageContext{
			Stage:        &Stage{Key: "backlog", Name: "Backlog", Role: RoleIntake},
			SourceStatus: strPtr("Backlog"),
			Card:         &Card{ID: "card_1", Handle: "builder"},
			Reason:       DispatchReasonDefault,
			Parent:       &ContextParent{Identifier: "PRJ-1", Title: "Parent work"},
			Children:     []ContextChild{{Identifier: "PRJ-2", Status: strPtr("Done")}, {Identifier: "PRJ-3"}},
		},
		DispatchReason: DispatchReasonMention,
	}
}

func TestFieldsDecodeAndRoundTrip(t *testing.T) {
	t.Parallel()
	var got Fields
	if err := json.Unmarshal([]byte(fullFieldsJSON), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if want := wantFullFields(); !reflect.DeepEqual(got, want) {
		t.Fatalf("decoded\n%#v\nwant\n%#v", got, want)
	}
	encoded, err := json.Marshal(got)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	// The re-encoded document equals the source document member for member.
	var source, again map[string]any
	if err := json.Unmarshal([]byte(fullFieldsJSON), &source); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(encoded, &again); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(source, again) {
		t.Fatalf("round trip changed the document:\n%s", encoded)
	}
}

// TestFieldsAbsentAddNothing pins the additive rule: a work item with no
// 1.0 field encodes to nothing.
func TestFieldsAbsentAddNothing(t *testing.T) {
	t.Parallel()
	encoded, err := json.Marshal(Fields{})
	if err != nil || string(encoded) != "{}" {
		t.Fatalf("empty fields encoded %s, %v", encoded, err)
	}
}

func TestEachFieldRoundTrips(t *testing.T) {
	t.Parallel()
	full := wantFullFields()
	for _, tc := range []struct {
		name   string
		fields Fields
		key    string
	}{
		{"stage", Fields{Stage: full.Stage}, "stage"},
		{"card", Fields{Card: full.Card}, "card"},
		{"transitions", Fields{Transitions: full.Transitions}, "transitions"},
		{"retry", Fields{Retry: full.Retry}, "retry"},
		{"stageContext", Fields{StageContext: full.StageContext}, "stageContext"},
		{"dispatchReason", Fields{DispatchReason: full.DispatchReason}, "dispatchReason"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			encoded, err := json.Marshal(tc.fields)
			if err != nil {
				t.Fatal(err)
			}
			var members map[string]json.RawMessage
			if err := json.Unmarshal(encoded, &members); err != nil {
				t.Fatal(err)
			}
			if len(members) != 1 || members[tc.key] == nil {
				t.Fatalf("encoded %s; want exactly member %q", encoded, tc.key)
			}
			var back Fields
			if err := json.Unmarshal(encoded, &back); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(back, tc.fields) {
				t.Fatalf("round trip %#v; want %#v", back, tc.fields)
			}
		})
	}
}

func TestRoleVocabularyAndUnknownRolesDropped(t *testing.T) {
	t.Parallel()
	for _, role := range Roles {
		if got, ok := ParseRole(string(role)); !ok || got != role {
			t.Fatalf("ParseRole(%q) = %q, %v", role, got, ok)
		}
	}
	if len(Roles) != 7 {
		t.Fatalf("role vocabulary has %d entries; want 7", len(Roles))
	}
	for _, raw := range []string{"", "terminal", "INFLIGHT", "future", " review"} {
		var s Stage
		if err := json.Unmarshal([]byte(`{"key":"k","role":`+quote(raw)+`,"runRole":`+quote(raw)+`}`), &s); err != nil {
			t.Fatal(err)
		}
		if s.Role != "" || s.RunRole != "" || s.Key != "k" {
			t.Fatalf("role %q decoded as %+v; want dropped role, kept key", raw, s)
		}
	}
}

func TestEffectiveRunRolePrecedence(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name  string
		stage *Stage
		want  Role
		ok    bool
	}{
		{"run role wins", &Stage{Role: RoleIntake, RunRole: RoleInflight}, RoleInflight, true},
		{"role when run role absent", &Stage{Role: RoleReview}, RoleReview, true},
		{"run role alone", &Stage{RunRole: RoleRework}, RoleRework, true},
		{"no role", &Stage{Key: "k"}, "", false},
		{"no stage", nil, "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, ok := Fields{Stage: tc.stage}.StageRunRole()
			if got != tc.want || ok != tc.ok {
				t.Fatalf("StageRunRole = %q, %v; want %q, %v", got, ok, tc.want, tc.ok)
			}
		})
	}
}

func TestRoleDefaultContracts(t *testing.T) {
	t.Parallel()
	want := map[Role]Contract{
		RoleInflight: {PR: true, WorkResult: true},
		RoleRework:   {PR: true, WorkResult: true},
		RoleReview:   {Verdict: true, WorkResult: true},
		RoleIntake:   {WorkResult: true},
		RoleHold:     {WorkResult: true},
		RoleDone:     {},
		RoleClosed:   {},
	}
	if len(want) != len(Roles) {
		t.Fatal("default-contract table does not cover every role")
	}
	for _, role := range Roles {
		if got := role.DefaultContract(); got != want[role] {
			t.Fatalf("%s default contract = %+v; want %+v", role, got, want[role])
		}
	}
	if got := Role("future").DefaultContract(); got != (Contract{}) {
		t.Fatalf("unknown role owes %+v; want nothing", got)
	}
}

func TestTransitionDecode(t *testing.T) {
	t.Parallel()
	long := strings.Repeat("x", MaxIDBytes+1)
	for _, tc := range []struct {
		name string
		raw  string
		want Transition
	}{
		{"stay", `"stay"`, Transition{}},
		{"target", `{"stageKey":"review","writeStatus":"s1"}`, Transition{StageKey: "review", WriteStatus: "s1"}},
		{"unknown string is stay", `"advance"`, Transition{}},
		{"missing write status is stay", `{"stageKey":"review"}`, Transition{}},
		{"missing stage key is stay", `{"writeStatus":"s1"}`, Transition{}},
		{"oversized write status is stay", `{"stageKey":"review","writeStatus":"` + long + `"}`, Transition{}},
		{"control character is stay", `{"stageKey":"review","writeStatus":"s\u00011"}`, Transition{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var got Transition
			if err := json.Unmarshal([]byte(tc.raw), &got); err != nil {
				t.Fatal(err)
			}
			if got != tc.want {
				t.Fatalf("decoded %+v; want %+v", got, tc.want)
			}
		})
	}
	var bad Transition
	if err := json.Unmarshal([]byte(`42`), &bad); err == nil {
		t.Fatal("a number decoded as a transition; want a decode error")
	}
	if encoded, _ := json.Marshal(Transition{}); string(encoded) != `"stay"` {
		t.Fatalf("stay encoded %s", encoded)
	}
}

func TestTransitionsFor(t *testing.T) {
	t.Parallel()
	tr := wantFullFields().Transitions
	for outcome, want := range map[Outcome]Transition{
		OutcomeStart:   {StageKey: "in-progress", WriteStatus: "state-started"},
		OutcomeSuccess: {StageKey: "review", WriteStatus: "state-review"},
		OutcomeFailure: {},
		OutcomeBlocked: {StageKey: "icebox", WriteStatus: "state-icebox"},
		OutcomeClose:   {StageKey: "closed", WriteStatus: "state-closed"},
		Outcome("x"):   {},
	} {
		if got := tr.For(outcome); got != want {
			t.Fatalf("For(%s) = %+v; want %+v", outcome, got, want)
		}
	}
	var none *Transitions
	if !none.For(OutcomeSuccess).Stay() || !(&Transitions{}).For(OutcomeStart).Stay() {
		t.Fatal("absent transitions must stay")
	}
}

func TestRetryClamped(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		raw  string
		want Retry
	}{
		{`{"retries":2,"backoffSeconds":60}`, Retry{2, 60}},
		{`{"retries":-1,"backoffSeconds":-5}`, Retry{0, 0}},
		{`{"retries":99,"backoffSeconds":999999}`, Retry{MaxRetries, MaxBackoffSeconds}},
		{`{}`, Retry{}},
	} {
		var got Retry
		if err := json.Unmarshal([]byte(tc.raw), &got); err != nil || got != tc.want {
			t.Fatalf("%s decoded %+v, %v; want %+v", tc.raw, got, err, tc.want)
		}
	}
}

func TestStringsBounded(t *testing.T) {
	t.Parallel()
	longID := strings.Repeat("a", MaxIDBytes+1)
	longText := strings.Repeat("b", MaxTextBytes+1)
	raw := `{
		"stage": {"id": "` + longID + `", "key": "k\u0007", "name": "` + longText + `", "role": "review"},
		"card": {"id": "card", "handle": "` + longID + `"},
		"dispatchReason": "because",
		"stageContext": {
			"sourceStatus": "` + longText + `",
			"reason": "nope",
			"parent": {"identifier": "` + longID + `", "title": "t"},
			"children": [` + strings.TrimSuffix(strings.Repeat(`{"identifier":"C-1","status":"s"},`, MaxChildren+5), ",") + `, {"identifier": ""}]
		}
	}`
	var got Fields
	if err := json.Unmarshal([]byte(raw), &got); err != nil {
		t.Fatal(err)
	}
	if *got.Stage != (Stage{Role: RoleReview}) {
		t.Fatalf("stage = %+v; want only the role kept", *got.Stage)
	}
	if got.Card.ID != "card" || got.Card.Handle != "" {
		t.Fatalf("card = %+v", *got.Card)
	}
	if got.DispatchReason != "" {
		t.Fatalf("unknown dispatch reason kept: %q", got.DispatchReason)
	}
	sc := got.StageContext
	if sc.SourceStatus != nil || sc.Reason != "" || sc.Parent != nil || len(sc.Children) != MaxChildren {
		t.Fatalf("stage context not bounded: %+v (children %d)", sc, len(sc.Children))
	}
	var invalid BatchKind
	if err := json.Unmarshal([]byte(`"`+longID+`"`), &invalid); err != nil || invalid != "" {
		t.Fatalf("oversized batch kind = %q, %v", invalid, err)
	}
	var kind BatchKind
	if err := json.Unmarshal([]byte(`"landing-run"`), &kind); err != nil || kind != "landing-run" {
		t.Fatalf("batch kind = %q, %v", kind, err)
	}
}

func TestCloneFieldsDoesNotAlias(t *testing.T) {
	t.Parallel()
	original := wantFullFields()
	clone := original.CloneFields()
	if !reflect.DeepEqual(clone, original) {
		t.Fatal("clone differs")
	}
	clone.Stage.Key = "moved"
	clone.Card.Contract.PR = false
	clone.Transitions.Success.WriteStatus = "moved"
	clone.Retry.Retries = 9
	*clone.StageContext.SourceStatus = "moved"
	*clone.StageContext.Children[0].Status = "moved"
	if !reflect.DeepEqual(original, wantFullFields()) {
		t.Fatal("mutating the clone moved the original")
	}
}

func quote(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}
