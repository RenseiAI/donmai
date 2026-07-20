package workarea

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

type receiverConfig struct {
	ReceiverKey string `json:"receiverKey"`
	Endpoint    string `json:"endpoint"`
}

// ReceiverRegistry is durable receiver configuration, separate from immutable
// outbox records. Endpoint rotation updates this registry without changing the
// opaque receiver key or retained body bytes.
type ReceiverRegistry struct {
	dir string
}

func newReceiverRegistry(dir string) (*ReceiverRegistry, error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(abs, 0o750); err != nil {
		return nil, err
	}
	if err := syncDir(abs); err != nil {
		return nil, err
	}
	registry := &ReceiverRegistry{dir: abs}
	if _, err := registry.List(); err != nil {
		return nil, err
	}
	return registry, nil
}

// Register creates or rotates one receiver endpoint while preserving its key.
func (r *ReceiverRegistry) Register(receiverKey, endpoint string) error {
	if err := validateGeneratedID(receiverKey, "rcv_"); err != nil {
		return err
	}
	parsed, err := url.Parse(endpoint)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return errors.New("runtime/workarea: receiver endpoint must be an absolute HTTP(S) URL")
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return errors.New("runtime/workarea: receiver endpoint must use HTTP or HTTPS")
	}
	config := receiverConfig{ReceiverKey: receiverKey, Endpoint: parsed.String()}
	data, err := json.Marshal(config)
	if err != nil {
		return err
	}
	return writeFileAtomic(r.dir, filepath.Join(r.dir, receiverKey+".json"), ".receiver-*.tmp", data)
}

// Resolve returns only the endpoint configured for the exact key. It never
// falls back to another receiver.
func (r *ReceiverRegistry) Resolve(receiverKey string) (string, error) {
	if err := validateGeneratedID(receiverKey, "rcv_"); err != nil {
		return "", err
	}
	data, err := os.ReadFile(filepath.Join(r.dir, receiverKey+".json"))
	if err != nil {
		return "", fmt.Errorf("runtime/workarea: resolve receiver %s: %w", receiverKey, err)
	}
	var config receiverConfig
	if err := json.Unmarshal(data, &config); err != nil {
		return "", fmt.Errorf("runtime/workarea: decode receiver %s: %w", receiverKey, err)
	}
	if config.ReceiverKey != receiverKey || strings.TrimSpace(config.Endpoint) == "" {
		return "", errors.New("runtime/workarea: receiver configuration identity mismatch")
	}
	return config.Endpoint, nil
}

// List implements the documented terminal-workarea contract.
func (r *ReceiverRegistry) List() ([]string, error) {
	entries, err := os.ReadDir(r.dir)
	if err != nil {
		return nil, err
	}
	keys := make([]string, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".json" {
			continue
		}
		key := strings.TrimSuffix(entry.Name(), ".json")
		if _, err := r.Resolve(key); err != nil {
			return nil, err
		}
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys, nil
}

// ReceiverAuthorizationResolver returns fresh ephemeral authorization for one
// send. Nothing returned by it is persisted in either authority.
type ReceiverAuthorizationResolver func(context.Context, string) (string, error)

// HTTPSender resolves the current endpoint and fresh authorization on every
// send. Missing resolution fails the same record and never selects a fallback.
func (r *ReceiverRegistry) HTTPSender(client *http.Client, auth ReceiverAuthorizationResolver) TerminalStatusSender {
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second}
	}
	return func(ctx context.Context, receiverKey string, body []byte) error {
		endpoint, err := r.Resolve(receiverKey)
		if err != nil {
			return err
		}
		var authorization string
		if auth != nil {
			authorization, err = auth(ctx, receiverKey)
			if err != nil {
				return fmt.Errorf("runtime/workarea: resolve receiver authorization: %w", err)
			}
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
		if err != nil {
			return fmt.Errorf("runtime/workarea: build receiver request: %w", err)
		}
		req.Header.Set("Content-Type", "application/json")
		if authorization != "" {
			req.Header.Set("Authorization", authorization)
		}
		resp, err := client.Do(req)
		if err != nil {
			return fmt.Errorf("runtime/workarea: send receiver request: %w", err)
		}
		defer func() { _ = resp.Body.Close() }()
		_, _ = io.Copy(io.Discard, resp.Body)
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			return fmt.Errorf("runtime/workarea: receiver returned HTTP %d", resp.StatusCode)
		}
		return nil
	}
}
