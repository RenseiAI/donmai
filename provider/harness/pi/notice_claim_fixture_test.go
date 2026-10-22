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

// runNoticeClaimMessageEnd fires the real production extension's captured
// message_end handler for a caller-supplied custom message WITHOUT issuing
// any claim — the peer-forgery and redelivery probe. It models a peer
// AdditionalExtension emitting its own donmai-notice message (or pi
// redelivering a persistence event) and reports what the receipt half did.
// trusted controls DONMAI_NOTICE_CLAIM_TRUST; the handler fires TWICE per
// call so a second stamp would surface as a duplicate.
func runNoticeClaimMessageEnd(t *testing.T, trusted bool, entriesJSON, messageJSON string) noticeClaimFixtureResult {
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

	cmd := exec.Command("node", harness, extPath, entriesJSON, messageJSON) //nolint:gosec // G204: fixed test-only harness path + node binary resolved from PATH; args are this test's own literals.
	cmd.Env = append(os.Environ(),
		"DONMAI_PI_HANDSHAKE=",
		"DONMAI_NOTICE_CLAIM_HARNESS_MODE=message-end",
	)
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

// runNoticeClaimTrustTransition issues the SAME claim twice in one process,
// flipping DONMAI_NOTICE_CLAIM_TRUST between them. The verdict is read per
// event, never cached at activation — so the transition must take effect on
// the second claim. flipToOne selects the flip direction: false starts
// trusted and revokes mid-process, true starts untrusted and stamps late.
func runNoticeClaimTrustTransition(t *testing.T, startTrusted, flipToOne bool, entriesJSON, claimArgs string) noticeClaimFixtureTransitionResult {
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

	flip := "0"
	if flipToOne {
		flip = "1"
	}
	cmd := exec.Command("node", harness, extPath, entriesJSON, claimArgs) //nolint:gosec // G204: fixed test-only harness path + node binary resolved from PATH; args are this test's own literals.
	cmd.Env = append(os.Environ(),
		"DONMAI_PI_HANDSHAKE=",
		"DONMAI_NOTICE_CLAIM_HARNESS_MODE=trust-transition",
		"DONMAI_NOTICE_CLAIM_HARNESS_FLIP="+flip,
	)
	if startTrusted {
		cmd.Env = append(cmd.Env, "DONMAI_NOTICE_CLAIM_TRUST=1")
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("fixture harness failed: %v\nstderr: %s", err, stderr.String())
	}
	var out noticeClaimFixtureTransitionResult
	if err := json.Unmarshal(stdout.Bytes(), &out); err != nil {
		t.Fatalf("decode fixture output %q: %v", stdout.String(), err)
	}
	return out
}

// noticeClaimFixtureTransitionResult is the trust-transition harness report:
// the full append/send/notify streams plus the stream lengths before the
// mid-process trust flip, so the test can isolate the second claim's work.
type noticeClaimFixtureTransitionResult struct {
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
	AppendedBefore int `json:"appendedBefore"`
	SentBefore     int `json:"sentBefore"`
}

// TestNoticeClaimFixture_PeerMessageWithoutOfferMintsNothing is the B1
// forgery proof: a peer AdditionalExtension sharing the process and the
// sendMessage surface emits its own donmai-notice message for a delivery
// the host never offered — the receipt half mints NOTHING, trusted or
// not. The applied receipt exists only after the host extension's own
// offer step persisted that delivery.
//
// RED proof: drop the offer-lookup gate from the message_end handler and
// the forged event appends a donmai-notice-applied receipt.
func TestNoticeClaimFixture_PeerMessageWithoutOfferMintsNothing(t *testing.T) {
	t.Parallel()
	// The reviewer's exact probe: zero offer entries, trusted session.
	msg := `{"role":"custom","customType":"donmai-notice","content":"VICTIM-TEXT","details":{"deliveryId":"D-VICTIM"}}`
	out := runNoticeClaimMessageEnd(t, true, `[]`, msg)
	if !out.Registered {
		t.Fatal("donmai-notice-claim extension did not register its handlers")
	}
	if len(out.Appended) != 0 {
		t.Fatalf("peer message without offer minted %+v, want no receipt", out.Appended)
	}
	if len(out.Sent) != 0 {
		t.Fatalf("peer message probe sent %+v, want nothing sent", out.Sent)
	}
}

// TestNoticeClaimFixture_UntrustedMessageEndMintsNothing is the B1 second
// half: even for a delivery the host DID offer, the receipt half mints
// nothing without the spawn-stamped trust verdict. The command half and
// the receipt half consult the same verdict.
//
// RED proof: drop the isTrusted gate from the message_end handler and the
// untrusted event appends the receipt.
func TestNoticeClaimFixture_UntrustedMessageEndMintsNothing(t *testing.T) {
	t.Parallel()
	offer := `{"type":"custom","customType":"donmai-notice-offer","data":{"delivery":"D1","text":"HELLO"}}`
	msg := `{"role":"custom","customType":"donmai-notice","content":"HELLO","details":{"deliveryId":"D1"}}`
	out := runNoticeClaimMessageEnd(t, false, `[`+offer+`]`, msg)
	if !out.Registered {
		t.Fatal("donmai-notice-claim extension did not register its handlers")
	}
	if len(out.Appended) != 0 {
		t.Fatalf("untrusted message_end minted %+v, want no receipt", out.Appended)
	}
}

// TestNoticeClaimFixture_RedeliveredMessageEndStaysExactlyOne is the B2
// idempotence proof: the harness fires message_end TWICE for the same
// persisted message (crash between application and marker, transport
// replay) — the receipt stream must hold exactly one applied entry, never
// two.
//
// RED proof: drop the appliedReceipt early-return from the message_end
// handler and the redelivery appends a second receipt.
func TestNoticeClaimFixture_RedeliveredMessageEndStaysExactlyOne(t *testing.T) {
	t.Parallel()
	offer := `{"type":"custom","customType":"donmai-notice-offer","data":{"delivery":"D1","text":"HELLO"}}`
	msg := `{"role":"custom","customType":"donmai-notice","content":"HELLO","details":{"deliveryId":"D1"}}`
	out := runNoticeClaimMessageEnd(t, true, `[`+offer+`]`, msg)
	if !out.Registered {
		t.Fatal("donmai-notice-claim extension did not register its handlers")
	}
	if len(out.Appended) != 1 {
		t.Fatalf("redelivered message_end appended %+v, want exactly one receipt", out.Appended)
	}
	if out.Appended[0].CustomType != "donmai-notice-applied" {
		t.Errorf("append = %q, want the single applied receipt", out.Appended[0].CustomType)
	}
	if delivery, _ := out.Appended[0].Data["delivery"].(string); delivery != "D1" {
		t.Errorf("receipt delivery = %q, want D1", delivery)
	}
}

// TestNoticeClaimFixture_MismatchedPayloadMintsNothing pins the text
// binding: an offer for (D1, HELLO) plus a persistence event carrying
// different text mints nothing — a peer cannot launder its own payload
// through the host's offer for the same delivery ID.
//
// RED proof: drop the text-equality check from the message_end handler
// and the mismatched event appends the receipt.
func TestNoticeClaimFixture_MismatchedPayloadMintsNothing(t *testing.T) {
	t.Parallel()
	offer := `{"type":"custom","customType":"donmai-notice-offer","data":{"delivery":"D1","text":"HELLO"}}`
	msg := `{"role":"custom","customType":"donmai-notice","content":"PEER-TEXT","details":{"deliveryId":"D1"}}`
	out := runNoticeClaimMessageEnd(t, true, `[`+offer+`]`, msg)
	if !out.Registered {
		t.Fatal("donmai-notice-claim extension did not register its handlers")
	}
	if len(out.Appended) != 0 {
		t.Fatalf("mismatched-payload event minted %+v, want no receipt", out.Appended)
	}
}

// TestNoticeClaimFixture_MalformedOfferEntryCannotWedgeClaim is the F1
// robustness proof: a dataless session entry (same-user writable state a
// peer can plant) is skipped, never fatal — the claim proceeds down the
// normal offer path instead of surfacing a TypeError.
//
// RED proof: drop the data guard from findEntry and the claim notifies a
// TypeError instead of claiming.
func TestNoticeClaimFixture_MalformedOfferEntryCannotWedgeClaim(t *testing.T) {
	t.Parallel()
	entries := `[{"type":"custom","customType":"donmai-notice-offer"}]`
	out := runNoticeClaimFixture(t, true, entries, `D1 HELLO`)
	if !out.Registered {
		t.Fatal("donmai-claim command was not registered")
	}
	if len(out.Notified) != 1 || out.Notified[0].Msg != "claimed D1" {
		t.Fatalf("notified = %+v, want exactly [claimed D1] past the dataless entry", out.Notified)
	}
	for _, n := range out.Notified {
		if strings.Contains(n.Msg, "TypeError") {
			t.Fatalf("malformed entry wedged the channel with %q", n.Msg)
		}
	}
	if len(out.Sent) != 1 {
		t.Fatalf("sent = %+v, want exactly one application", out.Sent)
	}
}

// TestNoticeClaimFixture_TrustRevocationTakesEffectImmediately is the F2
// per-event proof: the verdict is read on every invocation, never cached
// at activation. Revoking it mid-process — a peer mutating the shared
// env, or a session activated before the stamp — refuses the very next
// claim. The trust gate precedes the idempotence check, so the refusal
// holds even for a repeat claim of the just-applied delivery: an
// untrusted caller learns nothing further, and the second claim writes
// nothing.
//
// RED proof: cache the verdict once at activate time and the post-flip
// claim still applies.
func TestNoticeClaimFixture_TrustRevocationTakesEffectImmediately(t *testing.T) {
	t.Parallel()
	out := runNoticeClaimTrustTransition(t, true, false, `[]`, `D2 HELLO`)
	if !out.Registered {
		t.Fatal("donmai-claim command was not registered")
	}
	if len(out.Notified) != 2 {
		t.Fatalf("notified = %+v, want the initial claim plus the post-flip refusal", out.Notified)
	}
	if out.Notified[0].Msg != "claimed D2" {
		t.Fatalf("first notify = %+v, want the initial claim to succeed while trusted", out.Notified)
	}
	if last := out.Notified[1]; !strings.Contains(last.Msg, "not trusted") {
		t.Fatalf("post-revocation notify = %+v, want the not-trusted refusal", out.Notified)
	}
	if len(out.Appended) != out.AppendedBefore || len(out.Sent) != out.SentBefore {
		t.Fatalf("post-revocation claim wrote %d appends / %d sends past the flip (was %d / %d), want nothing written",
			len(out.Appended), len(out.Sent), out.AppendedBefore, out.SentBefore)
	}
}

// TestNoticeClaimFixture_LateTrustStampEnablesClaim is the F2 converse:
// an untrusted session that later receives the stamp — activation
// predating the spawn's verdict — can then claim. Per-event reads cut
// both ways: no stale refusal either.
func TestNoticeClaimFixture_LateTrustStampEnablesClaim(t *testing.T) {
	t.Parallel()
	out := runNoticeClaimTrustTransition(t, false, true, `[]`, `D2 HELLO`)
	if !out.Registered {
		t.Fatal("donmai-claim command was not registered")
	}
	if len(out.Notified) != 2 {
		t.Fatalf("notified = %+v, want the initial refusal plus the post-stamp claim", out.Notified)
	}
	if !strings.Contains(out.Notified[0].Msg, "not trusted") {
		t.Fatalf("first notify = %+v, want the not-trusted refusal while untrusted", out.Notified)
	}
	if out.AppendedBefore != 0 || out.SentBefore != 0 {
		t.Fatalf("pre-stamp claim wrote %d appends / %d sends, want nothing written while untrusted",
			out.AppendedBefore, out.SentBefore)
	}
	if last := out.Notified[1]; last.Msg != "claimed D2" {
		t.Fatalf("post-stamp notify = %+v, want exactly [claimed D2]", out.Notified)
	}
}

// TestNoticeClaimFixture_TextlessPlantedOfferMintsNothing is the B1
// both-sides-content proof: a peer plants a textless offer entry through
// the same-user session file, then fires a contentless persistence event
// for it. The receipt half mints NOTHING: the event carries no text and
// the offer binds no text, and the receipt requires both.
//
// RED proof: restore the one-sided gate (require text only when both
// sides carry it) and the contentless event mints the applied receipt.
func TestNoticeClaimFixture_TextlessPlantedOfferMintsNothing(t *testing.T) {
	t.Parallel()
	entries := `[{"type":"custom","customType":"donmai-notice-offer","data":{"delivery":"D1"}}]`
	msg := `{"role":"custom","customType":"donmai-notice","details":{"deliveryId":"D1"}}`
	out := runNoticeClaimMessageEnd(t, true, entries, msg)
	if !out.Registered {
		t.Fatal("donmai-notice-claim extension did not register its handlers")
	}
	if len(out.Appended) != 0 {
		t.Fatalf("textless planted offer minted %+v, want no receipt", out.Appended)
	}
}

// TestNoticeClaimFixture_TextlessPlantedOfferCannotBlockLegitimateClaim
// is the B1 claim-half proof: the same textless planted offer must not
// wedge the claim channel into a conflicting-payload refusal. The
// legitimate claim proceeds down the normal offer path — persisting its
// own binding offer and applying exactly once.
//
// RED proof: look up any persisted offer (including a textless one) in
// the claim path and the legitimate claim is refused as a conflict.
func TestNoticeClaimFixture_TextlessPlantedOfferCannotBlockLegitimateClaim(t *testing.T) {
	t.Parallel()
	entries := `[{"type":"custom","customType":"donmai-notice-offer","data":{"delivery":"D1"}}]`
	out := runNoticeClaimFixture(t, true, entries, `D1 HELLO`)
	if !out.Registered {
		t.Fatal("donmai-claim command was not registered")
	}
	if len(out.Notified) != 1 || out.Notified[0].Msg != "claimed D1" {
		t.Fatalf("notified = %+v, want exactly [claimed D1] past the textless entry", out.Notified)
	}
	if len(out.Sent) != 1 {
		t.Fatalf("sent = %+v, want exactly one application", out.Sent)
	}
	var offers int
	for _, a := range out.Appended {
		if a.CustomType == "donmai-notice-offer" {
			offers++
			if text, _ := a.Data["text"].(string); text != "HELLO" {
				t.Fatalf("rebound offer text = %q, want the legitimate payload HELLO", text)
			}
		}
	}
	if offers != 1 {
		t.Fatalf("appended = %+v, want exactly one rebound binding offer", out.Appended)
	}
}

// TestNoticeClaimFixture_ContentlessEventMintsNothing is the B1 event-half
// proof: even with a valid persisted offer, a persistence event carrying
// no content (the shape the runtime normalizes a null content to) mints
// nothing. The receipt attests persisted content, not a bare delivery ID.
//
// RED proof: drop the non-empty-text requirement on the receipt half and
// the contentless event mints the applied receipt.
func TestNoticeClaimFixture_ContentlessEventMintsNothing(t *testing.T) {
	t.Parallel()
	offer := `{"type":"custom","customType":"donmai-notice-offer","data":{"delivery":"D1","text":"HELLO"}}`
	msg := `{"role":"custom","customType":"donmai-notice","content":null,"details":{"deliveryId":"D1"}}`
	out := runNoticeClaimMessageEnd(t, true, `[`+offer+`]`, msg)
	if !out.Registered {
		t.Fatal("donmai-notice-claim extension did not register its handlers")
	}
	if len(out.Appended) != 0 {
		t.Fatalf("contentless event minted %+v, want no receipt", out.Appended)
	}
}

// TestNoticeClaimFixture_CrashBetweenApplyAndMarkerReplaysReceipt is the
// B2 exact-replay proof: the session holds the valid offer AND the
// persisted donmai-notice message (what pi's sendMessage path wrote before
// emitting message_end) but no applied marker — the crash window. The
// reclaim appends the applied receipt ONLY and reports "already applied":
// no duplicate turn, and the receipt still follows durable conversation
// persistence because the persisted message IS that persistence.
//
// RED proof: never look for the persisted message in the claim path and
// the reclaim re-applies a second custom message instead of replaying.
func TestNoticeClaimFixture_CrashBetweenApplyAndMarkerReplaysReceipt(t *testing.T) {
	t.Parallel()
	entries := `[{"type":"custom","customType":"donmai-notice-offer","data":{"delivery":"D1","text":"HELLO"}},` +
		`{"type":"custom_message","customType":"donmai-notice","content":"HELLO","details":{"deliveryId":"D1"},"display":true}]`
	out := runNoticeClaimFixture(t, true, entries, `D1 HELLO`)
	if !out.Registered {
		t.Fatal("donmai-claim command was not registered")
	}
	if len(out.Sent) != 0 {
		t.Fatalf("crash-window reclaim sent %+v, want no re-application", out.Sent)
	}
	if len(out.Appended) != 1 || out.Appended[0].CustomType != "donmai-notice-applied" {
		t.Fatalf("appended = %+v, want exactly the replayed applied receipt", out.Appended)
	}
	if delivery, _ := out.Appended[0].Data["delivery"].(string); delivery != "D1" {
		t.Errorf("receipt delivery = %q, want D1", delivery)
	}
	if len(out.Notified) != 1 || out.Notified[0].Msg != "already applied D1" {
		t.Errorf("notified = %+v, want exactly [already applied D1]", out.Notified)
	}
}

// TestNoticeClaimFixture_CrashWindowBindsPersistedText pins the
// crash-window replay's text binding: the session holds the valid offer
// for (D1, HELLO) but the persisted message carries a peer's different
// text. The reclaim must re-apply HELLO — never replay the receipt off
// the peer's message.
//
// RED proof: drop the text equality from findAppliedMessage and the
// reclaim appends the applied receipt with no send ("already applied").
func TestNoticeClaimFixture_CrashWindowBindsPersistedText(t *testing.T) {
	t.Parallel()
	entries := `[{"type":"custom","customType":"donmai-notice-offer","data":{"delivery":"D1","text":"HELLO"}},` +
		`{"type":"custom_message","customType":"donmai-notice","content":"PEER-TEXT","details":{"deliveryId":"D1"},"display":true}]`
	out := runNoticeClaimFixture(t, true, entries, `D1 HELLO`)
	if !out.Registered {
		t.Fatal("donmai-claim command was not registered")
	}
	if len(out.Sent) != 1 {
		t.Fatalf("mismatched crash-window message sent %+v, want exactly one re-application of the offered text", out.Sent)
	}
	if len(out.Notified) != 1 || out.Notified[0].Msg != "claimed D1" {
		t.Fatalf("notified = %+v, want exactly [claimed D1], not the crash-window replay", out.Notified)
	}
}

// TestNoticeClaimFixture_NonCustomMessageEndMintsNothing pins the
// message_end shape filter: only a persisted custom donmai-notice message
// mints the receipt. A same-delivery event with the wrong role or the
// wrong custom type mints nothing, trusted or not.
//
// RED proof: drop the role/customType filter from the message_end
// handler and either event appends the applied receipt.
func TestNoticeClaimFixture_NonCustomMessageEndMintsNothing(t *testing.T) {
	t.Parallel()
	offer := `{"type":"custom","customType":"donmai-notice-offer","data":{"delivery":"D1","text":"HELLO"}}`
	for _, tc := range []struct {
		name string
		msg  string
	}{
		{name: "wrong role", msg: `{"role":"assistant","customType":"donmai-notice","content":"HELLO","details":{"deliveryId":"D1"}}`},
		{name: "wrong type", msg: `{"role":"custom","customType":"donmai-notice-other","content":"HELLO","details":{"deliveryId":"D1"}}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			out := runNoticeClaimMessageEnd(t, true, `[`+offer+`]`, tc.msg)
			if !out.Registered {
				t.Fatal("donmai-claim extension did not register its handlers")
			}
			if len(out.Appended) != 0 {
				t.Fatalf("%s event minted %+v, want no receipt", tc.name, out.Appended)
			}
		})
	}
}

// TestNoticeClaimFixture_DatalessAppliedEntryCannotWedgeClaim pins the
// findEntry malformed-entry guard on the applied-receipt path: a dataless
// applied entry (same-user writable state a peer can plant) is skipped,
// never fatal — the claim proceeds down the normal offer path instead of
// surfacing a TypeError that would wedge every delivery.
//
// RED proof: drop the data guard from findEntry and the claim notifies a
// TypeError instead of claiming.
func TestNoticeClaimFixture_DatalessAppliedEntryCannotWedgeClaim(t *testing.T) {
	t.Parallel()
	entries := `[{"type":"custom","customType":"donmai-notice-applied"}]`
	out := runNoticeClaimFixture(t, true, entries, `D1 HELLO`)
	if !out.Registered {
		t.Fatal("donmai-claim command was not registered")
	}
	if len(out.Notified) != 1 || out.Notified[0].Msg != "claimed D1" {
		t.Fatalf("notified = %+v, want exactly [claimed D1] past the dataless applied entry", out.Notified)
	}
	for _, n := range out.Notified {
		if strings.Contains(n.Msg, "TypeError") {
			t.Fatalf("dataless applied entry wedged the channel with %q", n.Msg)
		}
	}
	if len(out.Sent) != 1 {
		t.Fatalf("sent = %+v, want exactly one application", out.Sent)
	}
}

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
