package codeintel

// voyage.go is a minimal, stdlib-only Go HTTP client for the Voyage AI
// embeddings API, used to rescore BM25 top-K candidates for hybrid code
// search (see hybrid.go). It intentionally does NOT retry — the caller
// (applyHybridSearch) treats any failure as "fall back to BM25" rather than
// spending extra wall-clock time on backoff, per the hardening constraint
// that hybrid search must never hang or spend unbounded time.
//
// API reference: https://docs.voyageai.com/reference/embeddings-api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
)

// voyageAPIURL is a package-level var (not a const) so tests can point it at
// an httptest server. Production code never mutates it.
var voyageAPIURL = "https://api.voyageai.com/v1/embeddings"

// voyageModel is the embedding model used for code search. voyage-code-3 is
// Voyage's current code-optimized embedding model per their public API docs.
const voyageModel = "voyage-code-3"

// voyageEmbedDimensions uses Matryoshka truncation to keep vectors small —
// cosine re-scoring over ~40 candidates doesn't need the full 2048-dim
// native output.
const voyageEmbedDimensions = 256

type voyageRequest struct {
	Model           string   `json:"model"`
	Input           []string `json:"input"`
	InputType       string   `json:"input_type"`
	OutputDimension int      `json:"output_dimension,omitempty"`
}

type voyageResponseDatum struct {
	Embedding []float32 `json:"embedding"`
	Index     int       `json:"index"`
}

type voyageResponse struct {
	Data []voyageResponseDatum `json:"data"`
}

// voyageEmbed calls the Voyage embeddings API for a batch of texts sharing a
// single input_type ("query" or "document") and returns one vector per
// input text, in the same order as texts. It performs no retries: any
// non-2xx status or transport error is returned immediately so the caller
// can fall back to BM25 within its timeout budget.
func voyageEmbed(ctx context.Context, httpClient *http.Client, apiKey string, texts []string, inputType string) ([][]float32, error) {
	if len(texts) == 0 {
		return nil, nil
	}

	reqBody, err := json.Marshal(voyageRequest{
		Model:           voyageModel,
		Input:           texts,
		InputType:       inputType,
		OutputDimension: voyageEmbedDimensions,
	})
	if err != nil {
		return nil, fmt.Errorf("voyage: encode request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, voyageAPIURL, bytes.NewReader(reqBody))
	if err != nil {
		return nil, fmt.Errorf("voyage: build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+apiKey)

	resp, err := httpClient.Do(req)
	if err != nil {
		// Do not include err's string verbatim beyond the transport-level
		// message — it never contains request headers/body, only dial/TLS
		// details, so this is safe to surface.
		return nil, fmt.Errorf("voyage: request failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		// Deliberately do not echo the response body: some providers echo
		// request metadata back in error payloads, and we never want that
		// anywhere near a log line. Status code is enough to diagnose.
		_, _ = io.Copy(io.Discard, resp.Body)
		return nil, fmt.Errorf("voyage: unexpected status %d", resp.StatusCode)
	}

	var parsed voyageResponse
	if err := json.NewDecoder(resp.Body).Decode(&parsed); err != nil {
		return nil, fmt.Errorf("voyage: decode response: %w", err)
	}

	out := make([][]float32, len(texts))
	for _, d := range parsed.Data {
		if d.Index < 0 || d.Index >= len(out) {
			continue
		}
		out[d.Index] = d.Embedding
	}
	for i, v := range out {
		if v == nil {
			return nil, fmt.Errorf("voyage: response missing embedding for input %d", i)
		}
	}
	return out, nil
}
