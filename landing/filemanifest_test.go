package landing

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestBuildFileManifest(t *testing.T) {
	tests := []struct {
		name      string
		stdout    string
		err       error
		want      []string
		wantDiff  string // expected diff arg substring
		wantCalls int
	}{
		{
			name:      "splits non-empty lines",
			stdout:    "src/a.ts\nsrc/b.ts\nsrc/c.ts\n",
			want:      []string{"src/a.ts", "src/b.ts", "src/c.ts"},
			wantDiff:  "origin/main...feature/x",
			wantCalls: 1,
		},
		{
			name:      "trims surrounding whitespace and drops blank lines",
			stdout:    "\nsrc/a.ts\n\nsrc/b.ts\n",
			want:      []string{"src/a.ts", "src/b.ts"},
			wantDiff:  "origin/main...feature/x",
			wantCalls: 1,
		},
		{
			name:      "diff failure yields empty manifest (fail-safe)",
			err:       errors.New("fatal: bad revision"),
			want:      nil,
			wantCalls: 1,
		},
		{
			name:      "empty output yields nil",
			stdout:    "",
			want:      nil,
			wantCalls: 1,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fr := &fakeRunner{reply: func(string, []string) (string, error) {
				return tt.stdout, tt.err
			}}
			got, err := buildFileManifest(context.Background(), fr, "/repo", "feature/x", "main", "origin")
			if err != nil {
				t.Fatalf("buildFileManifest returned error: %v", err)
			}
			if !equalStrings(got, tt.want) {
				t.Errorf("files = %v, want %v", got, tt.want)
			}
			if len(fr.calls) != tt.wantCalls {
				t.Fatalf("call count = %d, want %d", len(fr.calls), tt.wantCalls)
			}
			if tt.wantDiff != "" {
				line := fr.calls[0].commandLine()
				if !strings.Contains(line, "git diff --name-only") || !strings.Contains(line, tt.wantDiff) {
					t.Errorf("diff command = %q, want it to contain %q and %q", line, "git diff --name-only", tt.wantDiff)
				}
			}
		})
	}
}

func TestBuildFileManifests_PreservesOrderAndStampsTime(t *testing.T) {
	// Each source branch returns a distinct file so we can assert ordering.
	fr := &fakeRunner{reply: func(name string, args []string) (string, error) {
		line := name + " " + strings.Join(args, " ")
		switch {
		case strings.Contains(line, "feat-a"):
			return "src/a.ts\n", nil
		case strings.Contains(line, "feat-b"):
			return "", errors.New("diff failed") // fail-safe → empty manifest
		case strings.Contains(line, "feat-c"):
			return "src/c.ts\nsrc/d.ts\n", nil
		default:
			return "", nil
		}
	}}

	entries := []ManifestEntry{
		{Proposal: 10, SourceBranch: "feat-a"},
		{Proposal: 20, SourceBranch: "feat-b"},
		{Proposal: 30, SourceBranch: "feat-c"},
	}
	fixed := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	got, err := buildFileManifests(context.Background(), fr, "/repo", entries, "main", "origin", func() time.Time { return fixed })
	if err != nil {
		t.Fatalf("buildFileManifests error: %v", err)
	}

	want := []FileManifest{
		{Proposal: 10, SourceBranch: "feat-a", Files: []string{"src/a.ts"}, ComputedAt: fixed},
		{Proposal: 20, SourceBranch: "feat-b", Files: nil, ComputedAt: fixed},
		{Proposal: 30, SourceBranch: "feat-c", Files: []string{"src/c.ts", "src/d.ts"}, ComputedAt: fixed},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("manifests = %+v, want %+v", got, want)
	}
}

// equalStrings treats nil and empty as equal so fail-safe (nil) results compare cleanly.
func equalStrings(a, b []string) bool {
	if len(a) == 0 && len(b) == 0 {
		return true
	}
	return reflect.DeepEqual(a, b)
}
