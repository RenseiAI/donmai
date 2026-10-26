package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/RenseiAI/donmai/agent"
	"github.com/RenseiAI/donmai/worker"
)

func quotaClaimWindow(id string, used float64, resetsAt string) agent.UsageWindow {
	return agent.UsageWindow{ID: id, Kind: agent.UsageWindowSession, Label: "Session", UsedPercent: used, ResetsAt: resetsAt}
}

func quotaClaimItem(sessionID string) PollWorkItem {
	item := PollWorkItem{
		SessionID: sessionID, ProjectID: "project", Repository: "github.com/acme/repo",
		Priority: 1, QueuedAt: 1700000000000,
		ResolvedProfile: &SessionResolvedProfile{Harness: "codex", AuthMode: string(agent.AuthHostSession)},
	}
	item.claimAttempt = &workClaimAttemptProof{ContractVersion: workClaimAttemptContractVersion, SessionID: sessionID, AttemptToken: claimAttemptTokenOne}
	return item
}

// quotaClaimPollBody renders a real poll envelope: host-session work plus the
// sibling claimAttempts member the poll decoder binds outside work JSON.
func quotaClaimPollBody(sessionID, token string) string {
	return fmt.Sprintf(`{"work":[{"sessionId":%q,"projectId":"project",`+
		`"repository":"github.com/acme/repo","priority":1,"queuedAt":1700000000000,"resolvedProfile":{"harness":"codex","authMode":"host-session"}}],`+
		`"claimAttempts":{%q:{"contractVersion":"work-claim-attempt/v1","sessionId":%q,"attemptToken":%q}}}`,
		sessionID, sessionID, sessionID, token)
}

func seedQuotaWindows(d *Daemon, windows []agent.UsageWindow) {
	now := time.Now()
	d.quota.noteCodexProbe("opaque-codex", "promax", agent.MakeUsageLimits(agent.ISOTime(now), windows), now)
}

func seedClaudeWindows(d *Daemon, windows []agent.UsageWindow) {
	now := time.Now()
	d.quota.noteClaudeProbe("opaque-claude", agent.MakeUsageLimits(agent.ISOTime(now), windows), now)
}

func seedQuotaWindowsAt(d *Daemon, windows []agent.UsageWindow, checkedAt, probeAt time.Time) {
	d.quota.noteCodexProbe("opaque-codex", "promax", agent.MakeUsageLimits(agent.ISOTime(checkedAt), windows), probeAt)
}

func seedClaudeWindowsAt(d *Daemon, windows []agent.UsageWindow, checkedAt, probeAt time.Time) {
	d.quota.noteClaudeProbe("opaque-claude", agent.MakeUsageLimits(agent.ISOTime(checkedAt), windows), probeAt)
}

func seedClaudeUnsupported(d *Daemon, at time.Time) {
	d.quota.noteClaudeProbe("opaque-claude",
		agent.MakeUnavailableUsageLimits(agent.ISOTime(at), agent.UsageUnavailableUnsupported, "usage read unsupported"), at)
}

// quotaNamesRegistry is a minimal ProviderRegistry stub declaring exactly the
// provider names given, so quota-gate tests can state which harnesses (and,
// by presence, key-carried auth) a host serves.
type quotaNamesRegistry struct{ names []string }

func (r quotaNamesRegistry) Names() []string { return append([]string(nil), r.names...) }
func (r quotaNamesRegistry) Capabilities(string) (map[string]any, bool) {
	return map[string]any{}, true
}

// quotaClaimPollBodyFor renders a poll envelope for one harness/auth-mode
// pair plus the sibling claimAttempts member the poll decoder binds.
func quotaClaimPollBodyFor(sessionID, harness, authMode, token string) string {
	return fmt.Sprintf(`{"work":[{"sessionId":%q,"projectId":"project",`+
		`"repository":"github.com/acme/repo","priority":1,"queuedAt":1700000000000,"resolvedProfile":{"harness":%q,"authMode":%q}}],`+
		`"claimAttempts":{%q:{"contractVersion":"work-claim-attempt/v1","sessionId":%q,"attemptToken":%q}}}`,
		sessionID, harness, authMode, sessionID, sessionID, token)
}

func quotaClaimDaemon(t *testing.T, spawns *int) *Daemon {
	t.Helper()
	return quotaClaimDaemonWithOptions(t, Options{EnableSubscriptionWindowExhaustionRefusal: true}, spawns)
}

func quotaClaimDaemonWithOptions(t *testing.T, opts Options, spawns *int) *Daemon {
	t.Helper()
	return newRunningTestDaemon(t, opts, []ProjectConfig{{ID: "project", Repository: "github.com/acme/repo"}},
		func(SessionSpec, []string) ([]string, error) { *spawns++; return nil, nil })
}

// AC-1: a real poll with a valid claim attempt and an exhausted matching
// snapshot sends one NACK with the closed typed member (latest reset) and
// the matching attempt; nothing spawns.
func TestQuotaClaimAdmission_ExhaustedSnapshotNacksTypedReset(t *testing.T) {
	resetLate := "2031-07-08T09:10:11Z"
	fixture := newClaimAttemptHTTPFixture(t, quotaClaimPollBody("quota-1", claimAttemptTokenOne))
	var spawns int
	d := quotaClaimDaemon(t, &spawns)
	seedQuotaWindows(d, []agent.UsageWindow{
		quotaClaimWindow("five_hour", 100, "2031-02-03T04:05:06Z"),
		quotaClaimWindow("seven_day", 100, resetLate),
	})

	var onWorkErr error
	NewPollService(PollOptions{
		WorkerID: "worker-quota", RuntimeJWT: "runtime-quota",
		OrchestratorURL: fixture.server.URL, HTTPClient: fixture.server.Client(),
		OnWork: func(item PollWorkItem) error {
			onWorkErr = d.handlePollWorkItem(item, fixture.server.URL)
			return onWorkErr
		},
	}).pollOnce(context.Background())

	var exhausted *subscriptionWindowExhaustedError
	if !errors.As(onWorkErr, &exhausted) || !exhausted.usable {
		t.Fatalf("handlePollWorkItem error = %v, want usable quota-exhaustion refusal", onWorkErr)
	}
	if spawns != 0 || d.spawner.ActiveCount() != 0 {
		t.Fatalf("spawns = %d active = %d, want no spawn or reservation", spawns, d.spawner.ActiveCount())
	}
	nacks := fixture.nackBodies()
	if len(nacks) != 1 {
		t.Fatalf("NACK count = %d, want exactly 1", len(nacks))
	}
	var report map[string]any
	if err := json.Unmarshal(nacks[0]["subscriptionWindowExhausted"], &report); err != nil {
		t.Fatalf("typed member missing or invalid: %s", mustJSON(nacks[0]))
	}
	if !mapsEqual(report, map[string]any{
		"contractVersion": subscriptionWindowExhaustedContractVersion,
		"code":            subscriptionWindowExhaustedCode, "resetAt": resetLate,
	}) {
		t.Fatalf("typed member = %#v, want version/code/latest reset", report)
	}
	if proof := decodeClaimAttemptBody(t, nacks[0]["claimAttempt"]); proof["sessionId"] != "quota-1" ||
		proof["attemptToken"] != claimAttemptTokenOne {
		t.Fatalf("claimAttempt = %#v, want exact session/attempt binding", proof)
	}
	if got := string(nacks[0]["reason"]); got != `"accept work failed: subscription window exhausted"` {
		t.Fatalf("reason = %s, want fixed compatibility prose", got)
	}
}

// AC-2 and AC-3 share one guard table: fresh missing/malformed resets refuse
// before any spawn with no typed member (fail closed, no guessed instant),
// while other auth modes, unmatched or absent accounts, usage below 100%, a
// reset at or before now (the window has reset), and any stale snapshot
// keep the existing admission (spawn, no NACK).
func TestQuotaClaimAdmission_Guards(t *testing.T) {
	exhausted := []agent.UsageWindow{quotaClaimWindow("five_hour", 100, "2031-02-03T04:05:06Z")}
	cases := []struct {
		name, authMode, harness string
		windows                 []agent.UsageWindow // nil means no cached account at all
		refuse                  bool                // fail-closed refusal with a bare (untyped) NACK
		stale                   bool                // seed CheckedAt beyond the freshness TTL
	}{
		{"missing reset refuses untyped", string(agent.AuthHostSession), "codex", []agent.UsageWindow{quotaClaimWindow("five_hour", 100, "")}, true, false},
		{"malformed reset refuses untyped", string(agent.AuthHostSession), "codex", []agent.UsageWindow{quotaClaimWindow("five_hour", 100, "not-a-time")}, true, false},
		{"partial reset refuses untyped", string(agent.AuthHostSession), "codex", []agent.UsageWindow{quotaClaimWindow("five_hour", 100, "2031-02-03T04:05:06Z"), quotaClaimWindow("seven_day", 100, "")}, true, false},
		{"past reset admits (window has reset)", string(agent.AuthHostSession), "codex", []agent.UsageWindow{quotaClaimWindow("five_hour", 100, "2001-01-02T03:04:05Z")}, false, false},
		{"stale snapshot admits even with future reset", string(agent.AuthHostSession), "codex", exhausted, false, true},
		{"stale snapshot admits even with malformed reset", string(agent.AuthHostSession), "codex", []agent.UsageWindow{quotaClaimWindow("five_hour", 100, "not-a-time")}, false, true},
		{"other auth admits", string(agent.AuthBYOK), "codex", exhausted, false, false},
		{"unmatched harness admits", string(agent.AuthHostSession), "claude", exhausted, false, false},
		{"absent account admits", string(agent.AuthHostSession), "codex", nil, false, false},
		{"usage below full admits", string(agent.AuthHostSession), "codex", []agent.UsageWindow{quotaClaimWindow("five_hour", 99.9, "2031-02-03T04:05:06Z")}, false, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fixture := newClaimAttemptHTTPFixture(t)
			var spawns int
			d := quotaClaimDaemon(t, &spawns)
			if tc.windows != nil {
				if tc.stale {
					now := time.Now()
					seedQuotaWindowsAt(d, tc.windows, now.Add(-time.Hour), now.Add(-time.Hour))
				} else {
					seedQuotaWindows(d, tc.windows)
				}
			}
			item := quotaClaimItem("quota-guard")
			item.ResolvedProfile.AuthMode, item.ResolvedProfile.Harness = tc.authMode, tc.harness
			err, nacks := d.handlePollWorkItem(item, fixture.server.URL), fixture.nackBodies()
			if !tc.refuse && (err != nil || spawns != 1 || len(nacks) != 0) {
				t.Fatalf("err = %v spawns = %d nacks = %d, want admission preserved", err, spawns, len(nacks))
			}
			var denied *subscriptionWindowExhaustedError
			if tc.refuse && (!errors.As(err, &denied) || denied.usable || spawns != 0 || len(nacks) != 1) {
				t.Fatalf("err = %v spawns = %d nacks = %d, want fail-closed refusal", err, spawns, len(nacks))
			}
			if tc.refuse {
				if member, ok := nacks[0]["subscriptionWindowExhausted"]; ok {
					t.Fatalf("typed member %s, want omitted without a valid reset", string(member))
				}
			}
		})
	}
}

// B1: the poll claim gate is the primary decline. With a fresh exhausted
// snapshot, five consecutive production poll ticks through PollClaimGate must
// issue no poll request at all — no claim is taken, so no NACK can be
// counted toward terminalization and nothing spawns. Past-reset and stale
// snapshots reopen the gate.
func TestQuotaClaimAdmission_GateStopsPollingBeforeClaim(t *testing.T) {
	newExhaustedGateFixture := func(t *testing.T, windows []agent.UsageWindow, stale bool) (*Daemon, *claimAttemptHTTPFixture, *int, *PollService) {
		t.Helper()
		bodies := make([]string, 5)
		for i := range bodies {
			bodies[i] = quotaClaimPollBody("quota-gate", claimAttemptTokenOne)
		}
		fixture := newClaimAttemptHTTPFixture(t, bodies...)
		var spawns int
		d := quotaClaimDaemon(t, &spawns)
		if stale {
			now := time.Now()
			seedQuotaWindowsAt(d, windows, now.Add(-time.Hour), now.Add(-time.Hour))
		} else {
			seedQuotaWindows(d, windows)
		}
		poller := NewPollService(PollOptions{
			WorkerID: "worker-quota", RuntimeJWT: "runtime-quota",
			OrchestratorURL: fixture.server.URL, HTTPClient: fixture.server.Client(),
			OnWork: func(item PollWorkItem) error {
				return d.handlePollWorkItem(item, fixture.server.URL)
			},
			ClaimSuspended: d.PollClaimGate(),
		})
		return d, fixture, &spawns, poller
	}
	exhausted := []agent.UsageWindow{quotaClaimWindow("five_hour", 100, "2031-02-03T04:05:06Z")}
	t.Run("fresh exhaustion suspends five consecutive polls", func(t *testing.T) {
		d, fixture, spawns, poller := newExhaustedGateFixture(t, exhausted, false)
		if blocked, reason := d.PollClaimGate()(); !blocked || reason == "" {
			t.Fatalf("claim gate blocked = %v reason = %q, want suspended with reason", blocked, reason)
		}
		for range 5 {
			poller.pollOnce(context.Background())
		}
		if n := fixture.pollIndex; n != 0 {
			t.Fatalf("poll requests = %d, want 0 (gate must decline before claiming)", n)
		}
		if nacks := fixture.nackBodies(); len(nacks) != 0 {
			t.Fatalf("NACK count = %d, want 0 (no claim means no NACK)", len(nacks))
		}
		if *spawns != 0 || d.spawner.ActiveCount() != 0 {
			t.Fatalf("spawns = %d active = %d, want no spawn or reservation", *spawns, d.spawner.ActiveCount())
		}
		if !poller.ClaimsSuspended() {
			t.Fatal("ClaimsSuspended() = false after gated ticks, want true")
		}
	})
	t.Run("past reset reopens the gate", func(t *testing.T) {
		d, fixture, _, poller := newExhaustedGateFixture(t,
			[]agent.UsageWindow{quotaClaimWindow("five_hour", 100, "2001-01-02T03:04:05Z")}, false)
		if blocked, _ := d.PollClaimGate()(); blocked {
			t.Fatal("claim gate blocked on past reset, want admission")
		}
		poller.pollOnce(context.Background())
		if n := fixture.pollIndex; n != 1 {
			t.Fatalf("poll requests = %d, want 1 (past reset admits)", n)
		}
	})
	t.Run("stale snapshot reopens the gate", func(t *testing.T) {
		d, fixture, _, poller := newExhaustedGateFixture(t, exhausted, true)
		if blocked, _ := d.PollClaimGate()(); blocked {
			t.Fatal("claim gate blocked on stale snapshot, want admission as unknown")
		}
		poller.pollOnce(context.Background())
		if n := fixture.pollIndex; n != 1 {
			t.Fatalf("poll requests = %d, want 1 (stale snapshot admits)", n)
		}
	})
}

// AC-4: an identical generic error and a provider 429 surfaced as prose never
// gain the quota typed member: only the typed admission error projects it.
func TestQuotaClaimAdmission_UpstreamFailuresStayUntyped(t *testing.T) {
	typed := &subscriptionWindowExhaustedError{resetAt: "2031-02-03T04:05:06Z", usable: true}
	upstream := agent.NormalizeUpstreamError(429, "rate_limit", "quota exhausted", "2031-02-03T04:05:06Z")
	cases := []struct {
		name string
		err  error
	}{
		{"identical prose", errors.New(typed.Error())},
		{"provider 429", fmt.Errorf("provider request failed: status %d code %s: %s", upstream.HTTPStatus, upstream.ProviderCode, upstream.ProviderMessage)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fixture := newClaimAttemptHTTPFixture(t)
			item := quotaClaimItem("quota-untyped")
			if err := NackRejectedWork(context.Background(), fixture.server.Client(),
				fixture.server.URL, "worker-quota", "runtime-quota", &item, tc.err); err != nil {
				t.Fatalf("NackRejectedWork: %v", err)
			}
			if nacks := fixture.nackBodies(); len(nacks) != 1 {
				t.Fatalf("NACK count = %d, want exactly 1", len(nacks))
			} else if member, ok := nacks[0]["subscriptionWindowExhausted"]; ok {
				t.Fatalf("typed member %s for %s, want omitted", string(member), tc.name)
			}
		})
	}
}

// AC-5: a missing, malformed, or mismatched claimAttempt cannot emit the
// typed member; the proof stays out of work JSON and the operational payload.
func TestQuotaClaimAdmission_ProofGateAndPayloadBoundary(t *testing.T) {
	now := time.Now()
	quotaErr := decideQuotaClaimAdmission(
		[]agent.UsageAccount{{ID: "opaque-codex", Provider: "codex", Limits: agent.MakeUsageLimits(agent.ISOTime(now),
			[]agent.UsageWindow{quotaClaimWindow("five_hour", 100, "2031-02-03T04:05:06Z")})}},
		string(agent.AuthHostSession), "codex", now)
	if subscriptionWindowExhaustedReportForError(quotaErr) == nil {
		t.Fatal("usable fixture refusal produced no report, want typed report")
	}
	proofs := map[string]*workClaimAttemptProof{
		"missing": nil,
		"version": {ContractVersion: "work-claim-attempt/v0", SessionID: "s", AttemptToken: claimAttemptTokenOne},
		"session": {ContractVersion: workClaimAttemptContractVersion, SessionID: "other", AttemptToken: claimAttemptTokenOne},
		"token":   {ContractVersion: workClaimAttemptContractVersion, SessionID: "s", AttemptToken: "not-a-uuid"},
	}
	for name, proof := range proofs {
		t.Run(name, func(t *testing.T) {
			fixture := newClaimAttemptHTTPFixture(t)
			var envelope PollResponse
			if err := json.Unmarshal([]byte(quotaClaimPollBody("s", claimAttemptTokenOne)), &envelope); err != nil {
				t.Fatal(err)
			}
			item := envelope.Work[0]
			item.claimAttempt = proof
			if err := NackRejectedWork(context.Background(), fixture.server.Client(),
				fixture.server.URL, "worker-quota", "runtime-quota", &item, quotaErr); err != nil {
				t.Fatalf("NackRejectedWork: %v", err)
			}
			nacks := fixture.nackBodies()
			if len(nacks) != 1 {
				t.Fatalf("NACK count = %d, want exactly 1", len(nacks))
			}
			if member, ok := nacks[0]["subscriptionWindowExhausted"]; ok {
				t.Fatalf("typed member %s with %s proof, want omitted", string(member), name)
			}
			var work map[string]json.RawMessage
			if err := json.Unmarshal(nacks[0]["work"], &work); err != nil {
				t.Fatal(err)
			}
			if _, ok := work["claimAttempt"]; ok {
				t.Fatalf("claimAttempt entered work JSON with %s proof", name)
			}
			if payload, ok := work["operationalPayload"]; !ok ||
				strings.Contains(string(payload), "claimAttempt") ||
				strings.Contains(string(payload), claimAttemptTokenOne) {
				t.Fatalf("proof entered operational payload with %s proof", name)
			}
		})
	}
}

// Mixed-host gate: one exhausted harness must not starve the other harness,
// other auth modes, or the non-session lanes on the same poll response. The
// gate suspends ONLY when every fresh harness snapshot is still blocking;
// otherwise the poll goes out and the per-item admission refuses only the
// exhausted harness. A fresh missing reset still suspends when it is the
// only fresh snapshot (fail closed, bounded by the freshness TTL).
func TestQuotaClaimAdmission_MixedHarnessKeepsPolling(t *testing.T) {
	exhaustedCodex := []agent.UsageWindow{quotaClaimWindow("seven_day", 100, "2031-02-03T04:05:06Z")}
	availableClaude := []agent.UsageWindow{quotaClaimWindow("five_hour", 42, "2031-02-03T04:05:06Z")}

	t.Run("codex exhausted claude available still polls and spawns claude", func(t *testing.T) {
		fixture := newClaimAttemptHTTPFixture(t, quotaClaimPollBodyFor("claude-1", "claude", string(agent.AuthHostSession), claimAttemptTokenOne))
		var spawns int
		d := quotaClaimDaemon(t, &spawns)
		seedQuotaWindows(d, exhaustedCodex)
		seedClaudeWindows(d, availableClaude)
		if blocked, _ := d.PollClaimGate()(); blocked {
			t.Fatal("claim gate blocked with claude available, want open")
		}
		poller := NewPollService(PollOptions{
			WorkerID: "worker-quota", RuntimeJWT: "runtime-quota",
			OrchestratorURL: fixture.server.URL, HTTPClient: fixture.server.Client(),
			OnWork: func(item PollWorkItem) error {
				return d.handlePollWorkItem(item, fixture.server.URL)
			},
			ClaimSuspended: d.PollClaimGate(),
		})
		poller.pollOnce(context.Background())
		if n := fixture.pollIndex; n != 1 {
			t.Fatalf("poll requests = %d, want 1 (mixed host keeps polling)", n)
		}
		if spawns != 1 || len(fixture.nackBodies()) != 0 {
			t.Fatalf("spawns = %d nacks = %d, want claude admitted without NACK", spawns, len(fixture.nackBodies()))
		}
		if poller.ClaimsSuspended() {
			t.Fatal("ClaimsSuspended() = true with claude available, want false")
		}
	})

	t.Run("codex exhausted BYOK still polls and spawns through production gate", func(t *testing.T) {
		fixture := newClaimAttemptHTTPFixture(t, quotaClaimPollBodyFor("byok-1", "codex", string(agent.AuthBYOK), claimAttemptTokenOne))
		var spawns int
		d := quotaClaimDaemon(t, &spawns)
		seedQuotaWindows(d, exhaustedCodex)
		seedClaudeWindows(d, availableClaude)
		if blocked, _ := d.PollClaimGate()(); blocked {
			t.Fatal("claim gate blocked with claude available, want open for BYOK")
		}
		poller := NewPollService(PollOptions{
			WorkerID: "worker-quota", RuntimeJWT: "runtime-quota",
			OrchestratorURL: fixture.server.URL, HTTPClient: fixture.server.Client(),
			OnWork: func(item PollWorkItem) error {
				return d.handlePollWorkItem(item, fixture.server.URL)
			},
			ClaimSuspended: d.PollClaimGate(),
		})
		poller.pollOnce(context.Background())
		if n := fixture.pollIndex; n != 1 {
			t.Fatalf("poll requests = %d, want 1 (BYOK rides the open gate)", n)
		}
		if spawns != 1 || len(fixture.nackBodies()) != 0 {
			t.Fatalf("spawns = %d nacks = %d, want BYOK admitted without NACK", spawns, len(fixture.nackBodies()))
		}
	})

	t.Run("codex exhausted claude available keeps landing and kg lanes flowing", func(t *testing.T) {
		body := `{"work":[{"sessionId":"claude-lane","projectId":"project","repository":"github.com/acme/repo","priority":1,"queuedAt":1700000000000,"resolvedProfile":{"harness":"claude","authMode":"host-session"}}],` +
			`"landingWork":[{"batchJobId":"batch:landing:1","workType":"landing-run","orgId":"org-1","repoId":"repo-1"}],` +
			`"kgExtractWork":[{"batchJobId":"batch:kg:1","workType":"kg-extraction"}],` +
			`"claimAttempts":{"claude-lane":{"contractVersion":"work-claim-attempt/v1","sessionId":"claude-lane","attemptToken":"` + claimAttemptTokenOne + `"}}}`
		fixture := newClaimAttemptHTTPFixture(t, body)
		var spawns, landings int
		var kgs atomic.Int32
		d := newRunningTestDaemon(t, Options{
			EnableSubscriptionWindowExhaustionRefusal: true,
			OnLandingWork: func(context.Context, PollWorkItem) error { landings++; return nil },
		}, []ProjectConfig{{ID: "project", Repository: "github.com/acme/repo"}},
			func(SessionSpec, []string) ([]string, error) { spawns++; return nil, nil })
		seedQuotaWindows(d, exhaustedCodex)
		seedClaudeWindows(d, availableClaude)
		poller := NewPollService(PollOptions{
			WorkerID: "worker-quota", RuntimeJWT: "runtime-quota",
			OrchestratorURL: fixture.server.URL, HTTPClient: fixture.server.Client(),
			OnWork: func(item PollWorkItem) error {
				return d.handlePollWorkItem(item, fixture.server.URL)
			},
			OnKgExtractWork: func(context.Context, worker.BatchWorkItem) error { kgs.Add(1); return nil },
			ClaimSuspended:  d.PollClaimGate(),
		})
		poller.pollOnce(context.Background())
		poller.Stop()
		if n := fixture.pollIndex; n != 1 {
			t.Fatalf("poll requests = %d, want 1 (lanes ride the open poll)", n)
		}
		if spawns != 1 {
			t.Fatalf("spawns = %d, want 1 claude session", spawns)
		}
		if landings != 1 {
			t.Fatalf("landing handler calls = %d, want 1", landings)
		}
		deadline := time.Now().Add(5 * time.Second)
		for kgs.Load() == 0 && time.Now().Before(deadline) {
			time.Sleep(10 * time.Millisecond)
		}
		if got := kgs.Load(); got != 1 {
			t.Fatalf("kg handler calls = %d, want 1", got)
		}
		if nacks := fixture.nackBodies(); len(nacks) != 0 {
			t.Fatalf("NACK count = %d, want 0", len(nacks))
		}
	})

	t.Run("single-harness missing reset suspends polling before claim", func(t *testing.T) {
		bodies := make([]string, 2)
		for i := range bodies {
			bodies[i] = quotaClaimPollBody("quota-gate", claimAttemptTokenOne)
		}
		fixture := newClaimAttemptHTTPFixture(t, bodies...)
		var spawns int
		d := quotaClaimDaemon(t, &spawns)
		seedQuotaWindows(d, []agent.UsageWindow{quotaClaimWindow("five_hour", 100, "")})
		if blocked, reason := d.PollClaimGate()(); !blocked || reason == "" {
			t.Fatalf("claim gate blocked = %v reason = %q, want suspended awaiting probe", blocked, reason)
		}
		poller := NewPollService(PollOptions{
			WorkerID: "worker-quota", RuntimeJWT: "runtime-quota",
			OrchestratorURL: fixture.server.URL, HTTPClient: fixture.server.Client(),
			OnWork: func(item PollWorkItem) error {
				return d.handlePollWorkItem(item, fixture.server.URL)
			},
			ClaimSuspended: d.PollClaimGate(),
		})
		for range 2 {
			poller.pollOnce(context.Background())
		}
		if n := fixture.pollIndex; n != 0 {
			t.Fatalf("poll requests = %d, want 0 (unusable snapshot fails closed before claim)", n)
		}
		if nacks := fixture.nackBodies(); len(nacks) != 0 {
			t.Fatalf("NACK count = %d, want 0 (no claim means no NACK)", len(nacks))
		}
		if spawns != 0 {
			t.Fatalf("spawns = %d, want 0", spawns)
		}
	})
}

// The exhaustion behaviour is behind a default-off flag: with the flag off
// the host behaves exactly as before the refusal existed. The gate stays
// open and an exhausted host-session item is admitted and spawned with no
// NACK and no typed member — even with a fresh blocking snapshot, and even
// with a fresh missing reset that fails closed when the flag is on.
func TestQuotaClaimAdmission_FlagOffAdmitsExhaustedSnapshot(t *testing.T) {
	cases := []struct {
		name    string
		windows []agent.UsageWindow
	}{
		{"exhausted future reset admits", []agent.UsageWindow{quotaClaimWindow("seven_day", 100, "2031-02-03T04:05:06Z")}},
		{"missing reset admits", []agent.UsageWindow{quotaClaimWindow("five_hour", 100, "")}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fixture := newClaimAttemptHTTPFixture(t, quotaClaimPollBody("flag-off-1", claimAttemptTokenOne))
			var spawns int
			// Default options: the flag is off.
			d := quotaClaimDaemonWithOptions(t, Options{}, &spawns)
			seedQuotaWindows(d, tc.windows)
			if blocked, reason := d.PollClaimGate()(); blocked {
				t.Fatalf("claim gate blocked with flag off (reason %q), want open", reason)
			}
			poller := NewPollService(PollOptions{
				WorkerID: "worker-quota", RuntimeJWT: "runtime-quota",
				OrchestratorURL: fixture.server.URL, HTTPClient: fixture.server.Client(),
				OnWork: func(item PollWorkItem) error {
					return d.handlePollWorkItem(item, fixture.server.URL)
				},
				ClaimSuspended: d.PollClaimGate(),
			})
			poller.pollOnce(context.Background())
			if n := fixture.pollIndex; n != 1 {
				t.Fatalf("poll requests = %d, want 1 (flag off never suspends)", n)
			}
			if spawns != 1 {
				t.Fatalf("spawns = %d, want 1 (flag off admits)", spawns)
			}
			if nacks := fixture.nackBodies(); len(nacks) != 0 {
				t.Fatalf("NACK count = %d, want 0 (flag off never refuses)", len(nacks))
			}
		})
	}
}

// Served lanes keep the poll open: the host-wide gate suspends only when
// every lane this host serves is freshly exhausted. A served harness without
// a fresh blocking snapshot — none, stale, or an unsupported read that
// clears the account — is available, as is a harness with no probe at all
// and a key-carried auth mode the snapshot never binds. Each row drives a
// production pollOnce through PollClaimGate and asserts the poll goes out
// and the item spawns with no NACK.
func TestQuotaClaimAdmission_ServedLanesKeepPolling(t *testing.T) {
	exhaustedCodex := []agent.UsageWindow{quotaClaimWindow("seven_day", 100, "2031-02-03T04:05:06Z")}

	serve := func(t *testing.T, registry []string, seed func(*Daemon), body string) (*Daemon, *claimAttemptHTTPFixture, *int) {
		t.Helper()
		fixture := newClaimAttemptHTTPFixture(t, body)
		var spawns int
		opts := Options{EnableSubscriptionWindowExhaustionRefusal: true}
		if registry != nil {
			opts.ProviderRegistry = quotaNamesRegistry{names: registry}
		}
		d := quotaClaimDaemonWithOptions(t, opts, &spawns)
		seed(d)
		if blocked, reason := d.PollClaimGate()(); blocked {
			t.Fatalf("claim gate blocked (reason %q), want open with an available served lane", reason)
		}
		poller := NewPollService(PollOptions{
			WorkerID: "worker-quota", RuntimeJWT: "runtime-quota",
			OrchestratorURL: fixture.server.URL, HTTPClient: fixture.server.Client(),
			OnWork: func(item PollWorkItem) error {
				return d.handlePollWorkItem(item, fixture.server.URL)
			},
			ClaimSuspended: d.PollClaimGate(),
		})
		poller.pollOnce(context.Background())
		return d, fixture, &spawns
	}
	checkPolledAndSpawned := func(t *testing.T, d *Daemon, fixture *claimAttemptHTTPFixture, spawns *int) {
		t.Helper()
		if n := fixture.pollIndex; n != 1 {
			t.Fatalf("poll requests = %d, want 1 (available lane keeps polling)", n)
		}
		// No ActiveCount assertion here: the stub spawn completes
		// asynchronously, so the child may still be registered when the
		// tick returns. spawns == 1 plus zero NACKs is the admission
		// signal; the zero-spawn rows below assert ActiveCount == 0.
		if *spawns != 1 {
			t.Fatalf("spawns = %d, want 1 (available lane admits)", *spawns)
		}
		if nacks := fixture.nackBodies(); len(nacks) != 0 {
			t.Fatalf("NACK count = %d, want 0 (available lane admits)", len(nacks))
		}
	}

	t.Run("no claude snapshot keeps polling claude item", func(t *testing.T) {
		d, fixture, spawns := serve(t, []string{"codex", "claude"},
			func(d *Daemon) { seedQuotaWindows(d, exhaustedCodex) },
			quotaClaimPollBodyFor("claude-1", "claude", string(agent.AuthHostSession), claimAttemptTokenOne))
		checkPolledAndSpawned(t, d, fixture, spawns)
	})

	t.Run("stale claude snapshot keeps polling claude item", func(t *testing.T) {
		d, fixture, spawns := serve(t, []string{"codex", "claude"},
			func(d *Daemon) {
				seedQuotaWindows(d, exhaustedCodex)
				stale := time.Now().Add(-time.Hour)
				seedClaudeWindowsAt(d, exhaustedCodex, stale, stale)
			},
			quotaClaimPollBodyFor("claude-2", "claude", string(agent.AuthHostSession), claimAttemptTokenOne))
		checkPolledAndSpawned(t, d, fixture, spawns)
	})

	t.Run("unsupported claude probe keeps polling claude item", func(t *testing.T) {
		d, fixture, spawns := serve(t, []string{"codex", "claude"},
			func(d *Daemon) {
				seedQuotaWindows(d, exhaustedCodex)
				seedClaudeUnsupported(d, time.Now())
			},
			quotaClaimPollBodyFor("claude-3", "claude", string(agent.AuthHostSession), claimAttemptTokenOne))
		checkPolledAndSpawned(t, d, fixture, spawns)
	})

	t.Run("pi item on codex-snapshot host keeps polling", func(t *testing.T) {
		d, fixture, spawns := serve(t, []string{"codex", "pi"},
			func(d *Daemon) { seedQuotaWindows(d, exhaustedCodex) },
			quotaClaimPollBodyFor("pi-1", "pi", string(agent.AuthHostSession), claimAttemptTokenOne))
		checkPolledAndSpawned(t, d, fixture, spawns)
	})

	t.Run("codex BYOK on codex-snapshot host keeps polling", func(t *testing.T) {
		d, fixture, spawns := serve(t, []string{"codex"},
			func(d *Daemon) { seedQuotaWindows(d, exhaustedCodex) },
			quotaClaimPollBodyFor("byok-1", "codex", string(agent.AuthBYOK), claimAttemptTokenOne))
		checkPolledAndSpawned(t, d, fixture, spawns)
	})

	t.Run("fully exhausted registry host still refuses per item with typed NACK", func(t *testing.T) {
		// Every host-session lane is freshly exhausted, but the host serves
		// key-carried lanes the snapshot never binds, so the gate stays
		// open and the per-item fallback refuses the exhausted item with
		// the typed contract before reserve or spawn.
		body := quotaClaimPollBodyFor("codex-9", "codex", string(agent.AuthHostSession), claimAttemptTokenOne)
		fixture := newClaimAttemptHTTPFixture(t, body)
		var spawns int
		opts := Options{EnableSubscriptionWindowExhaustionRefusal: true, ProviderRegistry: quotaNamesRegistry{names: []string{"codex", "claude"}}}
		d := quotaClaimDaemonWithOptions(t, opts, &spawns)
		seedQuotaWindows(d, exhaustedCodex)
		seedClaudeWindows(d, exhaustedCodex)
		if blocked, reason := d.PollClaimGate()(); blocked {
			t.Fatalf("claim gate blocked (reason %q), want open: key-carried lanes never exhaust", reason)
		}
		var onWorkErr error
		NewPollService(PollOptions{
			WorkerID: "worker-quota", RuntimeJWT: "runtime-quota",
			OrchestratorURL: fixture.server.URL, HTTPClient: fixture.server.Client(),
			OnWork: func(item PollWorkItem) error {
				onWorkErr = d.handlePollWorkItem(item, fixture.server.URL)
				return onWorkErr
			},
			ClaimSuspended: d.PollClaimGate(),
		}).pollOnce(context.Background())
		if n := fixture.pollIndex; n != 1 {
			t.Fatalf("poll requests = %d, want 1 (gate stays open for key-carried lanes)", n)
		}
		var exhausted *subscriptionWindowExhaustedError
		if !errors.As(onWorkErr, &exhausted) || !exhausted.usable {
			t.Fatalf("handlePollWorkItem error = %v, want usable quota-exhaustion refusal", onWorkErr)
		}
		if spawns != 0 || d.spawner.ActiveCount() != 0 {
			t.Fatalf("spawns = %d active = %d, want no spawn or reservation", spawns, d.spawner.ActiveCount())
		}
		nacks := fixture.nackBodies()
		if len(nacks) != 1 {
			t.Fatalf("NACK count = %d, want exactly 1 (per-item fallback releases the claim)", len(nacks))
		}
		var report map[string]any
		if err := json.Unmarshal(nacks[0]["subscriptionWindowExhausted"], &report); err != nil {
			t.Fatalf("typed member missing or invalid: %s", mustJSON(nacks[0]))
		}
		if !mapsEqual(report, map[string]any{
			"contractVersion": subscriptionWindowExhaustedContractVersion,
			"code":            subscriptionWindowExhaustedCode, "resetAt": "2031-02-03T04:05:06Z",
		}) {
			t.Fatalf("typed member = %#v, want version/code/exact reset", report)
		}
		if proof := decodeClaimAttemptBody(t, nacks[0]["claimAttempt"]); proof["sessionId"] != "codex-9" ||
			proof["attemptToken"] != claimAttemptTokenOne {
			t.Fatalf("claimAttempt = %#v, want exact session/attempt binding", proof)
		}
	})
}

// The host-wide gate measured against explicit served lanes: suspend only
// when every served lane is freshly exhausted. Unprobed harnesses and
// key-carried auth never block; stale snapshots and past resets admit; a
// fresh missing reset fails closed; with nothing freshly blocked the poll
// stays open.
func TestQuotaExhaustionSuspendsClaiming_ServedLanes(t *testing.T) {
	now := time.Now()
	fresh := agent.ISOTime(now)
	stale := agent.ISOTime(now.Add(-time.Hour))
	future := "2031-02-03T04:05:06Z"
	past := "2001-01-02T03:04:05Z"
	account := func(provider, checkedAt string, windows ...agent.UsageWindow) agent.UsageAccount {
		return agent.UsageAccount{ID: "opaque-" + provider, Provider: provider, Limits: agent.MakeUsageLimits(checkedAt, windows)}
	}
	blocked := func(provider, checkedAt, resetsAt string) agent.UsageAccount {
		return account(provider, checkedAt, quotaClaimWindow("seven_day", 100, resetsAt))
	}
	hsOnly := func(harnesses ...string) quotaServedClaimLanes {
		return quotaServedClaimLanes{harnesses: harnesses}
	}
	cases := []struct {
		name     string
		accounts []agent.UsageAccount
		served   quotaServedClaimLanes
		suspend  bool
		reason   string // substring the reason must carry when suspending
	}{
		{"single served harness exhausted suspends",
			[]agent.UsageAccount{blocked("codex", fresh, future)}, hsOnly("codex"), true, future},
		{"unserved harness exhaustion is ignored",
			[]agent.UsageAccount{blocked("codex", fresh, future)}, hsOnly("claude"), false, ""},
		{"served harness without snapshot is available",
			[]agent.UsageAccount{blocked("codex", fresh, future)}, hsOnly("codex", "claude"), false, ""},
		{"stale served snapshot is available",
			[]agent.UsageAccount{blocked("codex", fresh, future), blocked("claude", stale, future)}, hsOnly("codex", "claude"), false, ""},
		{"past reset is available",
			[]agent.UsageAccount{blocked("codex", fresh, future), blocked("claude", fresh, past)}, hsOnly("codex", "claude"), false, ""},
		{"every served harness exhausted suspends with the latest reset",
			[]agent.UsageAccount{blocked("codex", fresh, future), blocked("claude", fresh, "2031-07-08T09:10:11Z")},
			hsOnly("codex", "claude"), true, "2031-07-08T09:10:11Z"},
		{"unprobed served harness without snapshot is available",
			[]agent.UsageAccount{blocked("codex", fresh, future)}, hsOnly("codex", "pi"), false, ""},
		{"key-carried auth lane is always available",
			[]agent.UsageAccount{blocked("codex", fresh, future)},
			quotaServedClaimLanes{harnesses: []string{"codex"}, otherAuthModes: true}, false, ""},
		{"fresh missing reset fails closed",
			[]agent.UsageAccount{account("codex", fresh, quotaClaimWindow("five_hour", 100, ""))}, hsOnly("codex"), true, "awaiting fresh quota probe"},
		{"usage below full admits",
			[]agent.UsageAccount{account("codex", fresh, quotaClaimWindow("five_hour", 99.9, future))}, hsOnly("codex"), false, ""},
		{"no fresh snapshot never suspends",
			[]agent.UsageAccount{blocked("codex", stale, future)}, hsOnly("codex"), false, ""},
		{"no snapshot never suspends",
			nil, hsOnly("codex"), false, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			suspended, reason := quotaExhaustionSuspendsClaiming(tc.accounts, tc.served, now)
			if suspended != tc.suspend {
				t.Fatalf("suspended = %v reason = %q, want %v", suspended, reason, tc.suspend)
			}
			if tc.suspend && (reason == "" || !strings.Contains(reason, tc.reason)) {
				t.Fatalf("reason = %q, want it to carry %q", reason, tc.reason)
			}
			if !tc.suspend && reason != "" {
				t.Fatalf("reason = %q, want empty when open", reason)
			}
		})
	}
}

// The served-lane derivation: the registry states which harnesses the host
// can run, a configured probe declares its harness served, and snapshot
// providers speak for a host with no runtime view. Test-only and
// interactive-only providers never constrain claiming.
func TestQuotaServedClaimLanesForSnapshot(t *testing.T) {
	now := time.Now()
	accounts := []agent.UsageAccount{{
		ID: "opaque-codex", Provider: "codex",
		Limits: agent.MakeUsageLimits(agent.ISOTime(now), []agent.UsageWindow{quotaClaimWindow("seven_day", 42, "2031-02-03T04:05:06Z")}),
	}}

	t.Run("registry declares harnesses and key-carried auth", func(t *testing.T) {
		served := quotaServedClaimLanesForSnapshot(accounts,
			map[string]string{"claude": "/bin/false"},
			quotaNamesRegistry{names: []string{"codex", "pi", "stub", "shell"}})
		for _, want := range []string{"codex", "claude", "pi"} {
			found := false
			for _, got := range served.harnesses {
				found = found || got == want
			}
			if !found {
				t.Fatalf("served harnesses = %v, want %q", served.harnesses, want)
			}
		}
		for _, got := range served.harnesses {
			if got == "stub" || got == "shell" {
				t.Fatalf("served harnesses = %v, want no test-only or interactive-only lanes", served.harnesses)
			}
		}
		if !served.otherAuthModes {
			t.Fatal("otherAuthModes = false with a registered runtime, want true")
		}
	})

	t.Run("no runtime view falls back to snapshots and probes", func(t *testing.T) {
		served := quotaServedClaimLanesForSnapshot(accounts, map[string]string{"claude": "/bin/false"}, nil)
		if served.otherAuthModes {
			t.Fatal("otherAuthModes = true with no registry, want false")
		}
		for _, want := range []string{"codex", "claude"} {
			found := false
			for _, got := range served.harnesses {
				found = found || got == want
			}
			if !found {
				t.Fatalf("served harnesses = %v, want %q", served.harnesses, want)
			}
		}
	})

	t.Run("empty host serves nothing and suspends nothing", func(t *testing.T) {
		served := quotaServedClaimLanesForSnapshot(nil, nil, nil)
		if len(served.harnesses) != 0 || served.otherAuthModes {
			t.Fatalf("served = %+v, want empty", served)
		}
		if suspended, _ := quotaExhaustionSuspendsClaiming(nil, served, now); suspended {
			t.Fatal("suspended with no snapshot, want open")
		}
	})
}
