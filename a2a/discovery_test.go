package a2a

import (
	"context"
	"encoding/json"
	"errors"
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

func serverURL(request *http.Request) string {
	return "http://" + request.Host
}
