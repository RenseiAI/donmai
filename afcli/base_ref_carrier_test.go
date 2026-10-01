package afcli

import (
	"encoding/json"
	"testing"

	"github.com/RenseiAI/donmai/daemon"
)

func TestWorkerDetailPreservesBaseRefMirror(t *testing.T) {
	t.Parallel()
	var detail daemon.SessionDetail
	if err := json.Unmarshal([]byte(`{"sessionId":"base-session","repository":"https://github.com/example/project.git","baseRef":"release/next","branch":"work/session","workType":"development"}`), &detail); err != nil {
		t.Fatal(err)
	}
	work, err := detailToQueuedWork(&detail)
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(work)
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]json.RawMessage
	if err = json.Unmarshal(encoded, &fields); err != nil {
		t.Fatal(err)
	}
	if string(fields["baseRef"]) != `"release/next"` {
		t.Fatal("worker reconstruction dropped the configured base branch")
	}
}

func TestWorkerDetailRefusesConflictingBaseRefMirror(t *testing.T) {
	t.Parallel()
	var detail daemon.SessionDetail
	if err := json.Unmarshal([]byte(`{"sessionId":"base-session","repository":"https://github.com/example/project.git","baseRef":"release/next","branch":"work/session","workType":"development","operationalPayload":{"sessionId":"base-session","repository":"https://github.com/example/project.git","baseRef":"different-base","branch":"work/session","workType":"development"}}`), &detail); err != nil {
		t.Fatal(err)
	}
	if _, err := detailToQueuedWork(&detail); err == nil {
		t.Fatal("worker accepted conflicting base-branch authority")
	}
}
