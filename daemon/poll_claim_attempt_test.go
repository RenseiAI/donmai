package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
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
		"claimAttempts":{
			%q:{"contractVersion":"work-claim-attempt/v1","sessionId":%q,"attemptToken":%q}
		}
	}`, sessionID, sessionID, sessionID, token)
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
	var detailJSON []byte
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
			detailJSON, err = json.Marshal(PollItemToSessionDetail(
				item, nil, fixture.server.URL, "runtime-proof", "worker-proof",
			))
			if err != nil {
				return err
			}
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
	for name, raw := range map[string][]byte{"item": marshaledItem, "detail": detailJSON} {
		if strings.Contains(string(raw), "work-claim-attempt") ||
			strings.Contains(string(raw), claimAttemptTokenOne) ||
			strings.Contains(string(raw), "claimAttempt") {
			t.Fatalf("%s leaked proof: %s", name, raw)
		}
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

func mustJSON(value any) string {
	raw, _ := json.Marshal(value)
	return string(raw)
}
