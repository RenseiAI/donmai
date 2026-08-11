package agent

import "testing"

// TestValidateEndpointBindingBaseURL exercises the fail-closed shape check
// the dispatch-wire boundary applies to EndpointBinding.BaseURL /
// SessionEndpointBinding.BaseURL before a dispatched binding is trusted:
// absolute http(s) only, no userinfo, https required for any non-loopback
// host. Loopback keeps working over plain http (matching the daemon's own
// local-only control API and pi's HostGateway loopback rule) while an
// external aggregator base URL must be https.
func TestValidateEndpointBindingBaseURL(t *testing.T) {
	tests := []struct {
		name    string
		baseURL string
		wantErr bool
	}{
		{"empty is valid — no override", "", false},
		{"https external aggregator accepted", "https://ai-gateway.example.com/v1", false},
		{"http localhost accepted", "http://localhost:7734", false},
		{"http loopback IP accepted", "http://127.0.0.1:7734", false},
		{"https loopback accepted", "https://localhost:7734", false},
		{"http external rejected", "http://ai-gateway.example.com/v1", true},
		{"non-http(s) scheme rejected", "ftp://example.com/v1", true},
		{"relative URL rejected (not absolute)", "/v1/models", true},
		{"missing host rejected", "https:///v1", true},
		{"userinfo rejected", "https://user:pass@ai-gateway.example.com/v1", true},
		{"unparseable URL rejected", "https://exa mple.com/v1", true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateEndpointBindingBaseURL(tc.baseURL)
			if tc.wantErr && err == nil {
				t.Fatalf("ValidateEndpointBindingBaseURL(%q) = nil error, want a rejection", tc.baseURL)
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("ValidateEndpointBindingBaseURL(%q) = %v, want nil", tc.baseURL, err)
			}
			if err == nil {
				return
			}
			bindingErr, ok := err.(*EndpointBindingError)
			if !ok {
				t.Fatalf("error is not a typed *EndpointBindingError: %v (%T)", err, err)
			}
			if bindingErr.Code != EndpointBindingDenialMalformedBaseURL {
				t.Errorf("Code = %q, want %q", bindingErr.Code, EndpointBindingDenialMalformedBaseURL)
			}
			if bindingErr.Field != "baseUrl" {
				t.Errorf("Field = %q, want %q", bindingErr.Field, "baseUrl")
			}
			// SpecAdmissionError-family style: no raw URL value leaks into the
			// rejection (Detail is a fixed reason string, never the input).
			if bindingErr.Detail == tc.baseURL {
				t.Errorf("Detail leaked the raw baseURL value: %q", bindingErr.Detail)
			}
		})
	}
}
