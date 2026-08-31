package attachclient

import (
	"bufio"
	"bytes"
	"context"
	crand "crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/RenseiAI/donmai/attachwire"
	"github.com/coder/websocket"
)

const (
	// maxDegradedBatch bounds the frame count in one host POST-up batch (§ 14).
	maxDegradedBatch = 64
	// sseHeartbeatBufMin is the initial SSE scanner buffer.
	sseHeartbeatBufMin = 64 << 10
)

// degradedEndpoints derives the host degraded-lane endpoints from AttachURL
// (§ 14, frozen): scheme map wss→https / ws→http, plus /host/sse and
// /host/output path suffixes. The /v1/ version segment inside the path is
// carried through unchanged.
func degradedEndpoints(attachURL string) (sseURL, postURL string, err error) {
	u, err := url.Parse(attachURL)
	if err != nil {
		return "", "", fmt.Errorf("attachclient: parsing AttachURL: %w", err)
	}
	switch u.Scheme {
	case "wss":
		u.Scheme = "https"
	case "ws":
		u.Scheme = "http"
	default:
		return "", "", fmt.Errorf("attachclient: AttachURL scheme %q is not ws/wss", u.Scheme)
	}
	base := strings.TrimRight(u.Path, "/")
	sse := *u
	sse.Path = base + "/host/sse"
	post := *u
	post.Path = base + "/host/output"
	return sse.String(), post.String(), nil
}

// tokenHolder caches the current bearer and can re-mint via the TokenSource on
// a 401 (§ 14/§ 15). Safe for concurrent use (POST-up + SSE + probe goroutines).
type tokenHolder struct {
	mu  sync.Mutex
	cur string
	src TokenSource
}

func (t *tokenHolder) current() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.cur
}

var errRejectedTokenUnchanged = errors.New("attachclient: token source returned the rejected bearer unchanged")

// remint re-resolves the token for proactive refresh (the upgrade probe).
func (t *tokenHolder) remint(ctx context.Context) (string, error) {
	tok, err := t.src(ctx)
	if err != nil {
		return "", err
	}
	t.mu.Lock()
	t.cur = tok
	t.mu.Unlock()
	return tok, nil
}

// remintAfterUnauthorized re-resolves after the relay rejects a bearer. It
// refuses to retry the same bearer immediately: returning a transient error
// hands control to RunHost's cancel-aware reconnect backoff instead of spinning
// on 401. If another concurrent lane already installed a different token, keep
// that newer value rather than regressing the holder to rejected.
func (t *tokenHolder) remintAfterUnauthorized(ctx context.Context, rejected string) error {
	tok, err := t.src(ctx)
	if err != nil {
		return err
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if tok == rejected {
		if t.cur != rejected {
			return nil
		}
		return errRejectedTokenUnchanged
	}
	t.cur = tok
	return nil
}

// dedupKey identifies a keystroke on the degraded SSE-down lane using only
// wire-carried fields (§ 14). See package doc for why penGeneration substitutes
// for the spec's jti (jti is not on the Input wire).
type dedupKey struct {
	userID   string
	penGen   uint64
	inputSeq uint64
}

// dedupSet is a bounded recency set of seen Input keys: at-least-once SSE
// replays are dropped, while the bound keeps memory finite (an SSE reconnect
// only replays a recent window).
type dedupSet struct {
	limit int
	seen  map[dedupKey]struct{}
	order []dedupKey
}

func newDedupSet(limit int) *dedupSet {
	return &dedupSet{limit: limit, seen: make(map[dedupKey]struct{})}
}

func (d *dedupSet) has(k dedupKey) bool {
	_, ok := d.seen[k]
	return ok
}

func (d *dedupSet) add(k dedupKey) {
	if _, ok := d.seen[k]; ok {
		return
	}
	d.seen[k] = struct{}{}
	d.order = append(d.order, k)
	if len(d.order) > d.limit {
		old := d.order[0]
		d.order = d.order[1:]
		delete(d.seen, old)
	}
}

func newBatchID() string {
	var b [16]byte
	_, _ = crand.Read(b[:])
	return hex.EncodeToString(b[:])
}

func parseRetryAfter(v string) time.Duration {
	v = strings.TrimSpace(v)
	if v == "" {
		return 500 * time.Millisecond
	}
	if secs, err := strconv.Atoi(v); err == nil && secs >= 0 {
		return time.Duration(secs) * time.Second
	}
	if t, err := http.ParseTime(v); err == nil {
		if d := time.Until(t); d > 0 {
			return d
		}
	}
	return 500 * time.Millisecond
}

func (h *host) httpClient() *http.Client {
	if h.cfg.HTTPClient != nil {
		return h.cfg.HTTPClient
	}
	return http.DefaultClient
}

// runDegraded runs the host leg over the § 14 degraded carrier: an SSE-down GET
// (relay→host frames) plus batched POST-up (host seq-bearing frames). It opens
// the SSE leg (binding the host leg with the epoch CAS), streams the Session's
// frames as POST batches, answers snapshot_request via the outOfSeq array, and
// periodically probes WSS for upgrade-back. Returns errUpgraded when WSS is
// reachable again, ErrEpochStale on a stale-epoch reject, a *RelayRingMissError
// when the relay (or our own retained ring) has lost history — RESET-AND-RETRY,
// never terminal — or a transient error otherwise.
func (h *host) runDegraded(ctx context.Context, tok string, cl hostClaims, exitDeadline time.Time) (attemptResult, error) {
	var res attemptResult

	sseURL, postURL, err := degradedEndpoints(h.cfg.AttachURL)
	if err != nil {
		return res, err
	}

	legCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	// Every refresh path (401 recovery + upgrade probe) must preserve the live
	// Session's immutable PTY epoch, not merely parse a bearer. validatedToken
	// applies the same ground-truth check as the top-level reconnect loop.
	tokH := &tokenHolder{cur: tok, src: h.validatedToken}

	// Open SSE-down: binds the host leg (epoch CAS). 409 == epoch-stale.
	sseResp, err := h.openHostSSE(legCtx, sseURL, tokH, cl)
	if err != nil {
		return res, err // ErrEpochStale propagates
	}
	defer sseResp.Body.Close() //nolint:errcheck
	res.progressed = true      // SSE bound → success (reset backoff)
	res.authorityConfirmed = true
	res.progressedAt = h.now()

	outCh := make(chan attachwire.Frame, 32) // post-Exit snapshot replies → outOfSeq
	dedup := newDedupSet(h.cfg.DedupWindow)

	downErr := make(chan error, 1)
	go func() { downErr <- h.degradedDown(legCtx, sseResp, dedup, outCh) }()

	upgradeCh := make(chan error, 1)
	go h.upgradeProbe(legCtx, tokH, upgradeCh)

	// Announce the host leg with a subscribe control in the first batch's
	// outOfSeq (§ 14: host Control rides outOfSeq). Binding also happened on the
	// SSE GET, so a failure here is non-fatal.
	if subFrame, serr := buildHostSubscribe(cl); serr == nil {
		batch := attachwire.HostFrameBatch{
			BatchID:  newBatchID(),
			OutOfSeq: []string{attachwire.EncodeFrameBase64(subFrame)},
		}
		if _, _, perr := h.postHostBatch(legCtx, postURL, tokH, batch); perr != nil {
			h.log.Debug("attachclient: degraded subscribe control POST failed (non-fatal)", "err", perr)
		}
	}

	// Begin streaming: Subscribe per the reconnect discipline; lastAck starts at
	// the fromSeq (the relay corrects it via a 409 if its ack differs).
	fromSeq, err := h.subscribeFromSeq()
	if err != nil {
		return res, fmt.Errorf("attachclient: resolving reconnect head: %w", err)
	}
	sub, err := h.cfg.Session.Subscribe(fromSeq)
	if err != nil {
		return res, fmt.Errorf("attachclient: session subscribe from %d: %w", fromSeq, err)
	}
	defer func() { _ = sub.Close() }()
	h.markStreamed()

	lastAck := int64(fromSeq) //nolint:gosec // G115: host seq lives in the protocol varint domain
	frames := sub.Frames()

	for {
		select {
		case <-ctx.Done():
			return res, ctx.Err()
		case derr := <-downErr:
			return res, fmt.Errorf("attachclient: degraded SSE-down: %w", derr)
		case upgradeResult := <-upgradeCh:
			// Between batches → all POSTed batches are acked. errUpgraded is a
			// clean carrier switch; a confirmed successor-grant error must reach
			// RunHost immediately rather than leaving the old degraded leg live.
			return res, upgradeResult
		case oof := <-outCh:
			batch := attachwire.HostFrameBatch{
				BatchID:  newBatchID(),
				OutOfSeq: append([]string{attachwire.EncodeFrameBase64(oof)}, encodeFrames(drainFrames(outCh, maxDegradedBatch))...),
			}
			if _, _, perr := h.postHostBatch(legCtx, postURL, tokH, batch); perr != nil {
				return res, perr
			}
		case f, ok := <-frames:
			if !ok {
				// Subscription ended. Exit delivered?
				if _, exited := h.cfg.Session.Exit(); !exited {
					return res, fmt.Errorf("attachclient: degraded subscription ended before Exit")
				}
				res.exitDelivered = true
				deadline := exitDeadline
				if deadline.IsZero() {
					deadline = h.now().Add(h.cfg.FinalScreenWindow)
				}
				res.windowDeadline = deadline
				werr := h.degradedWindow(legCtx, postURL, tokH, outCh, downErr, deadline)
				if werr == nil {
					res.windowServed = true
				}
				return res, werr
			}
			batch := h.collectHostBatch(f, frames, outCh, lastAck)
			ack, outcome, perr := h.postHostBatch(legCtx, postURL, tokH, batch)
			switch outcome {
			case postOK:
				lastAck = ack
			case postRewind:
				// 409: the relay wants firstSeq == ack+1. Re-Subscribe(ack) and
				// resend from the ring (§ 14). If our OWN retained ring can't
				// satisfy that ack either (e.g. the relay restarted and its ack
				// regressed below what we still hold), we lost the frames just as
				// surely as the relay did — that is a ring miss by definition, and
				// §13 makes ring misses RESET-AND-RETRY, never terminal: abandon
				// this attempt and let the top-level loop re-attach fresh (fromSeq
				// 0, no resume position) rather than fail the session.
				_ = sub.Close()
				ns, err := h.cfg.Session.Subscribe(attachwire.HostSeq(ack)) //nolint:gosec // G115: ack is a non-negative host seq
				if err != nil {
					return res, &RelayRingMissError{
						Code:    attachwire.CodeRingMiss,
						Message: fmt.Sprintf("degraded rewind past local ring at ack=%d: %v", ack, err),
					}
				}
				sub, lastAck, frames = ns, ack, ns.Frames()
			case postFatal:
				return res, perr
			}
		}
	}
}

// collectHostBatch builds one host POST-up batch starting with first, greedily
// draining more contiguous frames (up to maxDegradedBatch) and any pending
// outOfSeq. firstSeq is lastAck+1 by construction (the subscription yields
// lastAck+1 onward).
func (h *host) collectHostBatch(first attachwire.Frame, frames <-chan attachwire.Frame, outCh <-chan attachwire.Frame, lastAck int64) attachwire.HostFrameBatch {
	seqFrames := []attachwire.Frame{first}
drain:
	for len(seqFrames) < maxDegradedBatch {
		select {
		case f, ok := <-frames:
			if !ok {
				break drain // subscription closed; the main loop handles Exit next
			}
			seqFrames = append(seqFrames, f)
		default:
			break drain
		}
	}
	lastSeq := int64(seqFrames[len(seqFrames)-1].Seq) //nolint:gosec // G115: host seq lives in the protocol varint domain
	return attachwire.HostFrameBatch{
		BatchID:  newBatchID(),
		FirstSeq: lastAck + 1,
		LastSeq:  lastSeq,
		Frames:   encodeFrames(seqFrames),
		OutOfSeq: encodeFrames(drainFrames(outCh, maxDegradedBatch)),
	}
}

func drainFrames(ch <-chan attachwire.Frame, limit int) []attachwire.Frame {
	var out []attachwire.Frame
	for len(out) < limit {
		select {
		case f := <-ch:
			out = append(out, f)
		default:
			return out
		}
	}
	return out
}

func encodeFrames(fs []attachwire.Frame) []string {
	if len(fs) == 0 {
		return nil
	}
	out := make([]string, len(fs))
	for i, f := range fs {
		out[i] = attachwire.EncodeFrameBase64(f)
	}
	return out
}

// degradedWindow serves the post-Exit final-screen window on the degraded lane
// (§ 12.2): it POSTs post-Exit Snapshot replies (which arrive on outCh from the
// SSE-down handler) via outOfSeq until the deadline. A mid-window SSE drop
// returns a transient error so RunHost reconnects to serve the remainder.
func (h *host) degradedWindow(legCtx context.Context, postURL string, tokH *tokenHolder, outCh chan attachwire.Frame, downErr chan error, deadline time.Time) error {
	timer := time.NewTimer(time.Until(deadline))
	defer timer.Stop()
	for {
		select {
		case <-legCtx.Done():
			return legCtx.Err()
		case derr := <-downErr:
			return fmt.Errorf("attachclient: degraded SSE-down dropped during post-Exit window: %w", derr)
		case oof := <-outCh:
			batch := attachwire.HostFrameBatch{
				BatchID:  newBatchID(),
				OutOfSeq: append([]string{attachwire.EncodeFrameBase64(oof)}, encodeFrames(drainFrames(outCh, maxDegradedBatch))...),
			}
			if _, _, perr := h.postHostBatch(legCtx, postURL, tokH, batch); perr != nil {
				return perr
			}
		case <-timer.C:
			return nil
		}
	}
}

// openHostSSE opens the host SSE-down GET, binding the host leg (epoch CAS on
// the relay). It carries the Authorization header and ?epoch=<epoch> (host legs
// are always native, header-only; the host-inbound stream has no seq to resume,
// § 14). 401 → re-mint and retry; 409 → epoch-stale, surfaced to RunHost's
// local-PTY-grounded bounded recovery decision.
func (h *host) openHostSSE(ctx context.Context, sseURL string, tokH *tokenHolder, cl hostClaims) (*http.Response, error) {
	u := sseURL + "?epoch=" + strconv.FormatInt(cl.Epoch, 10)
	authRetried := false
	for {
		rejected := tokH.current()
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
		if err != nil {
			return nil, err
		}
		req.Header.Set("Authorization", "Bearer "+rejected)
		req.Header.Set("Accept", "text/event-stream")
		resp, err := h.httpClient().Do(req)
		if err != nil {
			return nil, err
		}
		switch resp.StatusCode {
		case http.StatusOK:
			return resp, nil
		case http.StatusUnauthorized:
			_ = resp.Body.Close()
			if authRetried {
				return nil, errors.New("attachclient: host SSE GET remained unauthorized after token refresh")
			}
			authRetried = true
			if err := tokH.remintAfterUnauthorized(ctx, rejected); err != nil {
				return nil, fmt.Errorf("attachclient: re-minting after 401 on host SSE GET: %w", err)
			}
		case http.StatusConflict:
			_ = resp.Body.Close()
			return nil, ErrEpochStale
		default:
			_ = resp.Body.Close()
			return nil, fmt.Errorf("attachclient: host SSE GET status %d", resp.StatusCode)
		}
	}
}

// degradedDown reads the host SSE-down stream (relay-originated frames) and
// applies each idempotently (§ 14 at-least-once). Returns when the stream ends
// or ctx is cancelled.
func (h *host) degradedDown(ctx context.Context, resp *http.Response, dedup *dedupSet, outCh chan attachwire.Frame) error {
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 0, sseHeartbeatBufMin), int(h.cfg.ReadLimitBytes))
	var data strings.Builder
	for sc.Scan() {
		if err := ctx.Err(); err != nil {
			return err
		}
		line := sc.Text()
		switch {
		case line == "":
			if data.Len() > 0 {
				if err := h.dispatchSSEFrame(ctx, data.String(), dedup, outCh); err != nil {
					return err
				}
			}
			data.Reset()
		case strings.HasPrefix(line, "data:"):
			data.WriteString(strings.TrimPrefix(strings.TrimPrefix(line, "data:"), " "))
		}
		// Other SSE lines — the ": heartbeat" comment and the "event: frame" name
		// (single frame per event by contract) — are informational and ignored.
	}
	if err := sc.Err(); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return io.EOF
}

func (h *host) dispatchSSEFrame(ctx context.Context, b64 string, dedup *dedupSet, outCh chan attachwire.Frame) error {
	frame, err := attachwire.DecodeFrameBase64(b64)
	if err != nil {
		h.log.Warn("attachclient: undecodable SSE frame; skipping", "err", err)
		return nil
	}
	return h.handleDegradedInbound(ctx, frame, dedup, outCh)
}

// handleDegradedInbound is the SSE-down counterpart of handleInbound with
// at-least-once idempotency (§ 14): Input is de-duplicated by (userId,
// penGeneration, inputSeq); snapshot_request/kill are repeat-safe; Resize is
// last-writer-wins. Post-Exit Snapshot replies are routed to outCh for the
// POST-up outOfSeq array.
func (h *host) handleDegradedInbound(ctx context.Context, f attachwire.Frame, dedup *dedupSet, outCh chan attachwire.Frame) error {
	if f.Type == attachwire.TypeInput {
		in, err := attachwire.DecodeInput(f.Payload)
		if err != nil {
			return err
		}
		if !in.Stamped() {
			h.log.Warn("attachclient: dropping unstamped SSE Input (userIdLen==0) — host trust posture §5", "inputSeq", in.InputSeq)
			return nil
		}
		key := dedupKey{userID: string(in.UserID), penGen: in.PenGeneration, inputSeq: in.InputSeq}
		if dedup.has(key) {
			h.log.Debug("attachclient: dropping replayed SSE Input (at-least-once dedup)", "inputSeq", in.InputSeq, "penGen", in.PenGeneration)
			return nil
		}
		dedup.add(key)
		if _, err := h.cfg.Session.WriteInput(in.Data); err != nil {
			return fmt.Errorf("attachclient: writing SSE input: %w", err)
		}
		return nil
	}

	back, err := h.handleInbound(ctx, f)
	if err != nil {
		return err
	}
	for _, bf := range back {
		select {
		case outCh <- bf:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return nil
}

// postOutcome is the mapped result of a host POST-up (§ 14 response taxonomy).
type postOutcome int

const (
	postOK     postOutcome = iota // 200: advance the send window
	postRewind                    // 409: rewind to ackSeq+1 and resend
	postFatal                     // transient/fatal error (caller reconnects)
)

// maxPostRetries bounds same-batchId retries on transient POST failures (a lost
// response, a 5xx). The relay de-duplicates by batchId so a retry is a no-op
// that returns the same ack (§ 14 idempotency).
const maxPostRetries = 8

// postHostBatch POSTs one host batch and maps the § 14 response taxonomy.
// batchId is fixed for the life of the batch (retries reuse it, never a new
// one), so a lost 200 can never double-apply. 401 → re-mint + retry; 429 →
// honor Retry-After + retry; a transient network/5xx error → bounded
// same-batchId retry.
func (h *host) postHostBatch(ctx context.Context, url string, tokH *tokenHolder, batch attachwire.HostFrameBatch) (ackSeq int64, outcome postOutcome, err error) {
	body, err := json.Marshal(batch)
	if err != nil {
		return 0, postFatal, fmt.Errorf("attachclient: marshaling host batch: %w", err)
	}
	attempts := 0
	authRetried := false
	for {
		attempts++
		rejected := tokH.current()
		req, rerr := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
		if rerr != nil {
			return 0, postFatal, rerr
		}
		req.Header.Set("Authorization", "Bearer "+rejected)
		req.Header.Set("Content-Type", "application/json")

		resp, derr := h.httpClient().Do(req)
		if derr != nil {
			// Lost response / connection error → retry the SAME batchId.
			if attempts > maxPostRetries || ctx.Err() != nil {
				return 0, postFatal, fmt.Errorf("attachclient: host POST %q: %w", batch.BatchID, derr)
			}
			if serr := sleepCtx(ctx, backoffStep(attempts)); serr != nil {
				return 0, postFatal, serr
			}
			continue
		}

		switch resp.StatusCode {
		case http.StatusOK:
			var acc attachwire.HostBatchAccepted
			_ = json.NewDecoder(resp.Body).Decode(&acc)
			_ = resp.Body.Close()
			return acc.AckSeq, postOK, nil
		case http.StatusConflict:
			var rej attachwire.HostBatchRejected
			_ = json.NewDecoder(resp.Body).Decode(&rej)
			_ = resp.Body.Close()
			return rej.AckSeq, postRewind, nil
		case http.StatusUnauthorized:
			_ = resp.Body.Close()
			if authRetried {
				return 0, postFatal, fmt.Errorf("attachclient: host POST %q remained unauthorized after token refresh", batch.BatchID)
			}
			authRetried = true
			if err := tokH.remintAfterUnauthorized(ctx, rejected); err != nil {
				return 0, postFatal, fmt.Errorf("attachclient: re-minting after 401 on host POST: %w", err)
			}
			// retry same batchId once with the refreshed bearer
		case http.StatusTooManyRequests:
			ra := parseRetryAfter(resp.Header.Get("Retry-After"))
			_ = resp.Body.Close()
			if serr := sleepCtx(ctx, ra); serr != nil {
				return 0, postFatal, serr
			}
			// retry same batchId
		default:
			retryable := resp.StatusCode >= 500
			_ = resp.Body.Close()
			if retryable && attempts <= maxPostRetries && ctx.Err() == nil {
				if serr := sleepCtx(ctx, backoffStep(attempts)); serr != nil {
					return 0, postFatal, serr
				}
				continue
			}
			return 0, postFatal, fmt.Errorf("attachclient: host POST %q unexpected status %d", batch.BatchID, resp.StatusCode)
		}
	}
}

// backoffStep is a small fixed-schedule backoff for same-batchId POST retries.
func backoffStep(attempt int) time.Duration {
	d := time.Duration(attempt) * 50 * time.Millisecond
	if d > time.Second {
		d = time.Second
	}
	return d
}

// upgradeProbe periodically re-dials WSS in the background (§ 14 upgrade-back).
// On a successful handshake it signals once and returns; RunHost then switches
// back to the WSS lane. The token is re-resolved per attempt (§ 15).
func (h *host) upgradeProbe(ctx context.Context, tokH *tokenHolder, result chan error) {
	t := time.NewTicker(h.cfg.UpgradeProbeInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			tok, err := tokH.remint(ctx)
			if err != nil {
				if errors.Is(err, errEpochGrantSuperseded) {
					select {
					case result <- err:
					default:
					}
					return
				}
				continue
			}
			if h.probeWSS(ctx, tok) {
				select {
				case result <- errUpgraded:
				default:
				}
				return
			}
		}
	}
}

func (h *host) probeWSS(ctx context.Context, tok string) bool {
	dctx, cancel := context.WithTimeout(ctx, h.cfg.DialTimeout)
	defer cancel()
	conn, _, err := websocket.Dial(dctx, h.cfg.AttachURL, &websocket.DialOptions{
		HTTPClient:   h.cfg.HTTPClient,
		HTTPHeader:   http.Header{"Authorization": {"Bearer " + tok}},
		Subprotocols: []string{attachwire.SubprotocolVersion},
	})
	if err != nil {
		return false
	}
	ok := conn.Subprotocol() == attachwire.SubprotocolVersion
	_ = conn.Close(websocket.StatusNormalClosure, "upgrade probe")
	return ok
}
