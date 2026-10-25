package a2a

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestFetchAgentCardUsesV1AndStrictProtoJSON(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		if request.Method != http.MethodGet || request.Header.Get(VersionHeader) != ProtocolVersion || request.Header.Get("Accept") != "application/json" {
			t.Errorf("request = %s version=%q accept=%q", request.Method, request.Header.Get(VersionHeader), request.Header.Get("Accept"))
		}
		_ = json.NewEncoder(w).Encode(AgentCard{
			Name: "Fixture agent", Description: "Fixture", Version: "1.2.3",
			SupportedInterfaces: []AgentInterface{{URL: serverURL(request) + "/rpc", ProtocolBinding: ProtocolBindingJSONRPC, ProtocolVersion: "1.0", Tenant: "seat-1"}},
			Capabilities:        AgentCapabilities{},
			DefaultInputModes:   []string{"text/plain"}, DefaultOutputModes: []string{"text/plain"},
			Skills: []AgentSkill{},
		})
	}))
	t.Cleanup(server.Close)
	card, err := FetchAgentCard(context.Background(), server.URL+AgentCardWellKnownPath, server.Client())
	if err != nil {
		t.Fatalf("FetchAgentCard: %v", err)
	}
	if card.Name != "Fixture agent" || card.SupportedInterfaces[0].Tenant != "seat-1" {
		t.Fatalf("card = %+v", card)
	}
}

func TestFetchAgentCardRejectsUnknownFieldsAndHTTPFailures(t *testing.T) {
	t.Parallel()
	t.Run("unknown field", func(t *testing.T) {
		t.Parallel()
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte(`{"name":"x","description":"x","supportedInterfaces":[],"version":"1","capabilities":{},"defaultInputModes":[],"defaultOutputModes":[],"skills":[],"surprise":true}`))
		}))
		t.Cleanup(server.Close)
		_, err := FetchAgentCard(context.Background(), server.URL, server.Client())
		var transportErr *TransportError
		if !errors.As(err, &transportErr) || !strings.Contains(err.Error(), "ProtoJSON") {
			t.Fatalf("error = %#v, want strict Agent Card TransportError", err)
		}
	})

	t.Run("HTTP status", func(t *testing.T) {
		t.Parallel()
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			http.Error(w, "secret upstream detail", http.StatusNotFound)
		}))
		t.Cleanup(server.Close)
		_, err := FetchAgentCard(context.Background(), server.URL, server.Client())
		var transportErr *TransportError
		if !errors.As(err, &transportErr) || transportErr.StatusCode != http.StatusNotFound || strings.Contains(err.Error(), "secret") {
			t.Fatalf("error = %#v, want redacted HTTP TransportError", err)
		}
	})
}

func TestFetchAgentCardDiscoveryErrorCoversAllFailureExits(t *testing.T) {
	t.Parallel()

	t.Run("invalid URL", func(t *testing.T) {
		t.Parallel()
		_, err := FetchAgentCard(context.Background(), "ftp://agent.example/card", nil)
		var discoveryErr *DiscoveryError
		if !errors.As(err, &discoveryErr) {
			t.Fatalf("error = %#v, want DiscoveryError", err)
		}
		if got, want := err.Error(), "a2a discovery: card URL must be an absolute HTTP(S) URL"; got != want {
			t.Fatalf("error text = %q, want %q", got, want)
		}
		var transportErr *TransportError
		if errors.As(err, &transportErr) {
			t.Fatalf("error = %#v, validation failure must not match TransportError", err)
		}
	})

	t.Run("disallowed URL", func(t *testing.T) {
		t.Parallel()
		_, err := FetchAgentCard(context.Background(), "https://agent.example/card#fragment", nil)
		var discoveryErr *DiscoveryError
		if !errors.As(err, &discoveryErr) {
			t.Fatalf("error = %#v, want DiscoveryError", err)
		}
		if got, want := err.Error(), "a2a discovery: card URL must not contain userinfo or a fragment"; got != want {
			t.Fatalf("error text = %q, want %q", got, want)
		}
	})

	t.Run("HTTP execution", func(t *testing.T) {
		t.Parallel()
		sentinel := errors.New("fixture dial failed")
		client := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
			return nil, sentinel
		})}
		_, err := FetchAgentCard(context.Background(), "https://agent.example/card", client)
		var discoveryErr *DiscoveryError
		if !errors.As(err, &discoveryErr) {
			t.Fatalf("error = %#v, want DiscoveryError", err)
		}
		if got, want := err.Error(), "a2a transport: fetch Agent Card failed"; got != want {
			t.Fatalf("error text = %q, want %q", got, want)
		}
		var transportErr *TransportError
		if !errors.As(err, &transportErr) || transportErr.StatusCode != 0 {
			t.Fatalf("error = %#v, want nested TransportError", err)
		}
		if !errors.Is(err, sentinel) {
			t.Fatalf("error = %#v, want cause %v", err, sentinel)
		}
	})

	t.Run("body read", func(t *testing.T) {
		t.Parallel()
		sentinel := errors.New("fixture read failed")
		client := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     make(http.Header),
				Body:       io.NopCloser(errorReader{err: sentinel}),
			}, nil
		})}
		_, err := FetchAgentCard(context.Background(), "https://agent.example/card", client)
		var discoveryErr *DiscoveryError
		if !errors.As(err, &discoveryErr) {
			t.Fatalf("error = %#v, want DiscoveryError", err)
		}
		if got, want := err.Error(), "a2a transport: HTTP 200: read Agent Card failed"; got != want {
			t.Fatalf("error text = %q, want %q", got, want)
		}
		var transportErr *TransportError
		if !errors.As(err, &transportErr) || transportErr.StatusCode != http.StatusOK {
			t.Fatalf("error = %#v, want nested TransportError with status 200", err)
		}
		if !errors.Is(err, sentinel) {
			t.Fatalf("error = %#v, want cause %v", err, sentinel)
		}
	})

	t.Run("oversize", func(t *testing.T) {
		t.Parallel()
		big := bytes.Repeat([]byte("x"), maxResponseBytes+1)
		client := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     make(http.Header),
				Body:       io.NopCloser(bytes.NewReader(big)),
			}, nil
		})}
		_, err := FetchAgentCard(context.Background(), "https://agent.example/card", client)
		var discoveryErr *DiscoveryError
		if !errors.As(err, &discoveryErr) {
			t.Fatalf("error = %#v, want DiscoveryError", err)
		}
		if got, want := err.Error(), "a2a transport: HTTP 200: Agent Card exceeded 4 MiB"; got != want {
			t.Fatalf("error text = %q, want %q", got, want)
		}
		var transportErr *TransportError
		if !errors.As(err, &transportErr) || transportErr.StatusCode != http.StatusOK {
			t.Fatalf("error = %#v, want nested TransportError with status 200", err)
		}
	})

	t.Run("HTTP status", func(t *testing.T) {
		t.Parallel()
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			http.Error(w, "secret upstream detail", http.StatusNotFound)
		}))
		t.Cleanup(server.Close)
		_, err := FetchAgentCard(context.Background(), server.URL, server.Client())
		var discoveryErr *DiscoveryError
		if !errors.As(err, &discoveryErr) {
			t.Fatalf("error = %#v, want DiscoveryError", err)
		}
		if got, want := err.Error(), "a2a transport: HTTP 404: Not Found"; got != want {
			t.Fatalf("error text = %q, want %q", got, want)
		}
		var transportErr *TransportError
		if !errors.As(err, &transportErr) || transportErr.StatusCode != http.StatusNotFound {
			t.Fatalf("error = %#v, want nested TransportError with status 404", err)
		}
		if strings.Contains(err.Error(), "secret") {
			t.Fatalf("error = %q, must not leak upstream detail", err.Error())
		}
	})

	t.Run("invalid ProtoJSON", func(t *testing.T) {
		t.Parallel()
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte(`{"name":"x","description":"x","supportedInterfaces":[],"version":"1","capabilities":{},"defaultInputModes":[],"defaultOutputModes":[],"skills":[],"surprise":true}`))
		}))
		t.Cleanup(server.Close)
		_, err := FetchAgentCard(context.Background(), server.URL, server.Client())
		var discoveryErr *DiscoveryError
		if !errors.As(err, &discoveryErr) {
			t.Fatalf("error = %#v, want DiscoveryError", err)
		}
		if got, want := err.Error(), "a2a transport: HTTP 200: Agent Card was not valid A2A v1 ProtoJSON"; got != want {
			t.Fatalf("error text = %q, want %q", got, want)
		}
		var transportErr *TransportError
		if !errors.As(err, &transportErr) || transportErr.StatusCode != http.StatusOK || transportErr.Cause == nil {
			t.Fatalf("error = %#v, want nested TransportError preserving decode cause", err)
		}
	})
}

func serverURL(request *http.Request) string {
	return "http://" + request.Host
}
