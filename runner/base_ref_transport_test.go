package runner

import (
	"encoding/json"
	"testing"
)

func TestBaseRefRequiresVersionedLocalTransport(t *testing.T) {
	t.Parallel()
	for _, mode := range []RuntimeTransportMode{RuntimeTransportController, RuntimeTransportLocal, RuntimeTransportMode("local/v2")} {
		t.Run(string(mode), func(t *testing.T) {
			t.Parallel()
			registry, err := NewRegistryWithOptions(RegistryOptions{RuntimeTransportMode: mode})
			if err != nil {
				t.Fatal(err)
			}
			var work QueuedWork
			if err = json.Unmarshal([]byte(`{"repository":"https://github.com/example/project.git","baseRef":"release/next","branch":"work/session","workType":"development"}`), &work); err != nil {
				t.Fatal(err)
			}
			err = registry.validateRuntimeTransport(work)
			if mode == RuntimeTransportMode("local/v2") {
				if err != nil {
					t.Fatal(err)
				}
			} else if err == nil {
				t.Fatal("unversioned transport admitted new base-branch semantics")
			}
		})
	}
}

func TestBaseRefRawInvalidPresenceRefuses(t *testing.T) {
	t.Parallel()
	for _, raw := range []string{`{"baseRef":null}`, `{"baseRef":4}`, `{"baseRef":""}`} {
		t.Run(raw, func(t *testing.T) {
			t.Parallel()
			work := QueuedWork{OperationalPayload: json.RawMessage(raw)}
			if err := NewRegistry().validateRuntimeTransport(work); err == nil {
				t.Fatal("malformed base branch was treated as absent")
			}
		})
	}
}
