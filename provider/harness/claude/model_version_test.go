package claude

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/RenseiAI/donmai/agent"
)

func syntheticVersionCLI(t *testing.T, body string) string {
	t.Helper()
	return writeFakeCLI(t, "synthetic-claude", body)
}

func TestCheckModelBinaryVersion_Sonnet5Boundary(t *testing.T) {
	for _, tt := range []struct {
		name, output string
		wantOK       bool
	}{
		{name: "below patch", output: "2.1.196 (Claude Code)", wantOK: false},
		{name: "below minor", output: "2.0.999 (Claude Code)", wantOK: false},
		{name: "below major", output: "1.99.999 (Claude Code)", wantOK: false},
		{name: "at threshold", output: "2.1.197 (Claude Code)", wantOK: true},
		{name: "bare threshold", output: "2.1.197", wantOK: true},
		{name: "newer patch", output: "2.1.283 (Claude Code)", wantOK: true},
		{name: "newer minor", output: "2.2.0 (Claude Code)", wantOK: true},
		{name: "newer major", output: "3.0.0 (Claude Code)", wantOK: true},
		{name: "threshold prerelease", output: "2.1.197-rc.1 (Claude Code)", wantOK: false},
		{name: "newer prerelease", output: "2.1.198-beta.1 (Claude Code)", wantOK: false},
		{name: "build suffix", output: "2.1.197+build (Claude Code)", wantOK: false},
		{name: "leading zero", output: "02.1.197 (Claude Code)", wantOK: false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			binary := syntheticVersionCLI(t, "printf '%s\\n' '"+tt.output+"'")
			err := CheckModelBinaryVersion(t.Context(), binary, sonnet5ModelID)
			if tt.wantOK && err != nil {
				t.Fatalf("qualified stable version refused: %v", err)
			}
			if !tt.wantOK && !errors.Is(err, agent.ErrProviderUnavailable) {
				t.Fatalf("unsupported version error = %v, want provider-unavailable", err)
			}
		})
	}
}

func TestCheckModelBinaryVersion_RefusesMissingMalformedAndNoisyEvidence(t *testing.T) {
	for _, tt := range []struct{ name, script string }{
		{name: "empty stdout", script: ":"},
		{name: "malformed", script: "printf 'Claude version unknown\\n'"},
		{name: "warning before version", script: "printf 'warning: stale cache\\n2.1.283 (Claude Code)\\n'"},
		{name: "extra line", script: "printf '2.1.283 (Claude Code)\\nready\\n'"},
		{name: "stderr warning", script: "printf '2.1.283 (Claude Code)\\n'; printf 'warning\\n' >&2"},
		{name: "oversized stdout", script: "printf '" + strings.Repeat("x", modelVersionOutputMax+1) + "'"},
		{name: "oversized stderr", script: "printf '2.1.283 (Claude Code)\\n'; printf '" + strings.Repeat("x", modelVersionOutputMax+1) + "' >&2"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			binary := syntheticVersionCLI(t, tt.script)
			if err := CheckModelBinaryVersion(t.Context(), binary, sonnet5ModelID); !errors.Is(err, agent.ErrProviderUnavailable) {
				t.Fatalf("untrusted output accepted: %v", err)
			}
		})
	}
	if err := CheckModelBinaryVersion(t.Context(), filepath.Join(t.TempDir(), "missing"), sonnet5ModelID); !errors.Is(err, agent.ErrProviderUnavailable) {
		t.Fatalf("missing CLI accepted: %v", err)
	}
	if err := CheckModelBinaryVersion(t.Context(), "relative-claude", sonnet5ModelID); !errors.Is(err, agent.ErrProviderUnavailable) {
		t.Fatalf("relative CLI path accepted: %v", err)
	}
}

func TestCheckModelBinaryVersion_ProbeIsIsolatedAndBounded(t *testing.T) {
	t.Setenv("GITHUB_TOKEN", "synthetic-never-pass-to-cli")
	t.Setenv("NODE_OPTIONS", "synthetic-never-pass-to-cli")
	binary := syntheticVersionCLI(t, `
[ "$1" = "--version" ] || exit 21
[ -z "${GITHUB_TOKEN+x}" ] || exit 22
[ -z "${NODE_OPTIONS+x}" ] || exit 23
[ "$(cd "$HOME" && pwd -P)" = "$(pwd -P)" ] || exit 24
[ ! -e .git ] || exit 25
printf '2.1.197 (Claude Code)\n'
`)
	if err := CheckModelBinaryVersion(t.Context(), binary, sonnet5ModelID); err != nil {
		t.Fatalf("isolated version probe: %v", err)
	}
	busy := syntheticVersionCLI(t, "while :; do :; done")
	ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
	defer cancel()
	start := time.Now()
	if err := CheckModelBinaryVersion(ctx, busy, sonnet5ModelID); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("timed-out probe error = %v, want deadline", err)
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("timed-out probe held caller for %s", elapsed)
	}
}

func TestCheckModelBinaryVersion_DoesNotLeaveWrapperDescendant(t *testing.T) {
	binary := syntheticVersionCLI(t, `
(sleep 0.4; printf 'survived' > "$0.child-survived") >/dev/null 2>&1 &
printf '2.1.197 (Claude Code)\n'
`)
	if err := CheckModelBinaryVersion(t.Context(), binary, sonnet5ModelID); err != nil {
		t.Fatalf("version probe: %v", err)
	}
	// The wrapper exits before its child. A successful probe must still own
	// that child before the private probe directory is removed.
	time.Sleep(600 * time.Millisecond)
	marker := binary + ".fixture.child-survived"
	if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("wrapper descendant outlived version probe: marker error = %v", err)
	}
}

func TestCheckModelBinaryVersion_UnrelatedModelKeepsPriorBehavior(t *testing.T) {
	if err := CheckModelBinaryVersion(context.Background(), "relative-missing-binary", "claude-sonnet-4-5"); err != nil {
		t.Fatalf("unrelated model acquired a new version requirement: %v", err)
	}
}

// Hiding WriterTo reproduces the io.Copy ReaderFrom dispatch used for child pipes.
func TestModelVersionOutputCopyBound(t *testing.T) {
	var output boundedModelVersionOutput
	source := struct{ io.Reader }{strings.NewReader(strings.Repeat("x", modelVersionOutputMax+1))}
	_, err := io.Copy(&output, source)
	if !errors.Is(err, errModelVersionOutputTooLarge) || output.Len() > modelVersionOutputMax {
		t.Fatalf("copy error=%v retained=%d; want overflow refusal within %d bytes", err, output.Len(), modelVersionOutputMax)
	}
}
