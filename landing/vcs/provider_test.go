package vcs

import (
	"errors"
	"strings"
	"testing"
)

func TestUnsupportedOperationError(t *testing.T) {
	err := &UnsupportedOperationError{Capability: "HasMergeQueue", ProviderID: "atomic"}
	if !strings.Contains(err.Error(), "atomic") {
		t.Errorf("Error() missing provider id: %q", err.Error())
	}
	if !strings.Contains(err.Error(), "HasMergeQueue") {
		t.Errorf("Error() missing capability: %q", err.Error())
	}
}

func TestAssertCapability(t *testing.T) {
	base := GitHubCapabilities // all gated flags true except those listed false

	tests := []struct {
		name    string
		caps    Capabilities
		cap     string
		wantErr bool
	}{
		{"HasPullRequests true", base, "HasPullRequests", false},
		{"HasMergeQueue true", base, "HasMergeQueue", false},
		{"SupportsAttest true", base, "SupportsAttest", false},
		{"HasMergeQueue false on atomic", AtomicCapabilities, "HasMergeQueue", true},
		{"HasPullRequests false on atomic", AtomicCapabilities, "HasPullRequests", true},
		{"unknown capability errors", base, "NoSuchFlag", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := AssertCapability(tt.caps, tt.cap, "p")
			if (err != nil) != tt.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tt.wantErr)
			}
			if tt.wantErr {
				var ue *UnsupportedOperationError
				if !errors.As(err, &ue) {
					t.Errorf("err = %v, want *UnsupportedOperationError", err)
				}
			}
		})
	}
}

// Compile-time + runtime guard that both adapters satisfy Provider.
func TestProvidersImplementInterface(t *testing.T) {
	providers := []Provider{
		NewGitHubProvider(GitHubOpts{}),
		NewAtomicProvider(),
	}
	for _, p := range providers {
		if p.Name() == "" {
			t.Errorf("provider has empty name")
		}
		_ = p.Capabilities()
	}
}
