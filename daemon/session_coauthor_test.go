package daemon

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/RenseiAI/donmai/runner"
)

func TestPollServiceCoAuthorContract(t *testing.T) {
	email := "contributor" + "@" + "example.invalid"
	cases := []struct {
		name, author, email string
		valid               bool
	}{
		{"valid", "Sample Contributor", email, true},
		{"preserve name whitespace", " Sample Contributor ", email, true},
		{"absent", "", "", true},
		{"name only", "Sample Contributor", "", false},
		{"email only", "", email, false},
		{"no at", "Sample Contributor", "address-without-separator", false},
		{"multiple at", "Sample Contributor", "a@@b", false},
		{"empty local part", "Sample Contributor", "@domain", false},
		{"empty domain", "Sample Contributor", "local@", false},
		{"email space", "Sample Contributor", "a b@domain", false},
		{"email list", "Sample Contributor", email + ",other", false},
		{"email comment", "Sample Contributor", email + "(comment)", false},
		{"email tab", "Sample Contributor", "a\tb@domain", false},
		{"email unicode space", "Sample Contributor", "a\u00a0b@domain", false},
	}
	for _, forbidden := range []string{"\r", "\n", "\x00", "<", ">", "Co-Authored-By:", "cO-aUtHoReD-bY:"} {
		cases = append(cases,
			struct {
				name, author, email string
				valid               bool
			}{"name forbidden " + forbidden, "Sample" + forbidden, email, false},
			struct {
				name, author, email string
				valid               bool
			}{"email forbidden " + forbidden, "Sample Contributor", forbidden + email, false},
		)
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var logs bytes.Buffer
			previous := slog.Default()
			slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
			t.Cleanup(func() { slog.SetDefault(previous) })
			// Raw wire keys exercise the actual poll decoder rather than a mirror
			// that could agree with an incorrect field tag.
			wire := map[string]any{"sessionId": "credit-session"}
			if tc.author != "" {
				wire["coAuthorName"] = tc.author
			}
			if tc.email != "" {
				wire["coAuthorEmail"] = tc.email
			}
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				_ = json.NewEncoder(w).Encode(map[string]any{"work": []any{wire}, "preClaimed": true})
			}))
			t.Cleanup(srv.Close)
			called := false
			p := NewPollService(PollOptions{WorkerID: "fixture-worker", RuntimeJWT: "fixture-token", OrchestratorURL: srv.URL, OnWork: func(item PollWorkItem) error {
				called = true
				spec := PollItemToSessionSpec(item, nil)
				detail := PollItemToSessionDetail(item, nil, srv.URL, "fixture-token", "fixture-worker")
				wantName, wantEmail := "", ""
				if tc.valid {
					wantName, wantEmail = tc.author, tc.email
				}
				if spec.CoAuthorName != wantName || spec.CoAuthorEmail != wantEmail {
					t.Error("session spec did not preserve or drop the pair as expected")
				}
				if detail.CoAuthorName != wantName || detail.CoAuthorEmail != wantEmail {
					t.Error("session detail did not preserve or drop the pair as expected")
				}
				return nil
			}})
			p.pollOnce(t.Context())
			if !called {
				t.Fatal("poll did not dispatch work")
			}
			if strings.Contains(logs.String(), "dropping invalid commit co-author pair") != !tc.valid {
				t.Error("invalid pair warning did not match validity")
			}
			if tc.email != "" && strings.Contains(logs.String(), tc.email) || tc.author != "" && strings.Contains(logs.String(), tc.author) {
				t.Error("warning leaked a co-author value")
			}
		})
	}
}

func TestCoAuthorWireOmission(t *testing.T) {
	for _, value := range []any{PollWorkItem{}, SessionSpec{}, SessionDetail{}, runner.QueuedWork{}} {
		raw, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(raw), "coAuthorName") || strings.Contains(string(raw), "coAuthorEmail") {
			t.Errorf("empty co-author fields not omitted for %T", value)
		}
	}
}
