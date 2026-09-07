package attachclient

// These tests pin the client half of the planned-restart contract: a relay that
// says it is going away on purpose must never end a dial loop, on either lane.
//
// The shape they defend against is specific and has happened: a deploy restarts
// the single relay process, every host carrier drops at the same instant, and a
// client that reads the drop as "the box died" condemns a session whose PTY it
// still owns and whose relay is seconds away.

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/RenseiAI/donmai/attachclient/attachtest"
	"github.com/RenseiAI/donmai/attachwire"
	attachwirev2 "github.com/RenseiAI/donmai/attachwire/v2"
	"github.com/coder/websocket"
)

func TestParsesTheFrozenRestartGrammar(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name    string
		message string
		want    time.Duration
	}{
		{name: "the frozen grammar", message: "redial after 5s", want: 5 * time.Second},
		{name: "a single second", message: "redial after 1s", want: time.Second},
		{name: "surrounding space is tolerated", message: " redial after 12s ", want: 12 * time.Second},
		{name: "zero is not a floor", message: "redial after 0s"},
		{name: "no unit", message: "redial after 5"},
		{name: "a duration string is not the grammar", message: "redial after 5000ms"},
		{name: "prose", message: "please come back later"},
		{name: "empty", message: ""},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			got, ok := parseRedialHint(test.message)
			if got != test.want || ok != (test.want != 0) {
				t.Fatalf("parseRedialHint(%q) = %v, %v; want %v", test.message, got, ok, test.want)
			}
		})
	}
}

func TestClassifiesEveryPlaceTheRestartIsAnnounced(t *testing.T) {
	t.Parallel()
	t.Run("the drain-window dial refusal", func(t *testing.T) {
		t.Parallel()
		for _, test := range []struct {
			name       string
			status     int
			retryAfter string
			want       time.Duration
			classified bool
		}{
			{name: "503 with a floor", status: http.StatusServiceUnavailable, retryAfter: "5", want: 5 * time.Second, classified: true},
			// A BARE 503 is the relay's OTHER 503: a durable rail that cannot
			// take its journal lock, or an intermediary with no capacity. Both
			// are answers about this candidate or this hop, not an announcement
			// nobody made — and on the host lane, classifying them as a restart
			// would suppress the §14 fallback for a condition the fallback lane
			// may not share.
			{name: "a bare 503 is not an announcement", status: http.StatusServiceUnavailable},
			{
				name: "503 with an HTTP-date this client does not read", status: http.StatusServiceUnavailable,
				retryAfter: "Wed, 02 Sep 2026 10:41:53 GMT",
			},
			{name: "503 with an unreadable Retry-After", status: http.StatusServiceUnavailable, retryAfter: "soon"},
			{name: "503 asking for zero seconds is no floor at all", status: http.StatusServiceUnavailable, retryAfter: "0"},
			{name: "429 is rate limiting, not a restart", status: http.StatusTooManyRequests, retryAfter: "5"},
			{name: "500 is not an announcement", status: http.StatusInternalServerError},
		} {
			t.Run(test.name, func(t *testing.T) {
				t.Parallel()
				resp := &http.Response{StatusCode: test.status, Header: http.Header{}}
				if test.retryAfter != "" {
					resp.Header.Set("Retry-After", test.retryAfter)
				}
				restart := RelayRestartRefusal(resp)
				if (restart != nil) != test.classified {
					t.Fatalf("RelayRestartRefusal(%d) = %v, classified want %v", test.status, restart, test.classified)
				}
				if !test.classified {
					return
				}
				if restart.RedialAfter != test.want {
					t.Fatalf("redial floor = %v, want %v", restart.RedialAfter, test.want)
				}
				if !IsRelayUnavailable(restart) || !IsRelayRestarting(restart) {
					t.Fatalf("%v is not classified as an unavailable relay announcing a restart", restart)
				}
				if isRelayStop(restart) {
					t.Fatalf("%v classified as terminal — a drain-window refusal never is", restart)
				}
			})
		}
	})

	t.Run("the close that ends the leg", func(t *testing.T) {
		t.Parallel()
		for _, test := range []struct {
			name       string
			code       websocket.StatusCode
			reason     string
			want       time.Duration
			classified bool
		}{
			{
				name: "the frozen reason with 1012", code: websocket.StatusServiceRestart,
				reason: "relay-restarting: redial after 5s", want: 5 * time.Second, classified: true,
			},
			{
				name: "1012 alone is a planned restart by definition", code: websocket.StatusServiceRestart,
				reason: "", classified: true,
			},
			{
				name: "the reason names it even under another code", code: websocket.StatusGoingAway,
				reason: "relay-restarting: redial after 7s", want: 7 * time.Second, classified: true,
			},
			{
				name: "an ordinary abnormal close is not an announcement", code: websocket.StatusAbnormalClosure,
				reason: "connection reset",
			},
			{name: "a policy close is not an announcement", code: websocket.StatusPolicyViolation, reason: "epoch-stale"},
		} {
			t.Run(test.name, func(t *testing.T) {
				t.Parallel()
				// Wrapped, the way a carrier's own %w wrapping delivers it.
				err := fmt.Errorf("reading the leg: %w", websocket.CloseError{Code: test.code, Reason: test.reason})
				restart := relayRestartClose(err)
				if (restart != nil) != test.classified {
					t.Fatalf("relayRestartClose(%v) = %v, classified want %v", err, restart, test.classified)
				}
				if !test.classified {
					return
				}
				if restart.RedialAfter != test.want {
					t.Fatalf("redial floor = %v, want %v", restart.RedialAfter, test.want)
				}
				hint, ok := RelayRedialAfter(fmt.Errorf("composing caller: %w", restart))
				if !ok || hint != test.want {
					t.Fatalf("RelayRedialAfter through wrapping = %v, %v; want %v", hint, ok, test.want)
				}
			})
		}
	})
}

// TestPlannedRelayRestartNeverTerminatesTheHostLeg is the attach-v1 half, end to
// end against a relay that runs the WHOLE drain: it announces with the §7
// control, closes the leg with 1012 and the frozen reason, and refuses every
// dial with 503 + Retry-After until the replacement is up.
//
// The v1 lane already survived the drop — it soft-ignored the announcement and
// took the close as an ordinary transient — so "RunHost did not return" is not
// the discriminating property here. The one that is: a client that READS the
// announcement waits out the floor the relay named, while one that does not
// hammers the drain window on its own 5–50ms backoff. This asserts the refusal
// count the relay actually saw.
func TestPlannedRelayRestartNeverTerminatesTheHostLeg(t *testing.T) {
	// BackoffMax is the ceiling the honoured floor is clamped to (see
	// TestAnAbsurdRetryAfterCannotParkTheCarrier), so it must be above the floor
	// this test asserts is honoured.
	h := startHost(t, attachtest.Config{}, func(c *HostConfig) {
		c.BackoffMax = 5 * time.Second
	})
	h.sess.PushOutput([]byte("boot"))
	waitBound(t, h.relay)
	if !waitFor(func() bool { return h.relay.Head() >= 1 }, 3*time.Second) {
		t.Fatalf("initial WSS frame never delivered (head=%d)", h.relay.Head())
	}

	const floor = 2 * time.Second
	h.relay.AnnounceRestart(floor)

	// The leg is closed and every re-dial is refused — and none of that is a
	// reason to stop.
	select {
	case err := <-h.done:
		t.Fatalf("RunHost terminated on a planned relay restart (%v); it must wait out the floor and re-dial", err)
	case <-time.After(floor / 3):
	}
	if h.relay.HostBound() {
		t.Fatal("the host leg survived a 1012 close; the drain never dropped it")
	}
	// One dial may already have been in flight when the drain began; a second
	// would mean the announced floor was ignored, because the floor has not yet
	// elapsed. Without the fix this is the client's own backoff and counts well
	// into double figures.
	if refused := h.relay.RefusedDials(); refused > 1 {
		t.Fatalf("the drain window turned away %d dials inside the first %v of a %v floor — the relay's floor was ignored",
			refused, floor/3, floor)
	}

	h.relay.EndRestart()

	if !waitFor(func() bool {
		h.sess.PushOutput([]byte("after-restart"))
		return h.relay.HostBound() && h.relay.Head() >= 2
	}, 10*time.Second) {
		t.Fatalf("the host never came back after the restart ended (bound=%v head=%d)", h.relay.HostBound(), h.relay.Head())
	}
	select {
	case err := <-h.done:
		t.Fatalf("RunHost returned %v after recovering from a planned restart", err)
	default:
	}
}

// serveRestartingV2Leg answers one v2 dial the way a draining relay answers the
// leg it is about to drop. sendControl chooses which half of the announcement
// this leg gets: today an attach-v2 leg is sent the close ONLY, because a v2
// client that treated the control as terminal made sending it strictly worse
// than the EOF it replaced — the very thing this change removes.
func serveRestartingV2Leg(ctx context.Context, conn *websocket.Conn, sendControl bool, redialAfter int) error {
	frame, _, err := readV2TestFrame(ctx, conn)
	if err != nil {
		return err
	}
	message, err := v2ControlFromFrame(frame)
	if err != nil || message.ControlType() != attachwire.CtrlSubscribe {
		return fmt.Errorf("first frame = %T/%v", message, err)
	}
	hint := fmt.Sprintf("redial after %ds", redialAfter)
	if sendControl {
		control, buildErr := attachwirev2.BuildControlFrame(attachwire.ControlError{
			Code: attachwire.CodeRelayRestarting, Message: hint, Retryable: true,
		})
		if buildErr != nil {
			return buildErr
		}
		if err := conn.Write(ctx, websocket.MessageBinary, control.Encode()); err != nil {
			return err
		}
	}
	// Best effort: a client that has already acted on the control half may close
	// first, and that race is the announcement working, not a relay fault.
	_ = conn.Close(websocket.StatusServiceRestart, "relay-restarting: "+hint)
	return nil
}

// serveAcceptedV2Candidate answers one v2 dial the way the replacement relay
// does: mandatory Snapshot request, then carrier_active at the exact cursor.
func serveAcceptedV2Candidate(ctx context.Context, conn *websocket.Conn, snapshot attachwire.Frame) error {
	frame, _, err := readV2TestFrame(ctx, conn)
	if err != nil {
		return err
	}
	message, err := v2ControlFromFrame(frame)
	if err != nil || message.ControlType() != attachwire.CtrlSubscribe {
		return fmt.Errorf("first frame = %T/%v", message, err)
	}
	request, err := attachwirev2.BuildControlFrame(attachwire.SnapshotRequest{Reason: attachwire.ReasonResync})
	if err != nil {
		return err
	}
	if err := conn.Write(ctx, websocket.MessageBinary, request.Encode()); err != nil {
		return err
	}
	if _, raw, readErr := readV2TestFrame(ctx, conn); readErr != nil || string(raw) != string(snapshot.Encode()) {
		return fmt.Errorf("candidate Snapshot raw mismatch: %v", readErr)
	}
	frame, _, err = readV2TestFrame(ctx, conn)
	message, controlErr := v2ControlFromFrame(frame)
	if err != nil || controlErr != nil || message.ControlType() != attachwirev2.CtrlCarrierActivate {
		return fmt.Errorf("activation frame = %T/%v/%v", message, err, controlErr)
	}
	active, err := attachwirev2.BuildControlFrame(attachwirev2.CarrierActive{
		PTYEpoch: 3, CarrierEpoch: 9, AckSeq: attachwirev2.DecimalUint64(snapshot.Seq),
	})
	if err != nil {
		return err
	}
	return conn.Write(ctx, websocket.MessageBinary, active.Encode())
}

// dialV2UntilTerminal is the composing daemon's discipline in miniature: dial,
// run, and stop only on an answer that is actually terminal. It is the loop the
// contract says a relay-restarting classification must not end, so the test
// drives the real classification through the real loop shape rather than
// asserting on an error type in isolation.
func dialV2UntilTerminal(ctx context.Context, t *testing.T, cfg V2HostConfig, snapshot attachwire.Frame, budget int) (dials int, err error) {
	t.Helper()
	for dials = 1; dials <= budget; dials++ {
		candidate, dialErr := DialV2HostCandidate(ctx, cfg)
		if dialErr != nil {
			err = dialErr
		} else {
			err = runV2CandidateToActivation(ctx, candidate, snapshot)
			_ = candidate.Close()
		}
		switch {
		case err == nil:
			return dials, nil
		case isRelayStop(err):
			// A terminal refusal ends the loop. This is the branch a
			// relay-restarting answer used to fall into.
			return dials, err
		case IsRelayUnavailable(err):
			continue
		default:
			return dials, err
		}
	}
	return dials - 1, err
}

func runV2CandidateToActivation(ctx context.Context, candidate *V2HostCandidate, snapshot attachwire.Frame) error {
	if _, err := candidate.WaitMandatorySnapshotRequest(ctx); err != nil {
		return err
	}
	if err := candidate.SendCandidateSnapshot(ctx, snapshot.Encode()); err != nil {
		return err
	}
	ack, err := candidate.Activate(ctx)
	if err != nil {
		return err
	}
	if ack != snapshot.Seq {
		return fmt.Errorf("carrier activated at %d, want %d", ack, snapshot.Seq)
	}
	return nil
}

// TestV2CarrierRedialsAPlannedRestartInsteadOfEndingTheDialLoop is the attach-v2
// half. Its error-control branch mapped EVERY code to a terminal stop, so the
// announcement the contract added would have ENDED the dial loop outright —
// strictly worse than the bare EOF it replaces, which is why the relay withheld
// it from v2 legs until this landed.
func TestV2CarrierRedialsAPlannedRestartInsteadOfEndingTheDialLoop(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name        string
		sendControl bool
	}{
		{name: "the 1012 close alone, which is all a v2 leg is sent today"},
		{name: "the error control the relay may send once this ships", sendControl: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			snapshot := v2ResumeSnapshot(5)
			serverErr := make(chan error, 2)
			dials := make(chan int, 1)
			dials <- 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{
					Subprotocols: []string{attachwirev2.SubprotocolVersion},
				})
				if err != nil {
					serverErr <- err
					return
				}
				defer conn.CloseNow() //nolint:errcheck
				ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
				defer cancel()
				dial := <-dials + 1
				dials <- dial
				if dial == 1 {
					serverErr <- serveRestartingV2Leg(ctx, conn, test.sendControl, 3)
					return
				}
				serverErr <- serveAcceptedV2Candidate(ctx, conn, snapshot)
			}))
			t.Cleanup(server.Close)

			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			cfg := V2HostConfig{
				AttachURL:        strings.Replace(server.URL, "http://", "ws://", 1) + "/v2/rooms/session-v2",
				TokenSource:      func(context.Context) (string, error) { return v2TestToken(t, nil), nil },
				DurableHighWater: 4,
			}

			// The first leg on its own: its answer must be the never-terminal
			// signal carrying the relay's floor.
			candidate, err := DialV2HostCandidate(ctx, cfg)
			if err != nil {
				t.Fatalf("first dial: %v", err)
			}
			_, err = candidate.WaitMandatorySnapshotRequest(ctx)
			_ = candidate.Close()
			if isRelayStop(err) {
				t.Fatalf("a planned restart ended the v2 leg terminally (%v); the dial loop must survive it", err)
			}
			var restart *RelayRestartingError
			if !errors.As(err, &restart) {
				t.Fatalf("first leg err = %v (%T), want *RelayRestartingError", err, err)
			}
			if restart.RedialAfter != 3*time.Second {
				t.Fatalf("redial floor = %v, want 3s — the relay's own hint is the next dial's floor", restart.RedialAfter)
			}
			if !IsRelayUnavailable(err) {
				t.Fatalf("%v is not classified as an unavailable relay", err)
			}

			// And through the loop: the re-dial happens and is accepted.
			spent, loopErr := dialV2UntilTerminal(ctx, t, cfg, snapshot, 3)
			if loopErr != nil {
				t.Fatalf("the dial loop did not recover after the planned restart: %v (dials=%d)", loopErr, spent)
			}
			for range 2 {
				if err := <-serverErr; err != nil {
					t.Fatalf("relay: %v", err)
				}
			}
		})
	}
}

// TestV2DialRefusedWhileDrainingRetriesWithTheRelaysFloor pins the half a dial
// made INSIDE the drain window sees: the relay refuses before the upgrade and
// before token verification, so the 503 is the entire answer. Reading it as an
// anonymous dial failure is what let a startup pass condemn a lineage over a
// deploy that was already finishing.
func TestV2DialRefusedWhileDrainingRetriesWithTheRelaysFloor(t *testing.T) {
	t.Parallel()
	const refusals = 2
	snapshot := v2ResumeSnapshot(5)
	serverErr := make(chan error, 1)
	dials := make(chan int, 1)
	dials <- 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		dial := <-dials + 1
		dials <- dial
		if dial <= refusals {
			w.Header().Set("Retry-After", "2")
			http.Error(w, "relay-restarting", http.StatusServiceUnavailable)
			return
		}
		conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{
			Subprotocols: []string{attachwirev2.SubprotocolVersion},
		})
		if err != nil {
			serverErr <- err
			return
		}
		defer conn.CloseNow() //nolint:errcheck
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		serverErr <- serveAcceptedV2Candidate(ctx, conn, snapshot)
	}))
	t.Cleanup(server.Close)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	cfg := V2HostConfig{
		AttachURL:        strings.Replace(server.URL, "http://", "ws://", 1) + "/v2/rooms/session-v2",
		TokenSource:      func(context.Context) (string, error) { return v2TestToken(t, nil), nil },
		DurableHighWater: 4,
	}

	_, err := DialV2HostCandidate(ctx, cfg)
	var restart *RelayRestartingError
	if !errors.As(err, &restart) {
		t.Fatalf("refused dial err = %v (%T), want *RelayRestartingError", err, err)
	}
	if restart.StatusCode != http.StatusServiceUnavailable || restart.RedialAfter != 2*time.Second {
		t.Fatalf("refusal = %+v, want a 503 carrying a 2s floor", restart)
	}

	spent, loopErr := dialV2UntilTerminal(ctx, t, cfg, snapshot, refusals+2)
	if loopErr != nil {
		t.Fatalf("the dial loop gave up on a drain-window refusal: %v (dials=%d)", loopErr, spent)
	}
	if err := <-serverErr; err != nil {
		t.Fatalf("relay: %v", err)
	}
}

// TestBare503IsADialFailureNotAnAnnouncement is the other side of the
// discrimination: the classification must be narrow, but the DISPOSITION must
// not be. A 503 with no Retry-After is not the planned-restart announcement —
// so nothing logs one and the host lane keeps counting it toward the §14
// fallback — yet it is still not a verdict about the lineage, so it keeps
// carrying ErrRelayUnavailable and a composing daemon's re-dial budget is
// untouched.
func TestBare503IsADialFailureNotAnAnnouncement(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		// The relay's own non-drain 503: its durable rail is not healthy. That
		// is an answer about this candidate, not "come back in five seconds".
		http.Error(w, "attach-v2 takeover unavailable", http.StatusServiceUnavailable)
	}))
	t.Cleanup(server.Close)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_, err := DialV2HostCandidate(ctx, V2HostConfig{
		AttachURL:        strings.Replace(server.URL, "http://", "ws://", 1) + "/v2/rooms/session-v2",
		TokenSource:      func(context.Context) (string, error) { return v2TestToken(t, nil), nil },
		DurableHighWater: 4,
	})
	if err == nil {
		t.Fatal("a bare 503 completed the dial")
	}
	if IsRelayRestarting(err) {
		t.Fatalf("err = %v asserts a planned restart the relay never announced", err)
	}
	var dial *RelayDialError
	if !errors.As(err, &dial) {
		t.Fatalf("err = %v (%T), want *RelayDialError", err, err)
	}
	if !IsRelayUnavailable(err) {
		t.Fatalf("err = %v lost ErrRelayUnavailable — a bare 503 still learned nothing about the lineage, "+
			"so a composing daemon's re-dial budget must still apply", err)
	}
	if isRelayStop(err) {
		t.Fatalf("err = %v classified as terminal", err)
	}
}

// TestBare503StillEarnsTheDegradedFallback is the behavioural half of the same
// discrimination, on the lane where it is not merely cosmetic.
//
// RunHost's planned-restart case deliberately never calls maybeFallback: the
// drain refuses BOTH lanes, so falling back would move a healthy carrier onto
// the slow lane for a condition the slow lane shares. A bare 503 has no such
// guarantee behind it — it may be one hop, or one lane's dependency — so it
// must keep reaching the ordinary path and, after FallbackAfterN, the §14
// degraded lane. Classifying every 503 as a restart suppressed that fallback
// permanently.
func TestBare503StillEarnsTheDegradedFallback(t *testing.T) {
	// Every WSS dial answers a bare 503; the degraded lane is healthy.
	h := startHost(t, attachtest.Config{
		RefuseWSS: true, RefuseWSSStatus: http.StatusServiceUnavailable,
	}, func(c *HostConfig) {
		c.FallbackAfterN = 2
	})
	h.sess.PushOutput([]byte("boot"))

	if !waitFor(func() bool { return h.relay.HostBound() }, 10*time.Second) {
		t.Fatal("the host never reached the §14 degraded lane after repeated bare 503s — " +
			"a 503 with no announcement behind it must not suppress the fallback")
	}
	if !waitFor(func() bool {
		h.sess.PushOutput([]byte("after-bare-503"))
		return h.relay.HostAckSeq() > 0
	}, 10*time.Second) {
		t.Fatalf("the degraded lane bound but never acknowledged a batch (ack=%d)", h.relay.HostAckSeq())
	}
	select {
	case err := <-h.done:
		t.Fatalf("RunHost returned %v while falling back", err)
	default:
	}
}

// TestAnAbsurdRetryAfterCannotParkTheCarrier pins the ceiling on the floor.
//
// The planned-restart arm deliberately never falls back to the § 14 lane, so
// every second it honours is a second a LIVE seat's carrier spends parked with
// no alternative — and the number comes from whoever answered the dial. This
// relay clamps its own hint, but that hint is a free-form configured duration
// and an intermediary answering 503 is bound by nothing at all. A Retry-After
// of an hour must not park the carrier for an hour: past BackoffMax the dial
// goes out again and is refused again, which costs one refusal instead of an
// unbounded silence.
func TestAnAbsurdRetryAfterCannotParkTheCarrier(t *testing.T) {
	h := startHost(t, attachtest.Config{}, func(c *HostConfig) {
		c.BackoffMax = 40 * time.Millisecond
	})
	h.sess.PushOutput([]byte("boot"))
	waitBound(t, h.relay)
	if !waitFor(func() bool { return h.relay.Head() >= 1 }, 3*time.Second) {
		t.Fatalf("initial WSS frame never delivered (head=%d)", h.relay.Head())
	}

	h.relay.AnnounceRestart(time.Hour)

	// Unclamped this parks the carrier until the heat death of the test suite:
	// exactly one refusal would ever be recorded.
	if !waitFor(func() bool { return h.relay.RefusedDials() >= 4 }, 5*time.Second) {
		t.Fatalf("the drain window saw only %d dials in 5s against an hour-long Retry-After — "+
			"the honoured floor is not clamped to BackoffMax, so one 503 parked a live carrier",
			h.relay.RefusedDials())
	}
	select {
	case err := <-h.done:
		t.Fatalf("RunHost terminated while clamping an absurd floor: %v", err)
	default:
	}

	h.relay.EndRestart()
	if !waitFor(func() bool {
		h.sess.PushOutput([]byte("after-absurd-floor"))
		return h.relay.HostBound() && h.relay.Head() >= 2
	}, 10*time.Second) {
		t.Fatalf("the host never came back after the restart ended (bound=%v head=%d)",
			h.relay.HostBound(), h.relay.Head())
	}
}
