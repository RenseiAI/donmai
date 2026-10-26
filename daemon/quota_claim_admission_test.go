package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/RenseiAI/donmai/agent"
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

func seedQuotaWindowsAt(d *Daemon, windows []agent.UsageWindow, checkedAt, probeAt time.Time) {
	d.quota.noteCodexProbe("opaque-codex", "promax", agent.MakeUsageLimits(agent.ISOTime(checkedAt), windows), probeAt)
}

func quotaClaimDaemon(t *testing.T, spawns *int) *Daemon {
	t.Helper()
	return newRunningTestDaemon(t, Options{}, []ProjectConfig{{ID: "project", Repository: "github.com/acme/repo"}},
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
