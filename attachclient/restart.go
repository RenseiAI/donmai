package attachclient

// A PLANNED RELAY RESTART IS NOT A LOST RELAY
//
// A relay that runs as a single always-on process with in-process room state
// restarts the whole fleet's host carriers at once on every deploy. The damage
// that caused was never the restart: it was that the restart arrived
// INDISTINGUISHABLE from the box dying. http.Server.Shutdown does not wait on
// hijacked connections, so each host read a bare mid-frame EOF — the same thing
// a power cycle produces — and classified a carrier that was seconds from
// returning as unreachable. Seats were quarantined; one was lost.
//
// The planned-restart contract closes that gap by making the relay SAY so, in
// three places a client can observe:
//
//   - a §7 `error` control with code relay-restarting, retryable, whose message
//     is the frozen grammar "redial after <N>s" (attach-v1 legs);
//   - a WebSocket close with status 1012 (Service Restart) and reason
//     "relay-restarting: redial after <N>s" (every leg);
//   - a 503 Service Unavailable with Retry-After: <N>, answered to every attach
//     dial during the drain window, BEFORE the upgrade and before token
//     verification.
//
// This file is the client half: it turns all three into ONE typed,
// never-terminal signal carrying the redial floor. What each carrier does with
// it is in its own lane — RunHost's reconnect switch for v1/degraded, the
// candidate's terminal error for v2 — but no lane may treat it as a verdict on
// the session. The host still owns the authoritative PTY; the only correct
// response is to wait out the hint and dial the replacement.

import (
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/RenseiAI/donmai/attachwire"
	"github.com/coder/websocket"
)

// ErrRelayUnavailable is the sentinel every transport-level relay refusal
// wraps: the relay did not admit this dial, and NOTHING was learned about the
// lineage behind it. It is the distinction a composing daemon needs before it
// decides anything terminal — "the relay did not answer" is evidence about the
// relay, never about the session whose carrier was being dialled.
var ErrRelayUnavailable = errors.New("attachclient: the relay did not admit this dial")

// RelayRestartingError is the planned-restart signal, from any of the three
// places the contract puts it. It is NEVER terminal on any lane.
//
// RedialAfter is the floor the relay asked for, zero when it named none (an
// unparsable grammar, or a Retry-After this client does not read). A caller
// uses it as a MINIMUM delay before the next dial, not as a maximum: its own
// backoff still applies on top, and a fleet that honours it does not arrive
// back before the replacement process has booted.
type RelayRestartingError struct {
	// Code is the §7 error.code when the relay named itself in a control frame
	// or in the close reason; empty when only the 1012 close code said so.
	Code attachwire.ErrorCode
	// Message is the relay's own message ("redial after 5s"), or the whole
	// close reason when the signal arrived as a close.
	Message string
	// RedialAfter is the parsed redial floor; zero when none was named.
	RedialAfter time.Duration
	// StatusCode is the pre-upgrade HTTP status that refused the dial (503),
	// zero when the signal did not arrive as a dial refusal.
	StatusCode int
	// CloseCode is the WebSocket close status that ended the leg (1012), zero
	// when the signal did not arrive as a close.
	CloseCode int
}

func (e *RelayRestartingError) Error() string {
	where := "the relay announced a planned restart"
	switch {
	case e.StatusCode != 0:
		where = fmt.Sprintf("the relay refused this dial with %d while draining for a planned restart", e.StatusCode)
	case e.CloseCode != 0:
		where = fmt.Sprintf("the relay closed this leg with %d for a planned restart", e.CloseCode)
	}
	switch {
	case e.Message != "":
		// The message IS the announcement, in the frozen grammar or the whole
		// close reason. Restating the parsed floor after it only prints the same
		// number twice into an operator's quarantine detail.
		return fmt.Sprintf("attachclient: %s: %s", where, e.Message)
	case e.RedialAfter > 0:
		return fmt.Sprintf("attachclient: %s: redial after %s", where, e.RedialAfter)
	default:
		return fmt.Sprintf("attachclient: %s: no redial hint", where)
	}
}

// Unwrap makes errors.Is(err, ErrRelayUnavailable) true through any composing
// caller's own %w wrapping.
func (e *RelayRestartingError) Unwrap() error { return ErrRelayUnavailable }

// RelayDialError is a transport-level dial or handshake failure: the relay was
// not reached at all, or did not complete the upgrade. Like a planned restart
// it says nothing about the lineage — it is typed only so a composing daemon
// can tell "nobody answered" apart from a refusal that carries a verdict.
type RelayDialError struct {
	// Op names the lane and stage, e.g. "attachclient: v2 wss dial".
	Op    string
	cause error
}

func (e *RelayDialError) Error() string {
	if e.cause == nil {
		return e.Op
	}
	return e.Op + ": " + e.cause.Error()
}

// Unwrap exposes both the sentinel and the transport cause, so
// errors.Is(err, ErrRelayUnavailable) and errors.As on the underlying network
// error both work through it.
func (e *RelayDialError) Unwrap() []error {
	if e.cause == nil {
		return []error{ErrRelayUnavailable}
	}
	return []error{ErrRelayUnavailable, e.cause}
}

func newRelayDialError(op string, cause error) *RelayDialError {
	return &RelayDialError{Op: op, cause: cause}
}

// IsRelayUnavailable reports whether err is, or wraps, a refusal that reached
// no verdict about the lineage: a planned restart, or a dial that never got
// through. It is the predicate a caller checks BEFORE any terminal decision.
func IsRelayUnavailable(err error) bool { return errors.Is(err, ErrRelayUnavailable) }

// IsRelayRestarting reports whether err is, or wraps, the planned-restart
// signal specifically.
func IsRelayRestarting(err error) bool {
	_, ok := relayRestarting(err)
	return ok
}

// RelayRedialAfter reports the redial floor a planned restart named, and
// whether err was a planned restart at all. A restart with no parsable hint
// reports (0, true): the caller still must not treat it as terminal.
func RelayRedialAfter(err error) (time.Duration, bool) {
	restart, ok := relayRestarting(err)
	if !ok {
		return 0, false
	}
	return restart.RedialAfter, true
}

func relayRestarting(err error) (*RelayRestartingError, bool) {
	var restart *RelayRestartingError
	if errors.As(err, &restart) {
		return restart, true
	}
	return nil, false
}

// redialHint parses the frozen announcement grammar, "redial after <N>s".
var redialHint = regexp.MustCompile(`^redial after ([0-9]+)s$`)

// parseRedialHint reads the frozen grammar and returns the floor it names. A
// message that does not match yields (0, false) — a relay that says something
// else is still restarting, it just named no floor, so the caller falls back to
// its own backoff rather than inventing a number.
func parseRedialHint(message string) (time.Duration, bool) {
	match := redialHint.FindStringSubmatch(strings.TrimSpace(message))
	if match == nil {
		return 0, false
	}
	seconds, err := strconv.ParseUint(match[1], 10, 32)
	if err != nil || seconds == 0 {
		return 0, false
	}
	return time.Duration(seconds) * time.Second, true
}

// parseRetryAfterSeconds reads the delta-seconds form of Retry-After, which is
// the form the planned-restart contract freezes (the same integer as the
// announcement's hint). The HTTP-date form is deliberately NOT read: it needs a
// trusted clock this client does not have on the refusal path, and a caller
// that gets no floor simply uses its own backoff.
func parseRetryAfterSeconds(value string) time.Duration {
	seconds, err := strconv.ParseUint(strings.TrimSpace(value), 10, 32)
	if err != nil || seconds == 0 {
		return 0
	}
	return time.Duration(seconds) * time.Second
}

// RelayRestartRefusal classifies one pre-upgrade HTTP answer to an attach dial
// as the drain-window refusal the planned-restart contract defines: **503
// Service Unavailable carrying a Retry-After the contract's delta-seconds form
// can be read from**. It returns nil for anything else, including a bare 503.
//
// The Retry-After is a REQUIREMENT here, not merely the source of the floor,
// and that is the whole discrimination this function exists to make. 503 on its
// own is not an announcement: a relay answers one for conditions that ARE a
// verdict about this candidate — a durable rail that cannot take its journal
// lock, for instance — and an intermediary answers one for having no capacity
// at all. Reading those as "the relay is restarting" would print an
// announcement nobody made, and on the host lane it would suppress the §14
// degraded fallback indefinitely for a condition the degraded lane might not
// share.
//
// A bare 503 is still not a verdict on the LINEAGE, so every caller wraps it as
// a RelayDialError, which carries the same ErrRelayUnavailable sentinel: a
// composing daemon's re-dial budget is unchanged, only the announcement is not
// asserted.
//
// The response body is deliberately not consulted. The contract puts the
// announcement in the header, and reading an arbitrary server's body on a dial
// path would make a refusal's classification depend on how fast that body
// arrives.
func RelayRestartRefusal(resp *http.Response) *RelayRestartingError {
	if resp == nil || resp.StatusCode != http.StatusServiceUnavailable {
		return nil
	}
	after := parseRetryAfterSeconds(resp.Header.Get("Retry-After"))
	if after <= 0 {
		return nil
	}
	return &RelayRestartingError{
		Code:        attachwire.CodeRelayRestarting,
		Message:     fmt.Sprintf("redial after %ds", int(after/time.Second)),
		RedialAfter: after,
		StatusCode:  resp.StatusCode,
	}
}

// relayRestartClose classifies the end of a live leg. The close half of the
// announcement reaches BOTH lanes — it is the only half an attach-v2 leg is
// sent — so reading it is what makes the two lanes agree without either one
// having to receive a control frame first.
//
// Two things classify: the frozen reason grammar
// "relay-restarting: redial after <N>s", and the 1012 Service Restart status on
// its own. 1012 means the server is restarting, which is a planned transient by
// definition, so a relay that closes with it but names no reason is still not a
// reason to give up.
func relayRestartClose(err error) *RelayRestartingError {
	var closeErr websocket.CloseError
	if !errors.As(err, &closeErr) {
		return nil
	}
	reason := strings.TrimSpace(closeErr.Reason)
	named := false
	hint := reason
	if prefix := string(attachwire.CodeRelayRestarting) + ":"; strings.HasPrefix(reason, prefix) {
		named = true
		hint = strings.TrimSpace(strings.TrimPrefix(reason, prefix))
	} else if reason == string(attachwire.CodeRelayRestarting) {
		named = true
		hint = ""
	}
	if !named && closeErr.Code != websocket.StatusServiceRestart {
		return nil
	}
	restart := &RelayRestartingError{
		Message:   reason,
		CloseCode: int(closeErr.Code),
	}
	if named {
		restart.Code = attachwire.CodeRelayRestarting
	}
	if after, ok := parseRedialHint(hint); ok {
		restart.RedialAfter = after
	}
	if restart.Message == "" {
		restart.Message = "no reason"
	}
	return restart
}

// relayRestartControl builds the signal from a §7 error control the relay sent
// on an open leg.
func relayRestartControl(message attachwire.ControlError) *RelayRestartingError {
	restart := &RelayRestartingError{Code: message.Code, Message: message.Message}
	if after, ok := parseRedialHint(message.Message); ok {
		restart.RedialAfter = after
	}
	return restart
}
