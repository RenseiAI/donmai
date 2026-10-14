package pi

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// --- Interactive applied-receipt extension
// (extensions/donmai-notice-claim.ts) ---
//
// Fast scripted half of the receipt contract: the harness activates the REAL
// production extension under node (no `pi` binary needed) and drives one
// claim through the captured `/donmai-claim` handler. The real-`pi` TUI
// canary owns the end-to-end evidence; this file owns the always-run
// ordering and trust properties: offer-before-apply, idempotent reclaim,
// conflicting-payload refusal, untrusted refusal, and the applied marker
// following the applied message through message_end.

// noticeClaimFixtureResult is one decoded harness report.
type noticeClaimFixtureResult struct {
	Registered bool `json:"registered"`
	Appended   []struct {
		CustomType string         `json:"customType"`
		Data       map[string]any `json:"data"`
	} `json:"appended"`
	Sent []struct {
		Message map[string]any `json:"message"`
		Options map[string]any `json:"options"`
	} `json:"sent"`
	Notified []struct {
		Msg  string `json:"msg"`
		Kind string `json:"kind"`
	} `json:"notified"`
}

// runNoticeClaimFixture activates the real production notice-claim extension
// inside testdata/notice-claim-harness.mjs with the given session entries
// and claim args, and returns the harness's JSON report (appends, sends,
// notifies). trusted controls DONMAI_NOTICE_CLAIM_TRUST — the verdict the
// Go spawn stamps only after the trusted-set check.
func runNoticeClaimFixture(t *testing.T, trusted bool, entriesJSON, claimArgs string) noticeClaimFixtureResult {
	t.Helper()
	nodeAvailable(t)

	harness, err := filepath.Abs(filepath.Join("testdata", "notice-claim-harness.mjs"))
	if err != nil {
		t.Fatalf("resolve harness path: %v", err)
	}
	extPath, err := filepath.Abs(filepath.Join("extensions", noticeClaimFileName))
	if err != nil {
		t.Fatalf("resolve extension path: %v", err)
	}

	cmd := exec.Command("node", harness, extPath, entriesJSON, claimArgs) //nolint:gosec // G204: fixed test-only harness path + node binary resolved from PATH; args are this test's own literals.
	cmd.Env = append(os.Environ(), "DONMAI_PI_HANDSHAKE=")
	if trusted {
		cmd.Env = append(cmd.Env, "DONMAI_NOTICE_CLAIM_TRUST=1")
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("fixture harness failed: %v\nstderr: %s", err, stderr.String())
	}
	var out noticeClaimFixtureResult
	if err := json.Unmarshal(stdout.Bytes(), &out); err != nil {
		t.Fatalf("decode fixture output %q: %v", stdout.String(), err)
	}
	return out
}

// TestNoticeClaimFixture_ClaimPersistsOfferThenApplies is the ordering proof:
// a fresh claim persists the offer entry, applies the notice into the
// conversation as a donmai-notice custom message, and — once pi reports the
// applied message persisted (message_end) — persists the applied receipt
// binding the delivery. The receipt therefore exists only after durable
// conversation persistence, never before it.
//
// RED proof: drop the pi.appendEntry(OFFER_TYPE...) line from the extension
// and the offer entry vanishes from Appended while the send remains.
func TestNoticeClaimFixture_ClaimPersistsOfferThenApplies(t *testing.T) {
	t.Parallel()
	out := runNoticeClaimFixture(t, true, `[]`, `D1 HELLO`)
	if !out.Registered {
		t.Fatal("donmai-claim command was not registered")
	}
	if len(out.Appended) != 2 {
		t.Fatalf("appended = %+v, want exactly the offer plus the applied receipt", out.Appended)
	}
	if out.Appended[0].CustomType != "donmai-notice-offer" {
		t.Errorf("first append = %q, want the offer persisted BEFORE application", out.Appended[0].CustomType)
	}
	if delivery, _ := out.Appended[0].Data["delivery"].(string); delivery != "D1" {
		t.Errorf("offer delivery = %q, want D1", delivery)
	}
	if out.Appended[1].CustomType != "donmai-notice-applied" {
		t.Errorf("second append = %q, want the applied receipt after persistence", out.Appended[1].CustomType)
	}
	if len(out.Sent) != 1 {
		t.Fatalf("sent = %+v, want exactly one applied custom message", out.Sent)
	}
	if typ, _ := out.Sent[0].Message["customType"].(string); typ != "donmai-notice" {
		t.Errorf("sent customType = %q, want donmai-notice", typ)
	}
	if text, _ := out.Sent[0].Message["content"].(string); text != "HELLO" {
		t.Errorf("sent content = %q, want the exact queued text", text)
	}
	if len(out.Notified) != 1 || out.Notified[0].Msg != "claimed D1" {
		t.Errorf("notified = %+v, want exactly [claimed D1]", out.Notified)
	}
}

// TestNoticeClaimFixture_ReclaimAppliedWritesNothing is the idempotence
// proof: a repeated claim for an already-applied delivery reports "already
// applied" and writes nothing — no duplicate turn, no second receipt.
//
// RED proof: drop the appliedReceipt early-return from the extension and the
// reclaim appends a second offer plus a second send.
func TestNoticeClaimFixture_ReclaimAppliedWritesNothing(t *testing.T) {
	t.Parallel()
	entries := `[{"type":"custom","customType":"donmai-notice-offer","data":{"delivery":"D1","text":"HELLO"}},` +
		`{"type":"custom","customType":"donmai-notice-applied","data":{"delivery":"D1","kind":"applied"}}]`
	out := runNoticeClaimFixture(t, true, entries, `D1 HELLO`)
	if len(out.Appended) != 0 || len(out.Sent) != 0 {
		t.Fatalf("reclaim wrote %+v / sent %+v, want nothing written for an applied delivery", out.Appended, out.Sent)
	}
	if len(out.Notified) != 1 || out.Notified[0].Msg != "already applied D1" {
		t.Errorf("notified = %+v, want exactly [already applied D1]", out.Notified)
	}
}

// TestNoticeClaimFixture_ConflictingPayloadRefused proves a conflicting
// payload under the same delivery ID is refused without writing: the offer
// binds (delivery, text) together, so a second text cannot hijack the ID.
func TestNoticeClaimFixture_ConflictingPayloadRefused(t *testing.T) {
	t.Parallel()
	entries := `[{"type":"custom","customType":"donmai-notice-offer","data":{"delivery":"D1","text":"OTHER"}}]`
	out := runNoticeClaimFixture(t, true, entries, `D1 HELLO`)
	if len(out.Appended) != 0 || len(out.Sent) != 0 {
		t.Fatalf("conflicting claim wrote %+v / sent %+v, want nothing written", out.Appended, out.Sent)
	}
	if len(out.Notified) != 1 || !strings.Contains(out.Notified[0].Msg, "different payload") {
		t.Errorf("notified = %+v, want the conflicting-payload refusal", out.Notified)
	}
}

// TestNoticeClaimFixture_UntrustedSessionRefusesEveryClaim is the trust
// boundary proof: without the spawn-stamped trust verdict the claim command
// refuses every delivery and writes nothing — a peer extension that loads
// its own copy of this file, or a session the host did not vet, mints no
// receipt.
//
// RED proof: drop the `trusted` gate from the extension and the untrusted
// claim appends an offer plus a send.
func TestNoticeClaimFixture_UntrustedSessionRefusesEveryClaim(t *testing.T) {
	t.Parallel()
	out := runNoticeClaimFixture(t, false, `[]`, `D1 HELLO`)
	if len(out.Appended) != 0 || len(out.Sent) != 0 {
		t.Fatalf("untrusted claim wrote %+v / sent %+v, want nothing written", out.Appended, out.Sent)
	}
	if len(out.Notified) != 1 || !strings.Contains(out.Notified[0].Msg, "not trusted") {
		t.Errorf("notified = %+v, want the not-trusted refusal", out.Notified)
	}
}

// TestNoticeClaimFixture_OfferWithoutAppliedIsUnappliedWork proves crash
// recovery replays instead of losing: an offer with no applied marker is
// still claimable — the reclaim re-offers nothing (the offer persists) and
// re-applies exactly once.
func TestNoticeClaimFixture_OfferWithoutAppliedIsUnappliedWork(t *testing.T) {
	t.Parallel()
	entries := `[{"type":"custom","customType":"donmai-notice-offer","data":{"delivery":"D1","text":"HELLO"}}]`
	out := runNoticeClaimFixture(t, true, entries, `D1 HELLO`)
	if len(out.Sent) != 1 {
		t.Fatalf("sent = %+v, want exactly one re-application for unapplied work", out.Sent)
	}
	for _, a := range out.Appended {
		if a.CustomType == "donmai-notice-offer" {
			t.Fatalf("reclaim persisted a second offer: %+v", out.Appended)
		}
	}
	if len(out.Notified) != 1 || out.Notified[0].Msg != "claimed D1" {
		t.Errorf("notified = %+v, want exactly [claimed D1]", out.Notified)
	}
}
