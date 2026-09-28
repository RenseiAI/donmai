package attachclient

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"path"
	"strings"
	"sync"
	"time"

	"github.com/RenseiAI/donmai/attachwire"
)

const (
	continuationUploadTimeout  = 30 * time.Second
	continuationUploadAttempts = 2
)

type continuationUploadState struct {
	source    ContinuationSource
	attachURL string
	client    *http.Client
	logger    *slog.Logger
	mu        sync.Mutex
	requestID string
	busy      bool
	payload   []byte
	epoch     uint64
}

type continuationContextKey struct{}

func continuationSchemas(source ContinuationSource) []string {
	if source != nil && source.SupportsContinuation() {
		return []string{attachwire.ContinuationSchema}
	}
	return nil
}

func (h *host) continuationSource() ContinuationSource {
	if h.cfg.ContinuationSource != nil {
		if h.cfg.ContinuationSource.SupportsContinuation() {
			return h.cfg.ContinuationSource
		}
		return nil
	}
	source, ok := h.cfg.Session.(ContinuationSource)
	if ok && source.SupportsContinuation() {
		return source
	}
	return nil
}

func newContinuationUploadState(source ContinuationSource, attachURL string, client *http.Client, logger *slog.Logger) *continuationUploadState {
	if len(continuationSchemas(source)) == 0 {
		return nil
	}
	if client == nil {
		client = http.DefaultClient
	}
	if logger == nil {
		logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	return &continuationUploadState{source: source, attachURL: attachURL, client: client, logger: logger}
}

func (h *host) continuationContext(ctx context.Context) context.Context {
	state := newContinuationUploadState(h.continuationSource(), h.cfg.AttachURL, h.httpClient(), h.log)
	if state == nil {
		return ctx
	}
	return context.WithValue(ctx, continuationContextKey{}, state)
}

// Format prevents checkpoint bytes and credentials from being exposed by logs.
func (s *continuationUploadState) Format(state fmt.State, _ rune) {
	_, _ = fmt.Fprint(state, "<continuation-upload-state>")
}

func (s *continuationUploadState) start(ctx context.Context, request attachwire.ContinuationRequest) {
	if err := request.Validate(); err != nil {
		s.logger.Warn("continuation request refused", "code", "invalid_request")
		return
	}
	s.mu.Lock()
	if s.busy {
		s.mu.Unlock()
		return
	}
	if s.requestID != request.RequestID {
		s.payload = nil
		s.requestID = request.RequestID
	}
	s.busy = true
	payload, epoch := s.payload, s.epoch
	s.mu.Unlock()
	go func() {
		defer func() { s.mu.Lock(); s.busy = false; s.mu.Unlock() }()
		bounded, cancel := context.WithTimeout(ctx, continuationUploadTimeout)
		defer cancel()
		if len(payload) == 0 {
			checkpoint, err := s.source.InspectContinuation(bounded, request.Schema)
			if err != nil {
				s.logger.Warn("continuation capture unavailable", "requestId", request.RequestID, "code", "capture_failed")
				return
			}
			payload, err = checkpoint.Encode(request.Schema)
			if err != nil {
				s.logger.Warn("continuation capture unavailable", "requestId", request.RequestID, "code", "invalid_checkpoint")
				return
			}
			epoch = checkpoint.Epoch
			s.mu.Lock()
			s.payload = payload
			s.epoch = epoch
			s.mu.Unlock()
		}
		if err := uploadContinuation(bounded, s.client, s.attachURL, request, epoch, payload); err != nil {
			s.logger.Warn("continuation upload unavailable", "requestId", request.RequestID, "code", "upload_failed")
		}
	}()
}

func continuationUploadURL(attachURL, requestID string) (string, error) {
	parsed, err := url.Parse(attachURL)
	if err != nil {
		return "", fmt.Errorf("attachclient: continuation URL: %w", err)
	}
	if parsed.User != nil || parsed.Host == "" {
		return "", fmt.Errorf("attachclient: invalid continuation origin")
	}
	switch parsed.Scheme {
	case "wss":
		parsed.Scheme = "https"
	case "ws":
		parsed.Scheme = "http"
	case "https", "http":
	default:
		return "", fmt.Errorf("attachclient: invalid continuation scheme")
	}
	index := strings.LastIndex(parsed.Path, "/rooms/")
	if index < 0 {
		return "", fmt.Errorf("attachclient: continuation room path missing")
	}
	room := strings.TrimPrefix(parsed.Path[index:], "/rooms/")
	if room == "" || strings.Contains(room, "/") {
		return "", fmt.Errorf("attachclient: invalid continuation room path")
	}
	prefix := path.Dir(parsed.Path[:index])
	parsed.Path = path.Join(prefix, "v3", "rooms", room, "continuation", requestID)
	parsed.RawPath = ""
	parsed.RawQuery = ""
	parsed.Fragment = ""
	return parsed.String(), nil
}

func uploadContinuation(ctx context.Context, client *http.Client, attachURL string, request attachwire.ContinuationRequest, epoch uint64, payload []byte) error {
	if err := request.Validate(); err != nil {
		return err
	}
	if len(payload) == 0 || len(payload) > attachwire.MaxContinuationBytes {
		return fmt.Errorf("attachclient: invalid continuation upload")
	}
	endpoint, err := continuationUploadURL(attachURL, request.RequestID)
	if err != nil {
		return err
	}
	if client == nil {
		client = http.DefaultClient
	}
	safeClient := *client
	safeClient.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	digest := sha256.Sum256(payload)
	digestHex := hex.EncodeToString(digest[:])
	total := uint32(uint64(len(payload)) & 0xffffffff)
	for offset := 0; offset < len(payload); offset += attachwire.ContinuationChunkBytes {
		if offset < 0 || offset > attachwire.MaxContinuationBytes {
			return fmt.Errorf("attachclient: continuation offset exceeded bound")
		}
		end := min(offset+attachwire.ContinuationChunkBytes, len(payload))
		chunk := attachwire.ContinuationChunk{Schema: request.Schema, RequestID: request.RequestID, HostEpoch: epoch, Offset: uint32(offset), Total: total, SHA256: digestHex, Data: payload[offset:end]}
		if err := chunk.Validate(); err != nil {
			return err
		}
		body, err := json.Marshal(chunk)
		if err != nil {
			return fmt.Errorf("attachclient: encode continuation upload: %w", err)
		}
		if err := postContinuationChunk(ctx, &safeClient, endpoint, request.UploadGrant, body, uint32(uint64(end)&0xffffffff), end == len(payload)); err != nil {
			return err
		}
	}
	return nil
}

func postContinuationChunk(ctx context.Context, client *http.Client, endpoint, grant string, body []byte, next uint32, complete bool) error {
	for attempt := 0; attempt < continuationUploadAttempts; attempt++ {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
		if err != nil {
			return fmt.Errorf("attachclient: continuation POST: %w", err)
		}
		req.Header.Set(attachwire.ContinuationUploadGrantHeader, grant)
		req.Header.Set("Content-Type", "application/json")
		response, err := client.Do(req)
		retry := false
		if err != nil {
			retry = true
		} else {
			data, readErr := io.ReadAll(io.LimitReader(response.Body, 2049))
			_ = response.Body.Close()
			if response.StatusCode >= 200 && response.StatusCode < 300 {
				if readErr != nil {
					return fmt.Errorf("attachclient: continuation acknowledgement: %w", readErr)
				}
				ack, decodeErr := attachwire.DecodeContinuationChunkAck(data)
				if decodeErr != nil || ack.NextOffset != next || ack.Complete != complete {
					return fmt.Errorf("attachclient: continuation acknowledgement mismatch")
				}
				return nil
			}
			retry = response.StatusCode == http.StatusTooManyRequests || response.StatusCode >= 500
			if !retry {
				return fmt.Errorf("attachclient: continuation POST refused with status %d", response.StatusCode)
			}
		}
		if !retry || attempt+1 == continuationUploadAttempts {
			return fmt.Errorf("attachclient: continuation upload attempts exhausted")
		}
		timer := time.NewTimer(100 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
	return fmt.Errorf("attachclient: continuation upload incomplete")
}
