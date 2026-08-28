package executionevent

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/RenseiAI/donmai/agent"
	"github.com/RenseiAI/donmai/runtime/statehome"
)

const (
	// DefaultMaxRetries is the bounded retry count for transient delivery.
	DefaultMaxRetries = 5
	// DefaultInitialBackoff is the first transient retry delay.
	DefaultInitialBackoff = 250 * time.Millisecond
	// DefaultMaxBackoff bounds transient retry delay.
	DefaultMaxBackoff = 5 * time.Second
	// DefaultHTTPTimeout bounds one request.
	DefaultHTTPTimeout = 5 * time.Second
	// DefaultStopDrainTimeout bounds terminal flush.
	DefaultStopDrainTimeout = 5 * time.Second
)

// RuntimeCredentials are the worker credentials used for ingest.
type RuntimeCredentials struct {
	WorkerID  string
	AuthToken string
}

// CredentialProvider supplies fresh worker credentials for each attempt.
type CredentialProvider func(context.Context) (RuntimeCredentials, error)

// Config configures one session's journal and runtime-event uploader.
type Config struct {
	SessionID          string
	BaseURL            string
	AuthToken          string
	CredentialProvider CredentialProvider
	HTTPClient         *http.Client
	Logger             *slog.Logger
	Now                func() time.Time
	Sleep              func(time.Duration)
	JournalDir         string
	MaxRetries         int
	InitialBackoff     time.Duration
	MaxBackoff         time.Duration
	StopDrainTimeout   time.Duration
}

func (c Config) retries() int {
	if c.MaxRetries > 0 {
		return c.MaxRetries
	}
	return DefaultMaxRetries
}

func (c Config) initialBackoff() time.Duration {
	if c.InitialBackoff > 0 {
		return c.InitialBackoff
	}
	return DefaultInitialBackoff
}

func (c Config) maxBackoff() time.Duration {
	if c.MaxBackoff > 0 {
		return c.MaxBackoff
	}
	return DefaultMaxBackoff
}

func (c Config) stopTimeout() time.Duration {
	if c.StopDrainTimeout > 0 {
		return c.StopDrainTimeout
	}
	return DefaultStopDrainTimeout
}

func (c Config) client() *http.Client {
	if c.HTTPClient != nil {
		return c.HTTPClient
	}
	return &http.Client{Timeout: DefaultHTTPTimeout}
}

func (c Config) logger() *slog.Logger {
	if c.Logger != nil {
		return c.Logger
	}
	return slog.Default()
}

func (c Config) now() time.Time {
	if c.Now != nil {
		return c.Now()
	}
	return time.Now()
}

func (c Config) sleep(d time.Duration) {
	if c.Sleep != nil {
		c.Sleep(d)
		return
	}
	time.Sleep(d)
}

// Uploader journals normalized records and drains them to the platform.
type Uploader struct {
	config  Config
	journal *Journal
}

// New creates a journal-backed uploader for one session.
func New(c Config) (*Uploader, error) {
	if strings.TrimSpace(c.SessionID) == "" {
		return nil, errors.New("executionevent: SessionID is required")
	}
	if _, err := SessionEndpoint(c.BaseURL, c.SessionID); err != nil {
		return nil, err
	}
	dir := c.JournalDir
	if dir == "" {
		sum := sha256.Sum256([]byte(c.SessionID))
		dir = statehome.StateDir("execution-events/" + hex.EncodeToString(sum[:]))
	}
	if dir == "" {
		return nil, errors.New("executionevent: cannot resolve journal directory")
	}
	j, err := OpenJournal(dir, c.SessionID)
	if err != nil {
		return nil, err
	}
	return &Uploader{config: c, journal: j}, nil
}

// Journal returns the uploader's durable journal for diagnostics and tests.
func (u *Uploader) Journal() *Journal { return u.journal }

// SendRecord journals before returning. The caller can call Flush immediately
// for live delivery or Drain at terminal shutdown; an interrupted upload is
// resumed from the durable ack on the next Uploader construction.
func (u *Uploader) SendRecord(record Record) error {
	return u.journal.Append(u.config.SessionID, record)
}

// SendEvent normalizes and durably appends one active agent event.
func (u *Uploader) SendEvent(event agent.Event) (bool, error) {
	seq := u.journal.NextSequence()
	record, emitted, err := NormalizeEvent(u.config.SessionID, seq, u.config.now(), event)
	if err != nil || !emitted {
		return emitted, err
	}
	if err := u.SendRecord(record); err != nil {
		return false, err
	}
	return true, nil
}

// SendSessionEnded appends a graceful terminal source record.
func (u *Uploader) SendSessionEnded(outcome, resultDigest string) error {
	return u.SendSessionEndedWithEvidence(outcome, "graceful", resultDigest)
}

// SendSessionEndedWithEvidence appends a terminal record with explicit evidence.
func (u *Uploader) SendSessionEndedWithEvidence(outcome, evidence, resultDigest string) error {
	record, err := NewSessionEndedRecordWithEvidence(u.config.SessionID, u.journal.NextSequence(), u.config.now(), outcome, evidence, resultDigest)
	if err != nil {
		return err
	}
	return u.SendRecord(record)
}

// SendSessionBlocked appends a runner-owned blocked fact before terminal
// settlement. The journal sequence is allocated at append time, preserving
// contiguous ordering with provider-normalized events and session.ended.
func (u *Uploader) SendSessionBlocked(reason string) error {
	record, err := NewSessionBlockedRecord(u.config.SessionID, u.journal.NextSequence(), u.config.now(), reason)
	if err != nil {
		return err
	}
	return u.SendRecord(record)
}

// SendPullRequestOpened appends one complete, validated GitHub PR fact.
func (u *Uploader) SendPullRequestOpened(fact agent.PullRequestFact) error {
	record, err := NewPullRequestOpenedRecord(u.config.SessionID, u.journal.NextSequence(), u.config.now(), fact)
	if err != nil {
		return err
	}
	return u.SendRecord(record)
}

// Flush delivers the lowest-unacknowledged contiguous batch. Retryable
// failures leave the journal and ack untouched. Permanent HTTP failures are
// durably quarantined and acknowledged, allowing later records to progress.
func (u *Uploader) Flush(ctx context.Context) (FlushResult, error) {
	var result FlushResult
	for {
		pending := u.journal.Pending()
		if len(pending) == 0 {
			return result, nil
		}
		batchRecords, body, err := u.batch(pending)
		if err != nil {
			return result, err
		}
		status, err := u.post(ctx, body)
		if err != nil {
			return result, err
		}
		if status >= 200 && status < 300 {
			if err := u.journal.Ack(batchRecords[len(batchRecords)-1].StructuredSeq); err != nil {
				return result, err
			}
			result.Delivered += len(batchRecords)
			continue
		}
		if isPermanent(status) {
			if err := u.journal.Quarantine(batchRecords, status, fmt.Sprintf("platform returned HTTP %d", status)); err != nil {
				return result, err
			}
			u.config.logger().Warn("execution-event batch quarantined", "status", status, "count", len(batchRecords))
			result.Quarantined += len(batchRecords)
			continue
		}
		return result, fmt.Errorf("executionevent: unexpected HTTP status %d", status)
	}
}

// FlushResult reports records durably delivered or quarantined.
type FlushResult struct {
	Delivered   int
	Quarantined int
}

func (u *Uploader) batch(pending []Record) ([]Record, []byte, error) {
	if len(pending) > MaxBatchRecords {
		pending = pending[:MaxBatchRecords]
	}
	for n := len(pending); n > 0; n-- {
		candidate := Batch{Version: BatchVersion, SessionID: u.config.SessionID, Records: pending[:n]}
		body, err := MarshalCompact(candidate)
		if err != nil {
			return nil, nil, err
		}
		if len(body) <= MaxTransportByte {
			if err := ValidateBatch(candidate); err != nil {
				return nil, nil, err
			}
			return pending[:n], body, nil
		}
	}
	return nil, nil, fmt.Errorf("executionevent: first pending batch exceeds %d bytes", MaxTransportByte)
}

func (u *Uploader) post(ctx context.Context, body []byte) (int, error) {
	endpoint, err := SessionEndpoint(u.config.BaseURL, u.config.SessionID)
	if err != nil {
		return 0, err
	}
	backoff := u.config.initialBackoff()
	for attempt := 0; attempt <= u.config.retries(); attempt++ {
		credentials := RuntimeCredentials{AuthToken: u.config.AuthToken}
		if u.config.CredentialProvider != nil {
			fresh, providerErr := u.config.CredentialProvider(ctx)
			if providerErr != nil {
				return 0, fmt.Errorf("executionevent: credentials: %w", providerErr)
			}
			if fresh.AuthToken != "" {
				credentials.AuthToken = fresh.AuthToken
			}
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
		if err != nil {
			return 0, err
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json")
		if credentials.AuthToken != "" {
			req.Header.Set("Authorization", "Bearer "+credentials.AuthToken)
		}
		resp, err := u.config.client().Do(req)
		if err == nil {
			_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
			_ = resp.Body.Close()
			if resp.StatusCode >= 200 && resp.StatusCode < 300 {
				return resp.StatusCode, nil
			}
			if isPermanent(resp.StatusCode) {
				return resp.StatusCode, nil
			}
			// An expired credential may become valid after the daemon refreshes
			// it. Give the credential provider a chance on the next attempt;
			// unauthorized is never quarantined as a source-data defect.
			if resp.StatusCode == http.StatusUnauthorized {
				if attempt == u.config.retries() {
					return 0, fmt.Errorf("executionevent: unauthorized after %d attempts", attempt+1)
				}
			} else if resp.StatusCode < 500 && resp.StatusCode != http.StatusRequestTimeout && resp.StatusCode != http.StatusTooManyRequests {
				return resp.StatusCode, nil
			}
			if attempt == u.config.retries() {
				return 0, fmt.Errorf("executionevent: retryable HTTP status %d after %d attempts", resp.StatusCode, attempt+1)
			}
		} else if attempt == u.config.retries() {
			return 0, fmt.Errorf("executionevent: upload request: %w", err)
		}
		select {
		case <-ctx.Done():
			return 0, ctx.Err()
		default:
		}
		if err := u.wait(ctx, backoff); err != nil {
			return 0, err
		}
		if backoff < u.config.maxBackoff() {
			backoff *= 2
			if backoff > u.config.maxBackoff() {
				backoff = u.config.maxBackoff()
			}
		}
	}
	return 0, errors.New("executionevent: unreachable retry loop")
}

func (u *Uploader) wait(ctx context.Context, d time.Duration) error {
	if u.config.Sleep == nil {
		timer := time.NewTimer(d)
		defer timer.Stop()
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-timer.C:
			return nil
		}
	}
	// Test wait functions are intentionally injectable, but cancellation is
	// still checked before and after the bounded callback.
	select {
	case <-ctx.Done():
		return ctx.Err()
	default:
	}
	u.config.sleep(d)
	select {
	case <-ctx.Done():
		return ctx.Err()
	default:
		return nil
	}
}

// Drain flushes all pending records until the context expires.
func (u *Uploader) Drain(ctx context.Context) (FlushResult, error) { return u.Flush(ctx) }

// Stop performs a bounded terminal drain and releases the journal lock.
func (u *Uploader) Stop() (FlushResult, error) {
	ctx, cancel := context.WithTimeout(context.Background(), u.config.stopTimeout())
	defer cancel()
	result, err := u.Drain(ctx)
	closeErr := u.journal.Close()
	if err != nil {
		return result, err
	}
	return result, closeErr
}

func isPermanent(status int) bool {
	switch status {
	case http.StatusBadRequest, http.StatusNotFound, http.StatusConflict, http.StatusRequestEntityTooLarge:
		return true
	default:
		return false
	}
}
