package linearcmd

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/spf13/cobra"
)

func TestLinearCommentCanonicalVisibility(t *testing.T) {
	for _, tc := range []struct {
		name string
		args []string
	}{
		{"help", []string{"--help"}},
		{"completion", []string{"__complete", ""}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out, err := runLinearCmd(t, "", tc.args...)
			if err != nil {
				t.Fatalf("command: %v, output=%q", err, out)
			}
			var canonical, compatibility int
			for _, line := range strings.Split(out, "\n") {
				fields := strings.Fields(line)
				if len(fields) == 0 {
					continue
				}
				switch fields[0] {
				case "create-comment":
					canonical++
				case "comment":
					compatibility++
				}
			}
			if canonical != 1 || compatibility != 0 {
				t.Fatalf("want one canonical operation, got create-comment=%d comment=%d: %s", canonical, compatibility, out)
			}
		})
	}
	for _, spelling := range []string{"create-comment", "comment"} {
		t.Run(spelling+"_help", func(t *testing.T) {
			out, err := runLinearCmd(t, "", spelling, "--help")
			if err != nil || !strings.Contains(out, "create-comment, comment") ||
				!strings.Contains(out, "comment spelling remains supported") {
				t.Fatalf("supported alias metadata missing: err=%v output=%q", err, out)
			}
		})
	}
}

func TestLinearCommentSpellingsTransport(t *testing.T) {
	const body = "Résumé 日本語\nsecond line\n"
	const created = "2026-09-30T10:00:00Z"
	for _, spelling := range []string{"create-comment", "comment"} {
		for _, tc := range []struct {
			name         string
			args         []string
			fileBody     string
			response     string
			status       int
			wantRequests int32
			wantError    bool
		}{
			{name: "inline", args: []string{"ENG-1", "--body", body}, wantRequests: 1},
			{name: "file", args: []string{"ENG-1", "--body-file", "FILE"}, fileBody: body, wantRequests: 1},
			{name: "file_precedence", args: []string{"ENG-1", "--body", "ignored", "--body-file", "FILE"}, fileBody: body, wantRequests: 1},
			{name: "missing_body", args: []string{"ENG-1"}, wantError: true},
			{name: "empty_inline", args: []string{"ENG-1", "--body", ""}, wantError: true},
			{name: "empty_file_precedence", args: []string{"ENG-1", "--body", "ignored", "--body-file", "FILE"}, wantError: true},
			{name: "unreadable_file", args: []string{"ENG-1", "--body", "ignored", "--body-file", "ABSENT"}, wantError: true},
			{name: "missing_issue", args: []string{"--body", body}, wantError: true},
			{name: "extra_issue", args: []string{"ENG-1", "ENG-2", "--body", body}, wantError: true},
			{name: "http_failure", args: []string{"ENG-1", "--body", body}, status: http.StatusForbidden, wantRequests: 1, wantError: true},
			{name: "graphql_failure", args: []string{"ENG-1", "--body", body}, response: `{"errors":[{"message":"fixture refusal"}]}`, wantRequests: 1, wantError: true},
			{name: "mutation_refusal", args: []string{"ENG-1", "--body", body}, response: `{"data":{"commentCreate":{"success":false}}}`, wantRequests: 1, wantError: true},
		} {
			t.Run(spelling+"/"+tc.name, func(t *testing.T) {
				file := filepath.Join(t.TempDir(), "body.md")
				if err := os.WriteFile(file, []byte(tc.fileBody), 0o600); err != nil {
					t.Fatal(err)
				}
				args := append([]string{spelling}, tc.args...)
				for i, arg := range args {
					if arg == "FILE" {
						args[i] = file
					} else if arg == "ABSENT" {
						args[i] = file + ".absent"
					}
				}
				var requests atomic.Int32
				setupLinearTest(t, func(w http.ResponseWriter, r *http.Request) {
					requests.Add(1)
					var request struct {
						Query     string            `json:"query"`
						Variables map[string]string `json:"variables"`
					}
					if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
						t.Errorf("decode: %v", err)
					}
					if r.Method != http.MethodPost || r.URL.Path != "/" ||
						r.Header.Get("Authorization") != "test-fixture-key-not-a-secret" ||
						!strings.Contains(request.Query, "mutation CreateComment(") ||
						request.Variables["issueId"] != "ENG-1" || request.Variables["body"] != body || len(request.Variables) != 2 {
						t.Errorf("transport changed: method=%s path=%s query=%q variables=%v", r.Method, r.URL.Path, request.Query, request.Variables)
					}
					if tc.status != 0 {
						w.WriteHeader(tc.status)
						return
					}
					if tc.response != "" {
						_, _ = fmt.Fprint(w, tc.response)
						return
					}
					writeLinearGQLData(w, fmt.Sprintf(`{"commentCreate":{"success":true,"comment":{"id":"c-fixture","body":%q,"createdAt":%q}}}`, body, created))
				})
				out, err := runLinearCmd(t, "", args...)
				if (err != nil) != tc.wantError || requests.Load() != tc.wantRequests {
					t.Fatalf("err=%v requests=%d want error=%t requests=%d output=%q", err, requests.Load(), tc.wantError, tc.wantRequests, out)
				}
				if tc.wantError {
					if strings.Contains(out, `"id"`) {
						t.Fatalf("failure emitted success JSON: %q", out)
					}
					return
				}
				var result map[string]string
				if err := json.Unmarshal([]byte(out), &result); err != nil || len(result) != 3 || result["id"] != "c-fixture" || result["body"] != body || result["createdAt"] != created {
					t.Fatalf("JSON contract changed: err=%v output=%q", err, out)
				}
			})
		}
	}
}

func TestLinearCommentSpellingsInheritedFlagsAndCancellation(t *testing.T) {
	for _, spelling := range []string{"create-comment", "comment"} {
		t.Run(spelling, func(t *testing.T) {
			started := make(chan struct{})
			finish := make(chan struct{})
			setupLinearTest(t, func(_ http.ResponseWriter, _ *http.Request) {
				close(started)
				<-finish
			})
			// Release the fixture before httptest cleanup, even on a failed assertion.
			t.Cleanup(func() { close(finish) })
			ctx, cancel := context.WithCancel(context.Background())
			t.Cleanup(cancel)
			var inherited string
			var preRuns atomic.Int32
			root := &cobra.Command{Use: "fixture", SilenceErrors: true, SilenceUsage: true}
			root.PersistentFlags().StringVar(&inherited, "fixture-scope", "", "fixture inherited flag")
			root.PersistentPreRunE = func(cmd *cobra.Command, _ []string) error {
				preRuns.Add(1)
				if inherited != "scope-fixture" || cmd.Context() != ctx {
					return fmt.Errorf("inherited flag or context lost")
				}
				return nil
			}
			root.AddCommand(New(nil, "fixture"))
			var output bytes.Buffer
			root.SetOut(&output)
			root.SetErr(&output)
			root.SetArgs([]string{"linear", spelling, "ENG-1", "--body", "cancel me", "--fixture-scope", "scope-fixture"})
			result := make(chan error, 1)
			go func() { result <- root.ExecuteContext(ctx) }()
			select {
			case <-started:
			case err := <-result:
				t.Fatalf("command ended before HTTP: %v", err)
			case <-time.After(3 * time.Second):
				t.Fatal("comment request never reached fixture")
			}
			cancel()
			select {
			case err := <-result:
				if !errors.Is(err, context.Canceled) || preRuns.Load() != 1 || strings.Contains(output.String(), `"id"`) {
					t.Fatalf("cancellation/pre-run changed: err=%v preRuns=%d output=%q", err, preRuns.Load(), output.String())
				}
			case <-time.After(3 * time.Second):
				t.Fatal("comment command ignored cancellation")
			}
		})
	}
}
