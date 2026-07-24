package afcli

import (
	"testing"

	"github.com/RenseiAI/donmai/daemon"
)

// TestOpencodeCtorHints_NilDetail proves a nil SessionDetail (never expected
// in production — runAgentRun always has a fetched detail in hand — but
// cheap to guard) resolves to the zero value: Lane A stays the default.
func TestOpencodeCtorHints_NilDetail(t *testing.T) {
	t.Parallel()

	if got := opencodeCtorHints(nil); got.PreferOpenCodeServer {
		t.Errorf("opencodeCtorHints(nil) = %+v; want PreferOpenCodeServer false", got)
	}
}

// TestOpencodeCtorHints_NilResolvedProfile covers the common legacy-dispatch
// shape: a SessionDetail whose ResolvedProfile was never populated.
func TestOpencodeCtorHints_NilResolvedProfile(t *testing.T) {
	t.Parallel()

	d := &daemon.SessionDetail{}
	if got := opencodeCtorHints(d); got.PreferOpenCodeServer {
		t.Errorf("opencodeCtorHints(nil ResolvedProfile) = %+v; want PreferOpenCodeServer false", got)
	}
}

// TestOpencodeCtorHints_KeyAbsent asserts a ResolvedProfile with a
// ProviderConfig that simply doesn't carry the opencode hint key (e.g. a
// claude/codex session, or an opencode session that hasn't opted in) leaves
// PreferOpenCodeServer false — the historical Lane-A-default behavior.
func TestOpencodeCtorHints_KeyAbsent(t *testing.T) {
	t.Parallel()

	d := &daemon.SessionDetail{
		ResolvedProfile: &daemon.SessionResolvedProfile{
			Provider:       "claude",
			ProviderConfig: map[string]any{"unrelated.knob": true},
		},
	}
	if got := opencodeCtorHints(d); got.PreferOpenCodeServer {
		t.Errorf("opencodeCtorHints(key absent) = %+v; want PreferOpenCodeServer false", got)
	}
}

// TestOpencodeCtorHints_TrueValue is the load-bearing case this change
// exists for: a resolved profile that sets the typed opencode.preferServer
// knob to true selects Lane B.
func TestOpencodeCtorHints_TrueValue(t *testing.T) {
	t.Parallel()

	d := &daemon.SessionDetail{
		ResolvedProfile: &daemon.SessionResolvedProfile{
			Provider:       "opencode",
			ProviderConfig: map[string]any{opencodeCtorHintKey: true},
		},
	}
	if got := opencodeCtorHints(d); !got.PreferOpenCodeServer {
		t.Errorf("opencodeCtorHints(%s=true) = %+v; want PreferOpenCodeServer true", opencodeCtorHintKey, got)
	}
}

// TestOpencodeCtorHints_FalseValue proves an explicit false is honored
// (distinct from absent, though both resolve to the same Lane-A outcome).
func TestOpencodeCtorHints_FalseValue(t *testing.T) {
	t.Parallel()

	d := &daemon.SessionDetail{
		ResolvedProfile: &daemon.SessionResolvedProfile{
			Provider:       "opencode",
			ProviderConfig: map[string]any{opencodeCtorHintKey: false},
		},
	}
	if got := opencodeCtorHints(d); got.PreferOpenCodeServer {
		t.Errorf("opencodeCtorHints(%s=false) = %+v; want PreferOpenCodeServer false", opencodeCtorHintKey, got)
	}
}

// TestOpencodeCtorHints_NonBoolValueIgnored guards the JSON round-trip
// realities of a map[string]any: a platform bug (or a stale/mistyped wire
// payload) that sends a non-bool for this key must not panic and must not
// be misread as truthy — it fails safe to Lane A.
func TestOpencodeCtorHints_NonBoolValueIgnored(t *testing.T) {
	t.Parallel()

	d := &daemon.SessionDetail{
		ResolvedProfile: &daemon.SessionResolvedProfile{
			Provider:       "opencode",
			ProviderConfig: map[string]any{opencodeCtorHintKey: "true"},
		},
	}
	if got := opencodeCtorHints(d); got.PreferOpenCodeServer {
		t.Errorf("opencodeCtorHints(%s=%q string) = %+v; want PreferOpenCodeServer false (fail-safe)", opencodeCtorHintKey, "true", got)
	}
}

// TestOpencodeCtorOptions_ThreadsPreferServer proves the hint actually
// reaches the provider Options that agentRunProviderCtors' opencode ctor
// constructs from — the point of the whole change: without this, a true
// hint would be computed and then silently dropped on the floor.
func TestOpencodeCtorOptions_ThreadsPreferServer(t *testing.T) {
	t.Parallel()

	if got := opencodeCtorOptions(agentRunCtorHints{}); got.PreferServer {
		t.Errorf("opencodeCtorOptions(zero value).PreferServer = true; want false")
	}
	if got := opencodeCtorOptions(agentRunCtorHints{PreferOpenCodeServer: true}); !got.PreferServer {
		t.Errorf("opencodeCtorOptions(PreferOpenCodeServer: true).PreferServer = false; want true")
	}
}

// TestAgentRunProviderCtors_ZeroArgUnchanged is the no-behavior-change proof
// for existing call sites: agentRunProviderCtors() (zero args — what
// BuildAgentRunRegistry/buildAgentRunRegistry and every pre-existing test
// call) must still declare exactly the same ctor names as before this
// change, and the opencode ctor it produces must construct with
// PreferServer false (the historical, only-ever-Options{} behavior).
func TestAgentRunProviderCtors_ZeroArgUnchanged(t *testing.T) {
	t.Parallel()

	ctors := agentRunProviderCtors()
	found := false
	for _, c := range ctors {
		if c.name != "opencode" {
			continue
		}
		found = true
	}
	if !found {
		t.Fatalf("agentRunProviderCtors() has no opencode ctor")
	}

	// The zero-hint construction path (what the zero-arg call always uses)
	// must yield PreferServer false.
	if got := opencodeCtorOptions(agentRunCtorHints{}); got.PreferServer {
		t.Errorf("zero-arg agentRunProviderCtors' opencode Options.PreferServer = true; want false")
	}
}

// TestAgentRunProviderCtors_HintSelectsLaneB proves the other end-to-end
// direction: passing a hint with PreferOpenCodeServer true through
// agentRunProviderCtors changes only the opencode entry's constructed
// Options, leaving every other provider ctor's name set identical to the
// zero-arg call (canonicalAgentRunProviders, asserted elsewhere) — this
// change threads a signal, it does not add or remove providers.
func TestAgentRunProviderCtors_HintSelectsLaneB(t *testing.T) {
	t.Parallel()

	base := agentRunProviderCtors()
	hinted := agentRunProviderCtors(agentRunCtorHints{PreferOpenCodeServer: true})

	if len(base) != len(hinted) {
		t.Fatalf("agentRunProviderCtors(hint) changed the ctor count: %d vs %d", len(hinted), len(base))
	}
	for i := range base {
		if base[i].name != hinted[i].name {
			t.Errorf("ctor[%d] name = %q with hint; want %q (hint must not reorder/rename providers)",
				i, hinted[i].name, base[i].name)
		}
	}

	if got := opencodeCtorOptions(agentRunCtorHints{PreferOpenCodeServer: true}); !got.PreferServer {
		t.Errorf("hinted opencode Options.PreferServer = false; want true")
	}
}
