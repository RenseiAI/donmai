package codeintel

// cohere.go is a minimal, stdlib-only Go HTTP client for the Cohere Rerank
// v2 API, used to optionally re-order Voyage-rescored hybrid search
// candidates (see hybrid.go). Like voyage.go, it does not retry: any
// failure is treated by the caller as "keep the Voyage-scored order",
// never a hard error.
//
// API reference: https://docs.cohere.com/reference/rerank

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
)

// cohereAPIURL is a package-level var (not a const) so tests can point it at
// an httptest server. Production code never mutates it.
var cohereAPIURL = "https://api.cohere.com/v2/rerank"

// cohereModel is Cohere's current general-purpose cross-encoder reranker.
const cohereModel = "rerank-v3.5"

type cohereRerankRequest struct {
	Model     string   `json:"model"`
	Query     string   `json:"query"`
	Documents []string `json:"documents"`
	TopN      int      `json:"top_n"`
}

type cohereRerankResultItem struct {
	Index          int     `json:"index"`
	RelevanceScore float64 `json:"relevance_score"`
}

type cohereRerankResponse struct {
	Results []cohereRerankResultItem `json:"results"`
}

// cohereRerankResult pairs a candidate index with its reranked relevance
// score.
type cohereRerankResult struct {
	Index int
	Score float64
}

// cohereRerank calls the Cohere v2 rerank API over the given documents
// (already-built candidate text, in candidate order) and returns one result
// per document with the index it corresponds to in the input slice. The
// response is not guaranteed to preserve input order — callers must key off
// Index.
func cohereRerank(ctx context.Context, httpClient *http.Client, apiKey string, query string, documents []string) ([]cohereRerankResult, error) {
	if len(documents) == 0 {
		return nil, nil
	}

	reqBody, err := json.Marshal(cohereRerankRequest{
		Model:     cohereModel,
		Query:     query,
		Documents: documents,
		TopN:      len(documents),
	})
	if err != nil {
		return nil, fmt.Errorf("cohere: encode request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, cohereAPIURL, bytes.NewReader(reqBody))
	if err != nil {
		return nil, fmt.Errorf("cohere: build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+apiKey)

	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("cohere: request failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		// Never echo the response body — see voyage.go for rationale.
		_, _ = io.Copy(io.Discard, resp.Body)
		return nil, fmt.Errorf("cohere: unexpected status %d", resp.StatusCode)
	}

	var parsed cohereRerankResponse
	if err := json.NewDecoder(resp.Body).Decode(&parsed); err != nil {
		return nil, fmt.Errorf("cohere: decode response: %w", err)
	}

	out := make([]cohereRerankResult, 0, len(parsed.Results))
	for _, r := range parsed.Results {
		out = append(out, cohereRerankResult{Index: r.Index, Score: r.RelevanceScore})
	}
	return out, nil
}
