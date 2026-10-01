package daemon

import (
	"encoding/json"
	"testing"
)

func TestPollDetailPreservesBaseRefMirror(t *testing.T) {
	t.Parallel()
	var item PollWorkItem
	if err := json.Unmarshal([]byte(`{"sessionId":"base-session","repository":"https://github.com/example/project.git","baseRef":"release/next","branch":"work/session","workType":"development"}`), &item); err != nil {
		t.Fatal(err)
	}
	detail := PollItemToSessionDetail(item, nil, "http://127.0.0.1:18888", "", "local-worker")
	if detail.BaseRef != "release/next" {
		t.Fatal("poll-to-worker detail dropped the configured base branch")
	}
	if detail.Ref != "" {
		t.Fatal("base branch was projected as amend-existing Ref")
	}
}
