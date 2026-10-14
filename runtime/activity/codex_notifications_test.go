package activity_test

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/RenseiAI/donmai/agent"
	"github.com/RenseiAI/donmai/runtime/activity"
)

func TestCodexNotificationsPostTypedActivities(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ subtype, kind string }{
		{"codex_model_rerouted", "warning"},
		{"codex_moderation", "warning"},
		{"codex_safety_buffering", "warning"},
		{"codex_progress", "progress"},
	} {
		t.Run(tc.subtype, func(t *testing.T) {
			t.Parallel()
			received := make(chan struct{ Kind, Content string }, 1)
			srv := newServer(t, func(w http.ResponseWriter, r *http.Request) {
				if strings.HasSuffix(r.URL.Path, "/activity") {
					var body struct {
						Activity struct {
							Type    string `json:"type"`
							Content string `json:"content"`
						} `json:"activity"`
					}
					if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
						t.Error(err)
					}
					received <- struct{ Kind, Content string }{body.Activity.Type, body.Activity.Content}
				}
				w.WriteHeader(http.StatusOK)
			})
			poster, err := activity.New(activity.Config{SessionID: "s", WorkerID: "w", BaseURL: srv.URL, HTTPClient: srv.Client()})
			if err != nil {
				t.Fatal(err)
			}
			if err := poster.Start(context.Background()); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = poster.Stop() })
			poster.Send(context.Background(), agent.SystemEvent{Subtype: tc.subtype, Message: "native observation"})
			select {
			case got := <-received:
				if got.Kind != tc.kind || got.Content != "native observation" {
					t.Fatalf("activity = %+v", got)
				}
			case <-time.After(2 * time.Second):
				t.Fatal("notification dropped before activity POST")
			}
		})
	}
}
