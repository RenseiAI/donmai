package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"
)

const (
	claimAttemptTokenOne = "11111111-1111-4111-8111-111111111111"
	claimAttemptTokenTwo = "22222222-2222-4222-8222-222222222222"
)

type claimAttemptHTTPFixture struct {
	t         *testing.T
	server    *httptest.Server
	mu        sync.Mutex
	polls     []string
	pollIndex int
	nacks     []map[string]json.RawMessage
}

func newClaimAttemptHTTPFixture(t *testing.T, polls ...string) *claimAttemptHTTPFixture {
	t.Helper()
	fixture := &claimAttemptHTTPFixture{t: t, polls: polls}
	fixture.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/poll"):
			fixture.mu.Lock()
			defer fixture.mu.Unlock()
			if fixture.pollIndex >= len(fixture.polls) {
				_, _ = io.WriteString(w, `{"work":[]}`)
				return
			}
			_, _ = io.WriteString(w, fixture.polls[fixture.pollIndex])
			fixture.pollIndex++
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/nack"):
			var body map[string]json.RawMessage
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Errorf("decode NACK body: %v", err)
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			fixture.mu.Lock()
			fixture.nacks = append(fixture.nacks, body)
			fixture.mu.Unlock()
			w.WriteHeader(http.StatusNoContent)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(fixture.server.Close)
	return fixture
}

func (f *claimAttemptHTTPFixture) nackBodies() []map[string]json.RawMessage {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]map[string]json.RawMessage(nil), f.nacks...)
}

func claimAttemptPollBody(sessionID, token string) string {
	return fmt.Sprintf(`{
		"work":[{
			"sessionId":%q,
			"organizationId":"org-test",
			"issueId":"issue-test",
			"issueIdentifier":"TEST-1",
			"priority":1,
			"queuedAt":1,
			"codeIntel":{"repo":"example/repo","tools":[]}
		}],
		"futureEnvelopeMember":{"ignored":true},
		"claimAttempts":{
			%q:{"contractVersion":"work-claim-attempt/v1","sessionId":%q,"attemptToken":%q}
		}
	}`, sessionID, sessionID, sessionID, token)
}

func tooManyClaimAttemptsBody() string {
	var proofs strings.Builder
	for index := 0; index <= maxPollClaimAttemptProofs; index++ {
		if index > 0 {
			proofs.WriteByte(',')
		}
		sessionID := fmt.Sprintf("proof-%d", index)
		_, _ = fmt.Fprintf(&proofs,
			`%q:{"contractVersion":"work-claim-attempt/v1","sessionId":%q,"attemptToken":%q}`,
			sessionID, sessionID, claimAttemptTokenOne)
	}
	return `{"work":[{"sessionId":"proof-0","issueId":"i","issueIdentifier":"T-1","priority":1,"queuedAt":1}],"claimAttempts":{` + proofs.String() + `}}`
}

func decodeClaimAttemptBody(t *testing.T, raw json.RawMessage) map[string]any {
	t.Helper()
	var value map[string]any
	if err := json.Unmarshal(raw, &value); err != nil {
		t.Fatalf("decode claimAttempt: %v", err)
	}
	return value
}

func TestPollClaimAttempt_ActualHTTPRoundTripOutsideWork(t *testing.T) {
	fixture := newClaimAttemptHTTPFixture(t, claimAttemptPollBody("session-proof", claimAttemptTokenOne))
	var marshaledItem []byte
	var detail *SessionDetail
	poller := NewPollService(PollOptions{
		WorkerID:        "worker-proof",
		RuntimeJWT:      "runtime-proof",
		OrchestratorURL: fixture.server.URL,
		HTTPClient:      fixture.server.Client(),
		OnWork: func(item PollWorkItem) error {
			var err error
			marshaledItem, err = json.Marshal(item)
			if err != nil {
				return err
			}
			detail = PollItemToSessionDetail(
				item, nil, fixture.server.URL, "runtime-proof", "worker-proof",
			)
			return NackRejectedWork(
				context.Background(), fixture.server.Client(), fixture.server.URL,
				"worker-proof", "runtime-proof", &item, errors.New("synthetic rejection"),
			)
		},
	})

	poller.pollOnce(context.Background())
	nacks := fixture.nackBodies()
	if len(nacks) != 1 {
		t.Fatalf("NACK count = %d, want 1", len(nacks))
	}
	proofRaw, ok := nacks[0]["claimAttempt"]
	if !ok {
		t.Fatalf("NACK body omitted claimAttempt: %s", mustJSON(nacks[0]))
	}
	proof := decodeClaimAttemptBody(t, proofRaw)
	if proof["contractVersion"] != "work-claim-attempt/v1" ||
		proof["sessionId"] != "session-proof" ||
		proof["attemptToken"] != claimAttemptTokenOne {
		t.Fatalf("claimAttempt = %#v", proof)
	}
	if _, ok := nacks[0]["work"]; !ok {
		t.Fatal("legacy NACK work member disappeared")
	}
	if strings.Contains(string(marshaledItem), "work-claim-attempt") ||
		strings.Contains(string(marshaledItem), claimAttemptTokenOne) ||
		strings.Contains(string(marshaledItem), "claimAttempt") {
		t.Fatalf("item leaked proof: %s", marshaledItem)
	}
	if detail == nil {
		t.Fatal("session detail was not built")
	}
	detailType := reflect.TypeOf(*detail)
	for index := 0; index < detailType.NumField(); index++ {
		field := detailType.Field(index)
		if strings.Contains(strings.ToLower(field.Name), "claimattempt") ||
			strings.Contains(field.Tag.Get("json"), "claimAttempt") {
			t.Fatalf("SessionDetail gained proof field %s", field.Name)
		}
	}
	if strings.Contains(string(detail.OperationalPayload), claimAttemptTokenOne) ||
		strings.Contains(string(detail.OperationalPayload), "claimAttempt") {
		t.Fatalf("detail operational payload leaked proof: %s", detail.OperationalPayload)
	}
	var work map[string]json.RawMessage
	if err := json.Unmarshal(nacks[0]["work"], &work); err != nil {
		t.Fatal(err)
	}
	if _, ok := work["claimAttempt"]; ok {
		t.Fatalf("claimAttempt entered work: %s", nacks[0]["work"])
	}
	var operational map[string]json.RawMessage
	if err := json.Unmarshal(work["operationalPayload"], &operational); err != nil {
		t.Fatal(err)
	}
	if _, ok := operational["claimAttempt"]; ok {
		t.Fatalf("claimAttempt entered operational payload: %s", work["operationalPayload"])
	}
	if _, ok := operational["codeIntel"]; !ok {
		t.Fatalf("operational payload changed while binding proof: %s", work["operationalPayload"])
	}
}

func TestPollClaimAttempt_MissingEnvelopePreservesLegacyExecutionAndNACK(t *testing.T) {
	fixture := newClaimAttemptHTTPFixture(t, `{"work":[{"sessionId":"legacy","issueId":"i","issueIdentifier":"T-1","priority":1,"queuedAt":1}]}`)
	var workCalls int
	poller := NewPollService(PollOptions{
		WorkerID: "worker-legacy", RuntimeJWT: "runtime-legacy",
		OrchestratorURL: fixture.server.URL, HTTPClient: fixture.server.Client(),
		OnWork: func(item PollWorkItem) error {
			workCalls++
			return NackRejectedWork(context.Background(), fixture.server.Client(), fixture.server.URL,
				"worker-legacy", "runtime-legacy", &item, errors.New("legacy rejection"))
		},
	})
	poller.pollOnce(context.Background())
	if workCalls != 1 {
		t.Fatalf("work calls = %d, want 1", workCalls)
	}
	nacks := fixture.nackBodies()
	if len(nacks) != 1 {
		t.Fatalf("NACK count = %d, want 1", len(nacks))
	}
	if _, ok := nacks[0]["claimAttempt"]; ok {
		t.Fatalf("legacy NACK gained claimAttempt: %s", mustJSON(nacks[0]))
	}
}

func TestPollClaimAttempt_InvalidEnvelopeNeverBindsOrBlocksWork(t *testing.T) {
	validProof := `{"contractVersion":"work-claim-attempt/v1","sessionId":"s","attemptToken":"` + claimAttemptTokenOne + `"}`
	tests := []struct {
		name string
		body string
	}{
		{"null map", `{"work":[{"sessionId":"s","issueId":"i","issueIdentifier":"T-1","priority":1,"queuedAt":1}],"claimAttempts":null}`},
		{"wrong map type", `{"work":[{"sessionId":"s","issueId":"i","issueIdentifier":"T-1","priority":1,"queuedAt":1}],"claimAttempts":[]}`},
		{"unknown version", strings.Replace(claimAttemptPollBody("s", claimAttemptTokenOne), "work-claim-attempt/v1", "work-claim-attempt/v2", 1)},
		{"unknown member", strings.Replace(claimAttemptPollBody("s", claimAttemptTokenOne), `"attemptToken":"`+claimAttemptTokenOne+`"`, `"attemptToken":"`+claimAttemptTokenOne+`","extra":true`, 1)},
		{"empty token", claimAttemptPollBody("s", "")},
		{"invalid token", claimAttemptPollBody("s", "11111111-1111-1111-8111-111111111111")},
		{"oversized token", claimAttemptPollBody("s", strings.Repeat("a", 300))},
		{"key mismatch", strings.Replace(claimAttemptPollBody("s", claimAttemptTokenOne), `"s":{"contractVersion"`, `"other":{"contractVersion"`, 1)},
		{"proof session mismatch", strings.Replace(claimAttemptPollBody("s", claimAttemptTokenOne), `"sessionId":"s","attemptToken"`, `"sessionId":"other","attemptToken"`, 1)},
		{"orphan proof", `{"work":[{"sessionId":"s","issueId":"i","issueIdentifier":"T-1","priority":1,"queuedAt":1}],"claimAttempts":{"orphan":` + strings.Replace(validProof, `"sessionId":"s"`, `"sessionId":"orphan"`, 1) + `}}`},
		{"duplicate proof key", `{"work":[{"sessionId":"s","issueId":"i","issueIdentifier":"T-1","priority":1,"queuedAt":1}],"claimAttempts":{"s":` + validProof + `,"s":` + validProof + `}}`},
		{"duplicate proof member", `{"work":[{"sessionId":"s","issueId":"i","issueIdentifier":"T-1","priority":1,"queuedAt":1}],"claimAttempts":{"s":{"contractVersion":"work-claim-attempt/v1","sessionId":"s","attemptToken":"` + claimAttemptTokenOne + `","attemptToken":"` + claimAttemptTokenTwo + `"}}}`},
		{"duplicate envelope member", `{"work":[{"sessionId":"s","issueId":"i","issueIdentifier":"T-1","priority":1,"queuedAt":1}],"claimAttempts":{"s":` + validProof + `},"claimAttempts":{"s":` + validProof + `}}`},
		{"duplicate work identity", `{"work":[{"sessionId":"s","issueId":"i","issueIdentifier":"T-1","priority":1,"queuedAt":1},{"sessionId":"s","issueId":"i2","issueIdentifier":"T-2","priority":1,"queuedAt":2}],"claimAttempts":{"s":` + validProof + `}}`},
		{"too many proofs", tooManyClaimAttemptsBody()},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fixture := newClaimAttemptHTTPFixture(t, tt.body)
			var workCalls int
			var logText strings.Builder
			poller := NewPollService(PollOptions{
				WorkerID: "worker-invalid", RuntimeJWT: "runtime-invalid",
				OrchestratorURL: fixture.server.URL, HTTPClient: fixture.server.Client(),
				LogWarn: func(format string, args ...any) {
					_, _ = fmt.Fprintf(&logText, format, args...)
				},
				OnWork: func(item PollWorkItem) error {
					workCalls++
					return NackRejectedWork(context.Background(), fixture.server.Client(), fixture.server.URL,
						"worker-invalid", "runtime-invalid", &item, errors.New("invalid-proof rejection"))
				},
			})
			poller.pollOnce(context.Background())
			if workCalls == 0 {
				t.Fatal("invalid proof blocked ordinary work execution")
			}
			for _, body := range fixture.nackBodies() {
				if _, ok := body["claimAttempt"]; ok {
					t.Fatalf("invalid proof was echoed: %s", mustJSON(body))
				}
			}
			if strings.Contains(logText.String(), claimAttemptTokenOne) ||
				strings.Contains(logText.String(), claimAttemptTokenTwo) {
				t.Fatalf("log leaked proof token: %s", logText.String())
			}
		})
	}
}

func TestPollClaimAttempt_AmbiguousTopLevelWorkNeverBindsProof(t *testing.T) {
	proof := `{"s":{"contractVersion":"work-claim-attempt/v1","sessionId":"s","attemptToken":"` + claimAttemptTokenOne + `"}}`
	work := `[{"sessionId":"s","issueId":"i","issueIdentifier":"T-1","priority":1,"queuedAt":1}]`
	tests := []struct {
		name string
		body string
	}{
		{
			"duplicate canonical work member",
			`{"work":` + work + `,"work":` + work + `,"claimAttempts":` + proof + `}`,
		},
		{
			"canonical plus casefold alias",
			`{"work":` + work + `,"Work":` + work + `,"claimAttempts":` + proof + `}`,
		},
		{
			"casefold alias without canonical member",
			`{"Work":` + work + `,"claimAttempts":` + proof + `}`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fixture := newClaimAttemptHTTPFixture(t, tt.body)
			var workCalls int
			poller := NewPollService(PollOptions{
				WorkerID: "worker-ambiguous", RuntimeJWT: "runtime-ambiguous",
				OrchestratorURL: fixture.server.URL, HTTPClient: fixture.server.Client(),
				OnWork: func(item PollWorkItem) error {
					workCalls++
					return NackRejectedWork(context.Background(), fixture.server.Client(), fixture.server.URL,
						"worker-ambiguous", "runtime-ambiguous", &item, errors.New("ambiguous-work rejection"))
				},
			})
			poller.pollOnce(context.Background())
			if workCalls != 1 {
				t.Fatalf("ordinary work calls = %d, want 1", workCalls)
			}
			nacks := fixture.nackBodies()
			if len(nacks) != 1 {
				t.Fatalf("NACK count = %d, want 1", len(nacks))
			}
			if _, ok := nacks[0]["claimAttempt"]; ok {
				t.Fatalf("ambiguous work member gained trusted proof: %s", mustJSON(nacks[0]))
			}
		})
	}
}

func TestPollClaimAttempt_AmbiguousWorkWithoutProofPreservesLegacyDecode(t *testing.T) {
	first := `[{"sessionId":"first","issueId":"i1","issueIdentifier":"T-1","priority":1,"queuedAt":1}]`
	last := `[{"sessionId":"last","issueId":"i2","issueIdentifier":"T-2","priority":1,"queuedAt":2}]`
	fixture := newClaimAttemptHTTPFixture(t, `{"work":`+first+`,"Work":`+last+`}`)
	var gotSession string
	poller := NewPollService(PollOptions{
		WorkerID: "worker-legacy-alias", RuntimeJWT: "runtime-legacy-alias",
		OrchestratorURL: fixture.server.URL, HTTPClient: fixture.server.Client(),
		OnWork: func(item PollWorkItem) error {
			gotSession = item.SessionID
			return nil
		},
	})
	poller.pollOnce(context.Background())
	if gotSession != "last" {
		t.Fatalf("legacy casefold decode session = %q, want last", gotSession)
	}
}

func TestPollClaimAttempt_SuccessiveSameSessionProofsDoNotAlias(t *testing.T) {
	fixture := newClaimAttemptHTTPFixture(t,
		claimAttemptPollBody("same-session", claimAttemptTokenOne),
		claimAttemptPollBody("same-session", claimAttemptTokenTwo),
	)
	var items []PollWorkItem
	poller := NewPollService(PollOptions{
		WorkerID: "worker-race", RuntimeJWT: "runtime-race",
		OrchestratorURL: fixture.server.URL, HTTPClient: fixture.server.Client(),
		OnWork: func(item PollWorkItem) error {
			items = append(items, item)
			return nil
		},
	})
	poller.pollOnce(context.Background())
	poller.pollOnce(context.Background())
	if len(items) != 2 {
		t.Fatalf("items = %d, want 2", len(items))
	}
	for index := range items {
		if err := NackRejectedWork(context.Background(), fixture.server.Client(), fixture.server.URL,
			"worker-race", "runtime-race", &items[index], errors.New("delayed rejection")); err != nil {
			t.Fatal(err)
		}
	}
	nacks := fixture.nackBodies()
	if len(nacks) != 2 {
		t.Fatalf("NACK count = %d, want 2", len(nacks))
	}
	got := []any{
		decodeClaimAttemptBody(t, nacks[0]["claimAttempt"])["attemptToken"],
		decodeClaimAttemptBody(t, nacks[1]["claimAttempt"])["attemptToken"],
	}
	want := []any{claimAttemptTokenOne, claimAttemptTokenTwo}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("successive tokens = %v, want %v", got, want)
	}
}

func TestPollClaimAttempt_MultipleSessionsBindByIdentity(t *testing.T) {
	body := `{
		"work":[
			{"sessionId":"session-a","issueId":"a","issueIdentifier":"T-A","priority":1,"queuedAt":1},
			{"sessionId":"session-b","issueId":"b","issueIdentifier":"T-B","priority":1,"queuedAt":2},
			{"sessionId":"session-legacy","issueId":"legacy","issueIdentifier":"T-L","priority":1,"queuedAt":3}
		],
		"claimAttempts":{
			"session-b":{"contractVersion":"work-claim-attempt/v1","sessionId":"session-b","attemptToken":"` + claimAttemptTokenTwo + `"},
			"session-a":{"contractVersion":"work-claim-attempt/v1","sessionId":"session-a","attemptToken":"` + claimAttemptTokenOne + `"}
		}
	}`
	fixture := newClaimAttemptHTTPFixture(t, body)
	poller := NewPollService(PollOptions{
		WorkerID: "worker-multiple", RuntimeJWT: "runtime-multiple",
		OrchestratorURL: fixture.server.URL, HTTPClient: fixture.server.Client(),
		OnWork: func(item PollWorkItem) error {
			return NackRejectedWork(context.Background(), fixture.server.Client(), fixture.server.URL,
				"worker-multiple", "runtime-multiple", &item, errors.New("multiple rejection"))
		},
	})
	poller.pollOnce(context.Background())
	nacks := fixture.nackBodies()
	if len(nacks) != 3 {
		t.Fatalf("NACK count = %d, want 3", len(nacks))
	}
	got := make(map[string]string)
	for _, nack := range nacks {
		var work struct {
			SessionID string `json:"sessionId"`
		}
		if err := json.Unmarshal(nack["work"], &work); err != nil {
			t.Fatal(err)
		}
		if proofRaw, ok := nack["claimAttempt"]; ok {
			proof := decodeClaimAttemptBody(t, proofRaw)
			got[work.SessionID], _ = proof["attemptToken"].(string)
		} else {
			got[work.SessionID] = ""
		}
	}
	want := map[string]string{
		"session-a":      claimAttemptTokenOne,
		"session-b":      claimAttemptTokenTwo,
		"session-legacy": "",
	}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("proof bindings = %v, want %v", got, want)
	}
}

func TestPollClaimAttempt_ConcurrentPollersDoNotAlias(t *testing.T) {
	tests := []struct {
		name      string
		sessionID string
		workerID  string
		token     string
		fixture   *claimAttemptHTTPFixture
		poller    *PollService
	}{
		{name: "one", sessionID: "concurrent-one", workerID: "worker-one", token: claimAttemptTokenOne},
		{name: "two", sessionID: "concurrent-two", workerID: "worker-two", token: claimAttemptTokenTwo},
	}
	for index := range tests {
		tc := &tests[index]
		tc.fixture = newClaimAttemptHTTPFixture(t, claimAttemptPollBody(tc.sessionID, tc.token))
		tc.poller = NewPollService(PollOptions{
			WorkerID: tc.workerID, RuntimeJWT: "runtime-" + tc.name,
			OrchestratorURL: tc.fixture.server.URL, HTTPClient: tc.fixture.server.Client(),
			OnWork: func(item PollWorkItem) error {
				return NackRejectedWork(context.Background(), tc.fixture.server.Client(), tc.fixture.server.URL,
					tc.workerID, "runtime-"+tc.name, &item, errors.New("concurrent rejection"))
			},
		})
	}
	var wait sync.WaitGroup
	for index := range tests {
		poller := tests[index].poller
		wait.Add(1)
		go func() {
			defer wait.Done()
			poller.pollOnce(context.Background())
		}()
	}
	wait.Wait()
	for _, tc := range tests {
		nacks := tc.fixture.nackBodies()
		if len(nacks) != 1 {
			t.Errorf("%s NACK count = %d, want 1", tc.name, len(nacks))
			continue
		}
		proof := decodeClaimAttemptBody(t, nacks[0]["claimAttempt"])
		if proof["sessionId"] != tc.sessionID || proof["attemptToken"] != tc.token {
			t.Errorf("%s proof = %#v", tc.name, proof)
		}
	}
}

func mustJSON(value any) string {
	raw, _ := json.Marshal(value)
	return string(raw)
}
