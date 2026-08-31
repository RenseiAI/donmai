package attachclient

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/RenseiAI/donmai/attachclient/attachtest"
	"github.com/RenseiAI/donmai/attachwire"
	"github.com/coder/websocket"
)

// runHostEpoch starts a RunHost host leg with an explicit epoch + jti.
func runHostEpoch(t *testing.T, relay *attachtest.StubRelay, epoch int64, jti string) (*fakeSession, chan error, context.CancelFunc) {
	t.Helper()
	sess := newFakeSession(uint64(epoch)) //nolint:gosec // G115: epoch is a small non-negative test constant
	ctx, cancel := context.WithCancel(context.Background())
	cfg := HostConfig{
		AttachURL:            relay.BaseWSURL(),
		TokenSource:          staticToken(mkHostToken(testSessionID, epoch, jti, true), nil),
		Session:              sess,
		BackoffMin:           5 * time.Millisecond,
		BackoffMax:           30 * time.Millisecond,
		FinalScreenWindow:    100 * time.Millisecond,
		UpgradeProbeInterval: time.Hour,
	}
	done := make(chan error, 1)
	go func() { done <- RunHost(ctx, cfg) }()
	t.Cleanup(cancel)
	return sess, done, cancel
}

func authorityConfirmingResize(t *testing.T) attachwire.Frame {
	t.Helper()
	payload, err := (attachwire.ResizePayload{Cols: 80, Rows: 24}).Encode()
	if err != nil {
		t.Fatalf("encode authority-confirming resize: %v", err)
	}
	return attachwire.Frame{Type: attachwire.TypeResize, Payload: payload}
}

func cancelHostAndWaitUnbound(t *testing.T, relay *attachtest.StubRelay, cancel context.CancelFunc) {
	t.Helper()
	cancel()
	if !waitFor(func() bool { return !relay.HostBound() }, 3*time.Second) {
		t.Fatalf("incumbent host remained bound as %q after cancellation", relay.HostJTI())
	}
}

func TestEpochStaleSameEpochRetriesWithBoundedBackoff(t *testing.T) {
	relay := attachtest.New(attachtest.Config{RoomID: "room-1"})
	if err := relay.Start(); err != nil {
		t.Fatalf("relay start: %v", err)
	}
	t.Cleanup(func() { _ = relay.Close() })

	// A is the incumbent. B has the SAME PTY epoch but a fresh token jti: this
	// is indistinguishable from a legitimate same-process re-sign while A's
	// prior carrier is half-open, so B must retry rather than terminate.
	_, _, cancelA := runHostEpoch(t, relay, 5, "host-A")
	waitBound(t, relay)

	const (
		floor   = 10 * time.Millisecond
		ceiling = 80 * time.Millisecond
	)
	delays := make(chan time.Duration, 16)
	releaseSleep := make(chan struct{}, 16)
	sessB := newFakeSession(5)
	// This frame predates the retry episode. Same-PTY recovery must preserve
	// hasStreamed and never replay it as a fresh generation.
	sessB.PushOutput([]byte("pre-stale"))
	ctxB, cancelB := context.WithCancel(context.Background())
	t.Cleanup(cancelB)
	doneB := make(chan error, 1)
	go func() {
		doneB <- RunHost(ctxB, HostConfig{
			AttachURL:             relay.BaseWSURL(),
			TokenSource:           staticToken(mkHostToken(testSessionID, 5, "host-B", true), nil),
			Session:               sessB,
			BackoffMin:            floor,
			BackoffMax:            ceiling,
			EpochStaleMaxRetries:  8,
			EpochStaleRetryWindow: time.Second,
			FinalScreenWindow:     100 * time.Millisecond,
			UpgradeProbeInterval:  time.Hour,
			epochStaleSleep: func(ctx context.Context, delay time.Duration) error {
				delays <- delay
				select {
				case <-ctx.Done():
					return ctx.Err()
				case <-releaseSleep:
					return nil
				}
			},
		})
	}()

	observed := make([]time.Duration, 0, 3)
	for len(observed) < 3 {
		select {
		case delay := <-delays:
			observed = append(observed, delay)
			if delay <= 0 || delay > ceiling {
				t.Fatalf("epoch-stale delay %d = %v, want (0, %v]", len(observed), delay, ceiling)
			}
			if len(observed) < 3 {
				releaseSleep <- struct{}{}
			}
		case err := <-doneB:
			t.Fatalf("same-epoch client terminated instead of retrying: %v", err)
		case <-time.After(3 * time.Second):
			t.Fatal("timed out waiting for bounded epoch-stale retries")
		}
	}
	if observed[2] <= floor {
		t.Fatalf("third epoch-stale delay = %v, want deeper than floor %v", observed[2], floor)
	}

	// Once the half-open incumbent is gone, the next bounded retry rebinds B.
	cancelHostAndWaitUnbound(t, relay, cancelA)
	releaseSleep <- struct{}{}
	if !waitFor(func() bool { return relay.HostJTI() == "host-B" }, 3*time.Second) {
		t.Fatalf("bound host jti = %q, want host-B after incumbent release", relay.HostJTI())
	}
	if relay.Head() != 0 {
		t.Fatalf("same-epoch rebind replayed pre-stale history (relay head=%d, want 0)", relay.Head())
	}
	select {
	case err := <-doneB:
		t.Fatalf("same-epoch client returned after successful rebind: %v", err)
	default:
	}
}

func TestRingMissClearsOlderEpochStaleBudget(t *testing.T) {
	relay := attachtest.New(attachtest.Config{RoomID: "room-1"})
	if err := relay.Start(); err != nil {
		t.Fatalf("relay start: %v", err)
	}
	t.Cleanup(func() { _ = relay.Close() })
	_, _, cancelA := runHostEpoch(t, relay, 5, "host-A")
	waitBound(t, relay)

	baseNow := time.Unix(2_000, 0)
	var nowNanos atomic.Int64
	nowNanos.Store(baseNow.UnixNano())
	retryEntered := make(chan struct{}, 1)
	releaseRetry := make(chan struct{}, 1)
	var releaseAll atomic.Bool
	sessB := newFakeSession(5)
	ctxB, cancelB := context.WithCancel(context.Background())
	t.Cleanup(cancelB)
	doneB := make(chan error, 1)
	go func() {
		doneB <- RunHost(ctxB, HostConfig{
			AttachURL:              relay.BaseWSURL(),
			TokenSource:            staticToken(mkHostToken(testSessionID, 5, "host-B", true), nil),
			Session:                sessB,
			BackoffMin:             5 * time.Millisecond,
			BackoffMax:             20 * time.Millisecond,
			EpochStaleMaxRetries:   4,
			EpochStaleRetryWindow:  50 * time.Millisecond,
			EpochStaleStableWindow: time.Hour,
			RingMissRetryCeiling:   20 * time.Millisecond,
			UpgradeProbeInterval:   time.Hour,
			now: func() time.Time {
				return time.Unix(0, nowNanos.Load())
			},
			epochStaleSleep: func(ctx context.Context, _ time.Duration) error {
				if releaseAll.Load() {
					return nil
				}
				select {
				case retryEntered <- struct{}{}:
				default:
				}
				select {
				case <-ctx.Done():
					return ctx.Err()
				case <-releaseRetry:
					return nil
				}
			},
		})
	}()

	select {
	case <-retryEntered:
	case err := <-doneB:
		t.Fatalf("same-epoch client terminated before recovery: %v", err)
	case <-time.After(3 * time.Second):
		t.Fatal("same-epoch client did not enter recovery")
	}
	releaseAll.Store(true)
	cancelHostAndWaitUnbound(t, relay, cancelA)
	releaseRetry <- struct{}{}
	if !waitFor(func() bool { return relay.HostJTI() == "host-B" }, 3*time.Second) {
		t.Fatalf("bound host jti = %q, want host-B", relay.HostJTI())
	}
	before := len(sessB.SubscribeSeqs())

	// Move the injected clock past the old stale window, then deliver an
	// authoritative ring-miss control. §13 must clear the stale episode before
	// the next top-level expiry check.
	nowNanos.Store(baseNow.Add(time.Second).UnixNano())
	relay.SendToHost(mustFrame(t, attachwire.ControlError{
		Code: attachwire.CodeRingMiss, Message: "relay ring lost", Retryable: false,
	}))
	if !waitFor(func() bool { return len(sessB.SubscribeSeqs()) > before }, 3*time.Second) {
		t.Fatalf("client did not re-subscribe after ring miss (subscribeSeqs=%v)", sessB.SubscribeSeqs())
	}
	select {
	case err := <-doneB:
		t.Fatalf("older stale budget terminated §13 ring-miss recovery: %v", err)
	default:
	}
}

func TestShortAdmittedCarrierDoesNotResetEpochBudget(t *testing.T) {
	relay := attachtest.New(attachtest.Config{RoomID: "room-1"})
	if err := relay.Start(); err != nil {
		t.Fatalf("relay start: %v", err)
	}
	t.Cleanup(func() { _ = relay.Close() })
	_, _, cancelA := runHostEpoch(t, relay, 5, "host-A")
	waitBound(t, relay)

	baseNow := time.Unix(3_000, 0)
	var nowNanos atomic.Int64
	nowNanos.Store(baseNow.UnixNano())
	retryDelays := make(chan time.Duration, 4)
	releaseRetry := make(chan struct{}, 4)
	sessB := newFakeSession(5)
	ctxB, cancelB := context.WithCancel(context.Background())
	t.Cleanup(cancelB)
	doneB := make(chan error, 1)
	go func() {
		doneB <- RunHost(ctxB, HostConfig{
			AttachURL:              relay.BaseWSURL(),
			TokenSource:            staticToken(mkHostToken(testSessionID, 5, "host-B", true), nil),
			Session:                sessB,
			BackoffMin:             time.Millisecond,
			BackoffMax:             4 * time.Millisecond,
			EpochStaleMaxRetries:   2,
			EpochStaleRetryWindow:  time.Minute,
			EpochStaleStableWindow: 30 * time.Second,
			UpgradeProbeInterval:   time.Hour,
			now: func() time.Time {
				return time.Unix(0, nowNanos.Load())
			},
			epochStaleSleep: func(ctx context.Context, delay time.Duration) error {
				retryDelays <- delay
				select {
				case <-ctx.Done():
					return ctx.Err()
				case <-releaseRetry:
					return nil
				}
			},
		})
	}()

	select {
	case <-retryDelays:
	case err := <-doneB:
		t.Fatalf("client stopped before first retry: %v", err)
	case <-time.After(3 * time.Second):
		t.Fatal("client did not enter first stale retry")
	}
	// Dial/backoff time advances beyond StableWindow BEFORE B is admitted. It
	// must not count as healthy carrier dwell.
	nowNanos.Store(baseNow.Add(31 * time.Second).UnixNano())
	cancelHostAndWaitUnbound(t, relay, cancelA)
	releaseRetry <- struct{}{}
	if !waitFor(func() bool { return relay.HostJTI() == "host-B" }, 3*time.Second) {
		t.Fatalf("bound host jti = %q, want host-B", relay.HostJTI())
	}
	relay.SendToHost(authorityConfirmingResize(t))
	if !waitFor(func() bool { return len(sessB.Resizes()) == 1 }, 3*time.Second) {
		t.Fatal("B did not receive the authority-confirming inbound frame")
	}

	// A genuine epoch-6 successor immediately ends B's newly admitted carrier.
	_, _, cancelC := runHostEpoch(t, relay, 6, "host-C")
	defer cancelC()
	if !waitFor(func() bool { return relay.HostJTI() == "host-C" }, 3*time.Second) {
		t.Fatalf("bound host jti = %q, want host-C", relay.HostJTI())
	}
	select {
	case <-retryDelays:
		releaseRetry <- struct{}{}
	case err := <-doneB:
		t.Fatalf("client stopped before its second configured retry: %v", err)
	case <-time.After(3 * time.Second):
		t.Fatal("client did not enter second stale retry")
	}

	select {
	case delay := <-retryDelays:
		t.Fatalf("short carrier reset epoch budget; observed forbidden third retry delay %v", delay)
	case err := <-doneB:
		if !errors.Is(err, ErrEpochStale) {
			t.Fatalf("budget-exhausted client returned %v, want ErrEpochStale", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("client neither exhausted its budget nor exposed a forbidden third retry")
	}
}

func TestStableAuthorityConfirmedCarrierResetsEpochBudget(t *testing.T) {
	relay := attachtest.New(attachtest.Config{RoomID: "room-1"})
	if err := relay.Start(); err != nil {
		t.Fatalf("relay start: %v", err)
	}
	t.Cleanup(func() { _ = relay.Close() })
	_, _, cancelA := runHostEpoch(t, relay, 5, "host-A")
	waitBound(t, relay)

	baseNow := time.Unix(4_000, 0)
	var nowNanos atomic.Int64
	nowNanos.Store(baseNow.UnixNano())
	retryDelays := make(chan time.Duration, 2)
	releaseRetry := make(chan struct{}, 2)
	sessB := newFakeSession(5)
	ctxB, cancelB := context.WithCancel(context.Background())
	t.Cleanup(cancelB)
	doneB := make(chan error, 1)
	go func() {
		doneB <- RunHost(ctxB, HostConfig{
			AttachURL:              relay.BaseWSURL(),
			TokenSource:            staticToken(mkHostToken(testSessionID, 5, "host-B", true), nil),
			Session:                sessB,
			BackoffMin:             time.Millisecond,
			BackoffMax:             4 * time.Millisecond,
			EpochStaleMaxRetries:   1,
			EpochStaleRetryWindow:  time.Minute,
			EpochStaleStableWindow: 30 * time.Second,
			UpgradeProbeInterval:   time.Hour,
			now: func() time.Time {
				return time.Unix(0, nowNanos.Load())
			},
			epochStaleSleep: func(ctx context.Context, delay time.Duration) error {
				retryDelays <- delay
				select {
				case <-ctx.Done():
					return ctx.Err()
				case <-releaseRetry:
					return nil
				}
			},
		})
	}()

	select {
	case <-retryDelays:
	case err := <-doneB:
		t.Fatalf("client stopped before first retry: %v", err)
	case <-time.After(3 * time.Second):
		t.Fatal("client did not enter initial stale retry")
	}
	cancelHostAndWaitUnbound(t, relay, cancelA)
	releaseRetry <- struct{}{}
	if !waitFor(func() bool { return relay.HostJTI() == "host-B" }, 3*time.Second) {
		t.Fatalf("bound host jti = %q, want host-B", relay.HostJTI())
	}
	relay.SendToHost(authorityConfirmingResize(t))
	if !waitFor(func() bool { return len(sessB.Resizes()) == 1 }, 3*time.Second) {
		t.Fatal("B did not receive the authority-confirming inbound frame")
	}

	// B remains admitted past StableWindow. A later supersession starts a new
	// bounded episode rather than inheriting the exhausted pre-admission one.
	nowNanos.Store(baseNow.Add(31 * time.Second).UnixNano())
	_, _, cancelC := runHostEpoch(t, relay, 6, "host-C")
	defer cancelC()
	if !waitFor(func() bool { return relay.HostJTI() == "host-C" }, 3*time.Second) {
		t.Fatalf("bound host jti = %q, want host-C", relay.HostJTI())
	}
	select {
	case <-retryDelays:
		// The new episode received its first retry: the old budget was reset.
		cancelB()
	case err := <-doneB:
		t.Fatalf("stable confirmed carrier did not reset the old epoch budget: %v", err)
	case <-time.After(3 * time.Second):
		t.Fatal("stable confirmed carrier produced neither a fresh retry nor a terminal result")
	}
}

func TestWSSAuthorityConfirmationRequiresInboundFrame(t *testing.T) {
	inbound, err := attachwire.BuildControlFrame(attachwire.RoomState{State: attachwire.RoomLive})
	if err != nil {
		t.Fatalf("build room_state: %v", err)
	}
	for _, tt := range []struct {
		name        string
		sendInbound bool
		wantConfirm bool
	}{
		{name: "subscribe write alone is not authority", sendInbound: false, wantConfirm: false},
		{name: "inbound relay frame confirms authority", sendInbound: true, wantConfirm: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				conn, acceptErr := websocket.Accept(w, r, &websocket.AcceptOptions{
					Subprotocols:       []string{attachwire.SubprotocolVersion},
					InsecureSkipVerify: true,
				})
				if acceptErr != nil {
					return
				}
				defer conn.CloseNow() //nolint:errcheck // test teardown
				if _, _, readErr := conn.Read(r.Context()); readErr != nil {
					return
				}
				if tt.sendInbound {
					_ = conn.Write(r.Context(), websocket.MessageBinary, inbound.Encode())
				}
				_ = conn.Close(websocket.StatusGoingAway, "test complete")
			}))
			defer srv.Close()

			tok := mkHostToken(testSessionID, 1, "host", true)
			claims, parseErr := parseHostClaims(tok)
			if parseErr != nil {
				t.Fatalf("parse token: %v", parseErr)
			}
			cfg := HostConfig{
				AttachURL:   strings.Replace(srv.URL, "http://", "ws://", 1),
				TokenSource: staticToken(tok, nil),
				Session:     newFakeSession(1),
				HTTPClient:  srv.Client(),
			}
			if defaultsErr := cfg.withDefaults(); defaultsErr != nil {
				t.Fatalf("withDefaults: %v", defaultsErr)
			}
			h := &host{cfg: cfg, log: cfg.Logger}
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			res, _ := h.runWSS(ctx, tok, claims, time.Time{})
			if !res.progressed {
				t.Fatal("WSS subscribe write did not record transport progress")
			}
			if res.authorityConfirmed != tt.wantConfirm {
				t.Errorf("authorityConfirmed = %v, want %v", res.authorityConfirmed, tt.wantConfirm)
			}
			if tt.wantConfirm && res.progressedAt.IsZero() {
				t.Error("confirmed WSS authority has zero progressedAt")
			}
			if !tt.wantConfirm && !res.progressedAt.IsZero() {
				t.Errorf("unconfirmed WSS authority has progressedAt %v", res.progressedAt)
			}
		})
	}
}

func TestEpochStaleDegradedCarrierRetriesAndRebinds(t *testing.T) {
	relay := attachtest.New(attachtest.Config{RoomID: "room-1"})
	if err := relay.Start(); err != nil {
		t.Fatalf("relay start: %v", err)
	}
	t.Cleanup(func() { _ = relay.Close() })

	_, _, cancelA := runHostEpoch(t, relay, 5, "host-A")
	waitBound(t, relay)
	// Existing WSS stays up; new B attempts are forced through the degraded
	// SSE binding, whose 409 must reach the same bounded recovery path.
	relay.SetRefuseWSS(true)

	retryEntered := make(chan struct{}, 1)
	releaseRetry := make(chan struct{}, 1)
	var releaseAllRetries atomic.Bool
	ctxB, cancelB := context.WithCancel(context.Background())
	t.Cleanup(cancelB)
	doneB := make(chan error, 1)
	go func() {
		doneB <- RunHost(ctxB, HostConfig{
			AttachURL:             relay.BaseWSURL(),
			TokenSource:           staticToken(mkHostToken(testSessionID, 5, "host-B-degraded", true), nil),
			Session:               newFakeSession(5),
			FallbackAfterN:        1,
			BackoffMin:            5 * time.Millisecond,
			BackoffMax:            20 * time.Millisecond,
			EpochStaleMaxRetries:  4,
			EpochStaleRetryWindow: time.Second,
			UpgradeProbeInterval:  time.Hour,
			epochStaleSleep: func(ctx context.Context, _ time.Duration) error {
				if releaseAllRetries.Load() {
					return nil
				}
				select {
				case retryEntered <- struct{}{}:
				default:
				}
				select {
				case <-ctx.Done():
					return ctx.Err()
				case <-releaseRetry:
					return nil
				}
			},
		})
	}()

	select {
	case <-retryEntered:
	case err := <-doneB:
		t.Fatalf("degraded same-epoch client terminated instead of retrying: %v", err)
	case <-time.After(3 * time.Second):
		t.Fatal("degraded carrier did not enter epoch-stale recovery")
	}
	releaseAllRetries.Store(true)
	cancelHostAndWaitUnbound(t, relay, cancelA)
	releaseRetry <- struct{}{}
	if !waitFor(func() bool { return relay.HostJTI() == "host-B-degraded" }, 3*time.Second) {
		t.Fatalf("bound host jti = %q, want host-B-degraded", relay.HostJTI())
	}
}

func TestUpgradeProbePropagatesSuccessorAuthority(t *testing.T) {
	h := &host{
		cfg: HostConfig{
			TokenSource: func(context.Context) (string, error) {
				return mkHostToken(testSessionID, 6, "successor", true), nil
			},
			Session:              newFakeSession(5),
			UpgradeProbeInterval: time.Millisecond,
		},
		log:              discardLogger(),
		authoritySet:     true,
		localPTYEpoch:    5,
		authoritySession: testSessionID,
	}
	tokH := &tokenHolder{cur: mkHostToken(testSessionID, 5, "current", true), src: h.validatedToken}
	result := make(chan error, 1)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	go h.upgradeProbe(ctx, tokH, result)

	select {
	case err := <-result:
		if !errors.Is(err, errEpochGrantSuperseded) {
			t.Fatalf("upgrade result = %v, want successor authority error", err)
		}
	case <-ctx.Done():
		t.Fatal("upgrade probe discarded successor authority instead of propagating it")
	}
}

func TestEpochStaleRetryBudgetExhaustionIsTerminal(t *testing.T) {
	relay := attachtest.New(attachtest.Config{RoomID: "room-1"})
	if err := relay.Start(); err != nil {
		t.Fatalf("relay start: %v", err)
	}
	t.Cleanup(func() { _ = relay.Close() })
	_, _, cancelA := runHostEpoch(t, relay, 5, "host-A")
	defer cancelA()
	waitBound(t, relay)

	ctxB, cancelB := context.WithCancel(context.Background())
	t.Cleanup(cancelB)
	doneB := make(chan error, 1)
	go func() {
		doneB <- RunHost(ctxB, HostConfig{
			AttachURL:             relay.BaseWSURL(),
			TokenSource:           staticToken(mkHostToken(testSessionID, 5, "host-B-budget", true), nil),
			Session:               newFakeSession(5),
			BackoffMin:            time.Millisecond,
			BackoffMax:            2 * time.Millisecond,
			EpochStaleMaxRetries:  2,
			EpochStaleRetryWindow: time.Minute,
			UpgradeProbeInterval:  time.Hour,
			epochStaleSleep: func(context.Context, time.Duration) error {
				return nil
			},
		})
	}()

	select {
	case err := <-doneB:
		if !errors.Is(err, ErrEpochStale) {
			t.Fatalf("budget-exhausted client returned %v, want ErrEpochStale", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("epoch-stale recovery did not stop at its configured retry budget")
	}
}

func TestEpochHigherGrantConfirmsNewerProcess(t *testing.T) {
	relay := attachtest.New(attachtest.Config{RoomID: "room-1"})
	if err := relay.Start(); err != nil {
		t.Fatalf("relay start: %v", err)
	}
	t.Cleanup(func() { _ = relay.Close() })

	var currentGrant atomic.Int64
	currentGrant.Store(5)
	sessA := newFakeSession(5)
	ctxA, cancelA := context.WithCancel(context.Background())
	t.Cleanup(cancelA)
	doneA := make(chan error, 1)
	go func() {
		doneA <- RunHost(ctxA, HostConfig{
			AttachURL: relay.BaseWSURL(),
			TokenSource: func(context.Context) (string, error) {
				epoch := currentGrant.Load()
				return mkHostToken(testSessionID, epoch, fmt.Sprintf("host-A-%d", epoch), true), nil
			},
			Session:              sessA,
			BackoffMin:           5 * time.Millisecond,
			BackoffMax:           30 * time.Millisecond,
			FinalScreenWindow:    100 * time.Millisecond,
			UpgradeProbeInterval: time.Hour,
		})
	}()
	waitBound(t, relay)

	// The shared grant rail advances to the genuine successor before epoch 6
	// takes the room. A must never apply that epoch-6 token to its epoch-5 PTY.
	currentGrant.Store(6)
	_, _, cancelC := runHostEpoch(t, relay, 6, "host-C")
	defer cancelC()
	if !waitFor(func() bool { return relay.HostJTI() == "host-C" }, 3*time.Second) {
		t.Fatalf("bound host jti = %q, want host-C", relay.HostJTI())
	}
	select {
	case err := <-doneA:
		if !errors.Is(err, ErrEpochStale) {
			t.Fatalf("superseded host A returned %v, want ErrEpochStale", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("superseded host A did not stop after the grant rail advanced")
	}
}

func TestRunHostRejectsSuccessorGrantBeforeDial(t *testing.T) {
	var requests atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		http.Error(w, "must not dial", http.StatusForbidden)
	}))
	defer srv.Close()

	err := RunHost(context.Background(), HostConfig{
		AttachURL: strings.Replace(srv.URL, "http://", "ws://", 1) + "/v1/rooms/room-1",
		TokenSource: staticToken(
			mkHostToken(testSessionID, 6, "successor", true), nil,
		),
		Session: newFakeSession(5),
	})
	if !errors.Is(err, ErrEpochStale) {
		t.Fatalf("RunHost error = %v, want ErrEpochStale", err)
	}
	if got := requests.Load(); got != 0 {
		t.Fatalf("successor grant reached transport (%d request(s)), want zero", got)
	}
}

func TestDegradedRefreshNeverUsesSuccessorGrant(t *testing.T) {
	const basePath = "/v1/rooms/room-1"
	current := mkHostToken(testSessionID, 5, "current", true)
	successor := mkHostToken(testSessionID, 6, "successor", true)
	var sourceCalls atomic.Int64
	var sseRequests atomic.Int64
	var successorPresented atomic.Bool

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case basePath:
			http.Error(w, "upgrade unavailable", http.StatusNotFound)
		case basePath + "/host/sse":
			sseRequests.Add(1)
			if r.Header.Get("Authorization") == "Bearer "+successor {
				successorPresented.Store(true)
				w.Header().Set("Content-Type", "text/event-stream")
				w.WriteHeader(http.StatusOK)
				w.(http.Flusher).Flush()
				<-r.Context().Done()
				return
			}
			w.WriteHeader(http.StatusUnauthorized)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	err := RunHost(ctx, HostConfig{
		AttachURL: strings.Replace(srv.URL, "http://", "ws://", 1) + basePath,
		TokenSource: func(context.Context) (string, error) {
			if sourceCalls.Add(1) <= 2 {
				return current, nil
			}
			return successor, nil
		},
		Session:              newFakeSession(5),
		FallbackAfterN:       1,
		BackoffMin:           time.Millisecond,
		BackoffMax:           2 * time.Millisecond,
		UpgradeProbeInterval: time.Hour,
	})
	if !errors.Is(err, ErrEpochStale) {
		t.Fatalf("RunHost error = %v, want ErrEpochStale", err)
	}
	if successorPresented.Load() {
		t.Fatal("degraded refresh presented a successor-process grant to the relay")
	}
	if got := sseRequests.Load(); got != 1 {
		t.Fatalf("SSE requests = %d, want exactly one rejected current-process request", got)
	}
}

func TestValidatedTokenUsesLocalPTYEpochGroundTruth(t *testing.T) {
	tests := []struct {
		name      string
		token     string
		tokenErr  error
		wantToken bool
		wantErrIs error
	}{
		{
			name:      "exact local epoch is admitted",
			token:     mkHostToken(testSessionID, 5, "exact", true),
			wantToken: true,
		},
		{
			name:      "lower epoch is ambiguous refresh lag",
			token:     mkHostToken(testSessionID, 4, "behind", true),
			wantErrIs: errEpochGrantAmbiguous,
		},
		{
			name:      "higher epoch proves successor ownership",
			token:     mkHostToken(testSessionID, 6, "successor", true),
			wantErrIs: errEpochGrantSuperseded,
		},
		{
			name:      "foreign session refresh is terminal authority mismatch",
			token:     mkHostToken("foreign-session", 5, "foreign", true),
			wantErrIs: errEpochGrantSuperseded,
		},
		{
			name:     "token source error remains ambiguous",
			tokenErr: errors.New("temporary token read failure"),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := &host{
				cfg: HostConfig{
					TokenSource: func(context.Context) (string, error) {
						return tt.token, tt.tokenErr
					},
					Session: newFakeSession(5),
				},
				log:              discardLogger(),
				authoritySet:     true,
				localPTYEpoch:    5,
				authoritySession: testSessionID,
			}
			got, err := h.validatedToken(context.Background())
			if tt.tokenErr != nil {
				if !errors.Is(err, tt.tokenErr) {
					t.Fatalf("error = %v, want token source error %v", err, tt.tokenErr)
				}
				return
			}
			if tt.wantErrIs != nil {
				if !errors.Is(err, tt.wantErrIs) {
					t.Fatalf("error = %v, want errors.Is(%v)", err, tt.wantErrIs)
				}
				return
			}
			if err != nil {
				t.Fatalf("validatedToken: %v", err)
			}
			if tt.wantToken && got != tt.token {
				t.Fatalf("token = %q, want exact source token", got)
			}
		})
	}
}

func TestValidatedTokenUsesImmutableInitialAuthorityForLegacyZeroSnapshot(t *testing.T) {
	first := mkHostToken(testSessionID, 1, "first", true)
	h := &host{
		cfg: HostConfig{
			InitialAuthorityToken: first,
			TokenSource:           staticToken(first, nil),
			Session:               newFakeSession(0),
		},
		log: discardLogger(),
	}
	got, err := h.validatedToken(context.Background())
	if err != nil {
		t.Fatalf("initial legacy token: %v", err)
	}
	if got != first || h.localPTYEpoch != 1 {
		t.Fatalf("legacy authority = token %q epoch %d, want first token pinned at epoch 1",
			got, h.localPTYEpoch)
	}

	h.cfg.TokenSource = staticToken(mkHostToken(testSessionID, 2, "successor", true), nil)
	if _, err := h.validatedToken(context.Background()); !errors.Is(err, errEpochGrantSuperseded) {
		t.Fatalf("successor refresh error = %v, want errEpochGrantSuperseded", err)
	}
}

func TestLegacyZeroRejectsSuccessorOnFirstLiveRead(t *testing.T) {
	initial := mkHostToken(testSessionID, 1, "initial", true)
	h := &host{
		cfg: HostConfig{
			InitialAuthorityToken: initial,
			TokenSource: staticToken(
				mkHostToken(testSessionID, 2, "successor-already-in-shared-file", true), nil,
			),
			Session: newFakeSession(0),
		},
		log: discardLogger(),
	}
	if _, err := h.validatedToken(context.Background()); !errors.Is(err, errEpochGrantSuperseded) {
		t.Fatalf("first live read error = %v, want errEpochGrantSuperseded", err)
	}
}

func TestLegacyZeroRequiresImmutableInitialAuthority(t *testing.T) {
	h := &host{
		cfg: HostConfig{
			TokenSource: staticToken(mkHostToken(testSessionID, 1, "live", true), nil),
			Session:     newFakeSession(0),
		},
		log: discardLogger(),
	}
	if _, err := h.validatedToken(context.Background()); err == nil ||
		!strings.Contains(err.Error(), "InitialAuthorityToken is required") {
		t.Fatalf("missing initial authority error = %v", err)
	}
}

func TestValidatedTokenRejectsInvalidInitialAuthority(t *testing.T) {
	tests := []struct {
		name          string
		snapshotEpoch uint64
		initialToken  string
		liveToken     string
		wantErrIs     error
		wantContains  string
		wantUnset     bool
	}{
		{
			name:          "first live token cannot replace the immutable session",
			snapshotEpoch: 0,
			initialToken:  mkHostToken(testSessionID, 1, "initial", true),
			liveToken:     mkHostToken("foreign-session", 1, "foreign-live", true),
			wantErrIs:     errEpochGrantSuperseded,
		},
		{
			name:          "initial epoch below a nonzero snapshot is rejected",
			snapshotEpoch: 5,
			initialToken:  mkHostToken(testSessionID, 4, "initial-behind", true),
			liveToken:     mkHostToken(testSessionID, 5, "live-exact", true),
			wantErrIs:     errEpochGrantSuperseded,
			wantUnset:     true,
		},
		{
			name:          "initial epoch above a nonzero snapshot is rejected",
			snapshotEpoch: 5,
			initialToken:  mkHostToken(testSessionID, 6, "initial-ahead", true),
			liveToken:     mkHostToken(testSessionID, 5, "live-exact", true),
			wantErrIs:     errEpochGrantSuperseded,
			wantUnset:     true,
		},
		{
			name:          "malformed initial token cannot fall back to live authority",
			snapshotEpoch: 0,
			initialToken:  "not-a-jwt",
			liveToken:     mkHostToken(testSessionID, 1, "live-valid", true),
			wantContains:  "parsing InitialAuthorityToken",
			wantUnset:     true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := &host{
				cfg: HostConfig{
					InitialAuthorityToken: tt.initialToken,
					TokenSource:           staticToken(tt.liveToken, nil),
					Session:               newFakeSession(tt.snapshotEpoch),
				},
				log: discardLogger(),
			}
			_, err := h.validatedToken(context.Background())
			if tt.wantErrIs != nil && !errors.Is(err, tt.wantErrIs) {
				t.Fatalf("error = %v, want errors.Is(%v)", err, tt.wantErrIs)
			}
			if tt.wantContains != "" && (err == nil || !strings.Contains(err.Error(), tt.wantContains)) {
				t.Fatalf("error = %v, want text %q", err, tt.wantContains)
			}
			if tt.wantUnset && h.authoritySet {
				t.Fatal("invalid initial authority was committed")
			}
		})
	}
}

func TestEpochRetryStateIsFinite(t *testing.T) {
	cfg := HostConfig{
		EpochStaleMaxRetries:  3,
		EpochStaleRetryWindow: time.Minute,
	}
	state := newEpochRetryState(5*time.Millisecond, 20*time.Millisecond)
	now := time.Unix(1_000, 0)
	for attempt := 0; attempt < cfg.EpochStaleMaxRetries; attempt++ {
		delay, ok := state.next(now, cfg)
		if !ok {
			t.Fatalf("attempt %d refused before configured budget", attempt)
		}
		if delay <= 0 || delay > 20*time.Millisecond {
			t.Fatalf("attempt %d delay = %v, want (0, 20ms]", attempt, delay)
		}
	}
	if delay, ok := state.next(now, cfg); ok || delay != 0 {
		t.Fatalf("attempt beyond budget = (%v, %v), want (0, false)", delay, ok)
	}
}

func TestEpochRetryStateExpiresByElapsedWindow(t *testing.T) {
	cfg := HostConfig{
		EpochStaleMaxRetries:  10,
		EpochStaleRetryWindow: 20 * time.Millisecond,
	}
	state := newEpochRetryState(time.Millisecond, 5*time.Millisecond)
	started := time.Unix(1_000, 0)
	if _, ok := state.next(started, cfg); !ok {
		t.Fatal("first retry unexpectedly refused")
	}
	if !state.expired(started.Add(cfg.EpochStaleRetryWindow), cfg) {
		t.Fatal("state.expired returned false at the elapsed-window boundary")
	}
	if delay, ok := state.next(started.Add(cfg.EpochStaleRetryWindow), cfg); ok || delay != 0 {
		t.Fatalf("elapsed-window retry = (%v, %v), want (0, false)", delay, ok)
	}
}

func TestEpochRetryDelayIsClampedToRemainingWindow(t *testing.T) {
	cfg := HostConfig{
		EpochStaleMaxRetries:  10,
		EpochStaleRetryWindow: 15 * time.Millisecond,
	}
	state := newEpochRetryState(10*time.Millisecond, 80*time.Millisecond)
	started := time.Unix(1_000, 0)
	if _, ok := state.next(started, cfg); !ok {
		t.Fatal("first retry unexpectedly refused")
	}
	delay, ok := state.next(started.Add(14*time.Millisecond), cfg)
	if !ok {
		t.Fatal("retry inside the elapsed window unexpectedly refused")
	}
	if delay <= 0 || delay > time.Millisecond {
		t.Fatalf("clamped delay = %v, want (0, 1ms]", delay)
	}
}

func TestEpochStaleRecoveryDefaultsAreFinite(t *testing.T) {
	cfg := HostConfig{
		AttachURL:   "ws://127.0.0.1/v1/rooms/room-1",
		TokenSource: staticToken(mkHostToken(testSessionID, 1, "host", true), nil),
		Session:     newFakeSession(1),
	}
	if err := cfg.withDefaults(); err != nil {
		t.Fatalf("withDefaults: %v", err)
	}
	if cfg.EpochStaleMaxRetries != defaultEpochStaleRetries {
		t.Errorf("EpochStaleMaxRetries = %d, want %d",
			cfg.EpochStaleMaxRetries, defaultEpochStaleRetries)
	}
	if cfg.EpochStaleRetryWindow != defaultEpochStaleWindow {
		t.Errorf("EpochStaleRetryWindow = %v, want %v",
			cfg.EpochStaleRetryWindow, defaultEpochStaleWindow)
	}
	if cfg.EpochStaleStableWindow != defaultEpochStableWindow {
		t.Errorf("EpochStaleStableWindow = %v, want %v",
			cfg.EpochStaleStableWindow, defaultEpochStableWindow)
	}
}
