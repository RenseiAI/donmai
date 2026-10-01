package runner

import (
	"encoding/json"
	"testing"
)

func TestBaseRefIsPreservedByOperationalProjection(t *testing.T) {
	t.Parallel()
	raw := []byte(`{"sessionId":"base-session","repository":"https://github.com/example/project.git","baseRef":"release/next","branch":"work/session","workType":"development"}`)
	var work QueuedWork
	if err := json.Unmarshal(raw, &work); err != nil {
		t.Fatal(err)
	}
	canonical, err := CanonicalOperationalPayload(work)
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]json.RawMessage
	if err = json.Unmarshal(canonical, &fields); err != nil {
		t.Fatal(err)
	}
	if string(fields["baseRef"]) != `"release/next"` {
		t.Fatal("operational projection dropped the configured base branch")
	}
	if _, exists := fields["ref"]; exists {
		t.Fatal("new-branch base became amend-existing Ref")
	}
}
