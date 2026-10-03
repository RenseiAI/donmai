package providerretry

import "testing"

func TestStatusRetryable(t *testing.T) {
	cases := map[int]bool{
		400: false, 401: false, 403: false, 404: false, 413: false, 422: false,
		408: true, 409: true, 429: true,
		500: true, 502: true, 503: true, 504: true, 529: true,
		0: true, 200: true, 302: true,
	}
	for status, want := range cases {
		if got := StatusRetryable(status); got != want {
			t.Errorf("StatusRetryable(%d) = %v; want %v", status, got, want)
		}
	}
}

// TestTextStatus pins the anchored forms a status is read from, and that a
// port, an address or a number in prose is never one.
func TestTextStatus(t *testing.T) {
	cases := []struct {
		text   string
		status int // 0 = none
	}{
		{text: "400 Bad Request", status: 400},
		{text: "  503 Service Unavailable", status: 503},
		{text: `400 {"type":"error","error":{"type":"invalid_request_error"}}`, status: 400},
		{text: "429: rate limited", status: 429},
		{text: "404", status: 404},
		{text: "request failed: status 401", status: 401},
		{text: "status=403 forbidden", status: 403},
		{text: "statusCode: 422", status: 422},
		{text: "Request failed with status code 400", status: 400},
		{text: "upstream returned HTTP 502", status: 502},
		{text: "HTTP/1.1 400 Bad Request", status: 400},
		{text: "HTTP/2 429", status: 429},
		{text: "connect ETIMEDOUT 104.18.32.47:443"},
		{text: "connect ECONNREFUSED 10.0.0.12:443"},
		{text: "dial tcp 10.0.0.12:443: i/o timeout"},
		{text: "timeout after 400 seconds"},
		{text: "stream reset after 404 bytes of the response body"},
		{text: "500:8080 refused"},
		{text: "4000 ms elapsed"},
		{text: "status 4000"},
		{text: "see https://example.com:443/v1"},
		{text: "Error: 400 Bad Request"},
		{text: ""},
	}
	for _, tc := range cases {
		status, ok := TextStatus(tc.text)
		if tc.status == 0 {
			if ok {
				t.Errorf("TextStatus(%q) = %d; want no status", tc.text, status)
			}
			continue
		}
		if !ok || status != tc.status {
			t.Errorf("TextStatus(%q) = %d, %v; want %d", tc.text, status, ok, tc.status)
		}
	}
}

func TestContextOverflow(t *testing.T) {
	for text, want := range map[string]bool{
		`400 {"error":{"message":"prompt is too long: 213456 tokens > 200000 maximum"}}`: true,
		"This model's maximum context length is 128000 tokens.":                          true,
		"400 context_length_exceeded":                                                    true,
		"Input is too long for requested model.":                                         true,
		"the request exceeds the context window":                                         true,
		"400 The request contains invalid parameters":                                    false,
		"connect ECONNREFUSED 10.0.0.12:443":                                             false,
	} {
		if got := ContextOverflow(text); got != want {
			t.Errorf("ContextOverflow(%q) = %v; want %v", text, got, want)
		}
	}
}

func TestTextRetryable(t *testing.T) {
	for text, want := range map[string]bool{
		"400 The request contains invalid parameters":            false,
		"Request failed with status code 401":                    false,
		"400 prompt is too long: 213456 tokens > 200000 maximum": true,
		"408 Request Timeout":                                    true,
		"409 Conflict":                                           true,
		"429 Too Many Requests":                                  true,
		"HTTP 503":                                               true,
		"connect ETIMEDOUT 104.18.32.47:443":                     true,
		"timeout after 400 seconds":                              true,
		"model provider error":                                   true,
	} {
		if got := TextRetryable(text); got != want {
			t.Errorf("TextRetryable(%q) = %v; want %v", text, got, want)
		}
	}
}
