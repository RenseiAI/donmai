package opencode

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/RenseiAI/donmai/agent"
)

// ─── extractVersion / compareVersions (pure helpers) ───────────────────────

func TestExtractVersion(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name   string
		raw    string
		want   string
		wantOK bool
	}{
		{"bare version", "1.17.18", "1.17.18", true},
		{"labeled version", "opencode 1.17.18", "1.17.18", true},
		{"leading noise", "opencode-ai/1.17.18 darwin-arm64", "1.17.18", true},
		{"trailing newline", "1.17.18\n", "1.17.18", true},
		{"no version", "not a version string", "", false},
		{"empty", "", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, ok := extractVersion(tc.raw)
			if ok != tc.wantOK || got != tc.want {
				t.Errorf("extractVersion(%q) = (%q, %v), want (%q, %v)", tc.raw, got, ok, tc.want, tc.wantOK)
			}
		})
	}
}

func TestCompareVersions(t *testing.T) {
	t.Parallel()
	cases := []struct {
		a, b string
		want int
	}{
		{"1.17.18", "1.17.18", 0},
		{"1.17.17", "1.17.18", -1},
		{"1.17.19", "1.17.18", 1},
		{"1.18.0", "1.17.99", 1},
		{"2.0.0", "1.99.99", 1},
		{"1.17", "1.17.0", 0},
		{"1.17.0", "1.17", 0},
	}
	for _, tc := range cases {
		if got := compareVersions(tc.a, tc.b); got != tc.want {
			t.Errorf("compareVersions(%q, %q) = %d, want %d", tc.a, tc.b, got, tc.want)
		}
	}
}

// ─── checkVersionPin ────────────────────────────────────────────────────────

func TestCheckVersionPin(t *testing.T) {
	t.Parallel()

	fakeProbe := func(raw string, err error) versionProbeFunc {
		return func(context.Context, string) (string, error) { return raw, err }
	}

	t.Run("within range: verified, no error", func(t *testing.T) {
		t.Parallel()
		unverified, err := checkVersionPin(context.Background(), fakeProbe(MinVersion, nil), "opencode")
		if err != nil {
			t.Fatalf("checkVersionPin: %v", err)
		}
		if unverified {
			t.Error("unverified: want false for a version == MinVersion == VerifiedAgainst")
		}
	})

	t.Run("below MinVersion: hard failure", func(t *testing.T) {
		t.Parallel()
		_, err := checkVersionPin(context.Background(), fakeProbe("0.1.0", nil), "opencode")
		if !errors.Is(err, agent.ErrProviderUnavailable) {
			t.Fatalf("checkVersionPin err = %v, want wrapping agent.ErrProviderUnavailable", err)
		}
	})

	t.Run("above VerifiedAgainst: unverified, non-fatal", func(t *testing.T) {
		t.Parallel()
		unverified, err := checkVersionPin(context.Background(), fakeProbe("99.0.0", nil), "opencode")
		if err != nil {
			t.Fatalf("checkVersionPin: unexpected error: %v", err)
		}
		if !unverified {
			t.Error("unverified: want true for a version above VerifiedAgainst")
		}
	})

	t.Run("probe error: unverified, non-fatal", func(t *testing.T) {
		t.Parallel()
		unverified, err := checkVersionPin(context.Background(), fakeProbe("", errors.New("exec: not found")), "opencode")
		if err != nil {
			t.Fatalf("checkVersionPin: unexpected error: %v", err)
		}
		if !unverified {
			t.Error("unverified: want true when the probe itself fails (label, don't block)")
		}
	})

	t.Run("unparseable output: unverified, non-fatal", func(t *testing.T) {
		t.Parallel()
		unverified, err := checkVersionPin(context.Background(), fakeProbe("no version here", nil), "opencode")
		if err != nil {
			t.Fatalf("checkVersionPin: unexpected error: %v", err)
		}
		if !unverified {
			t.Error("unverified: want true when the version string can't be parsed")
		}
	})
}

// ─── New() integration: version-pin enforcement in CLI mode ───────────────

func TestNew_CLIMode_VersionBelowMin_ConstructionFails(t *testing.T) {
	t.Parallel()
	p, err := New(Options{
		LookPath: fakeLookPath(map[string]string{DefaultBinary: "/usr/local/bin/opencode"}),
		Getenv:   fakeEnv(nil),
		VersionProbe: func(context.Context, string) (string, error) {
			return "0.0.1", nil
		},
	})
	if p != nil {
		t.Errorf("New: want nil provider on a below-minimum pinned version, got %+v", p)
	}
	if !errors.Is(err, agent.ErrProviderUnavailable) {
		t.Fatalf("New err = %v, want wrapping agent.ErrProviderUnavailable", err)
	}
}

func TestNew_CLIMode_VersionAboveVerified_ConstructionSucceedsUnverified(t *testing.T) {
	t.Parallel()
	p, err := New(Options{
		LookPath: fakeLookPath(map[string]string{DefaultBinary: "/usr/local/bin/opencode"}),
		Getenv:   fakeEnv(nil),
		VersionProbe: func(context.Context, string) (string, error) {
			return "99.9.9", nil
		},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if !p.versionUnverified {
		t.Error("versionUnverified: want true for a version above VerifiedAgainst")
	}
}

func TestNew_CLIMode_VersionWithinRange_ConstructionSucceedsVerified(t *testing.T) {
	t.Parallel()
	p, err := New(Options{
		LookPath: fakeLookPath(map[string]string{DefaultBinary: "/usr/local/bin/opencode"}),
		Getenv:   fakeEnv(nil),
		VersionProbe: func(context.Context, string) (string, error) {
			return PinnedVersion, nil
		},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if p.versionUnverified {
		t.Error("versionUnverified: want false for the exact pinned version")
	}
}

func TestNew_CLIMode_SkipVersionCheck_BypassesEnforcement(t *testing.T) {
	t.Parallel()
	called := false
	p, err := New(Options{
		LookPath: fakeLookPath(map[string]string{DefaultBinary: "/usr/local/bin/opencode"}),
		Getenv:   fakeEnv(nil),
		VersionProbe: func(context.Context, string) (string, error) {
			called = true
			return "0.0.1", nil // would fail construction if consulted
		},
		SkipVersionCheck: true,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if called {
		t.Error("VersionProbe: should not be consulted when SkipVersionCheck is set")
	}
	if p.versionUnverified {
		t.Error("versionUnverified: want false when the check is skipped entirely")
	}
}

// ─── Spawn: unverified-version SystemEvent, emitted once per session ──────

func TestProvider_Spawn_UnverifiedVersion_EmitsSystemEventOnce(t *testing.T) {
	t.Parallel()

	scriptPath := writeFakeOpenCodeScript(t)
	p := &Provider{binary: scriptPath, versionUnverified: true}

	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()

	h, err := p.Spawn(ctx, agent.Spec{Prompt: "list files"})
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	defer func() { _ = h.Stop(context.Background()) }()

	events := collectUntilResult(ctx, t, h)

	count := 0
	for _, ev := range events {
		sysEv, ok := ev.(agent.SystemEvent)
		if !ok {
			continue
		}
		if sysEv.Subtype == unverifiedVersionSubtype {
			count++
		}
	}
	if count != 1 {
		t.Errorf("unverified_harness_version SystemEvent count = %d, want exactly 1", count)
	}
}

func TestProvider_Spawn_VerifiedVersion_NoSystemEvent(t *testing.T) {
	t.Parallel()

	scriptPath := writeFakeOpenCodeScript(t)
	p := &Provider{binary: scriptPath, versionUnverified: false}

	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()

	h, err := p.Spawn(ctx, agent.Spec{Prompt: "list files"})
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	defer func() { _ = h.Stop(context.Background()) }()

	events := collectUntilResult(ctx, t, h)
	for _, ev := range events {
		if sysEv, ok := ev.(agent.SystemEvent); ok && sysEv.Subtype == unverifiedVersionSubtype {
			t.Error("unexpected unverified_harness_version SystemEvent on a verified-pin session")
		}
	}
}
