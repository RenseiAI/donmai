package afcli

import (
	"testing"

	"github.com/RenseiAI/donmai/afclient"
)

// TestPublicFactoriesReturnFreshTrees pins the contract that each
// public daemon-targeted factory returns a non-nil *cobra.Command
// with the expected Use name and at least one subcommand. Downstream
// binaries (e.g. rensei-tui) graft these trees under their own
// parents (rensei host provider / kit / workarea) — if a factory
// returned nil or an empty tree the graft would silently break.
func TestPublicFactoriesReturnFreshTrees(t *testing.T) {
	t.Parallel()

	ds := func() afclient.DataSource { return afclient.NewMockClient() }

	cases := []struct {
		name        string
		build       func() (string, int)
		wantUse     string
		minChildren int
	}{
		{
			name: "NewProviderCmd",
			build: func() (string, int) {
				cmd := NewProviderCmd(ds)
				return cmd.Use, len(cmd.Commands())
			},
			wantUse:     "provider",
			minChildren: 2, // list, show
		},
		{
			name: "NewKitCmd",
			build: func() (string, int) {
				cmd := NewKitCmd(ds)
				return cmd.Use, len(cmd.Commands())
			},
			wantUse:     "kit",
			minChildren: 5, // list, show, install, enable, disable, verify, sources
		},
		{
			name: "NewWorkareaCmd",
			build: func() (string, int) {
				cmd := NewWorkareaCmd(ds)
				return cmd.Use, len(cmd.Commands())
			},
			wantUse:     "workarea",
			minChildren: 4, // list, show, restore, diff
		},
		{
			name: "NewRoutingCmd",
			build: func() (string, int) {
				cmd := NewRoutingCmd(ds)
				return cmd.Use, len(cmd.Commands())
			},
			wantUse:     "routing",
			minChildren: 2, // show, explain
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			use, n := tc.build()
			if use != tc.wantUse {
				t.Errorf("Use = %q, want %q", use, tc.wantUse)
			}
			if n < tc.minChildren {
				t.Errorf("subcommand count = %d, want >= %d", n, tc.minChildren)
			}
		})
	}
}

// TestPublicFactoriesReturnIndependentTrees pins that each invocation
// returns a fresh tree — calling NewProviderCmd twice must not share
// children or state, since downstream binaries register the same
// surface twice (top-level + nested under `host`).
func TestPublicFactoriesReturnIndependentTrees(t *testing.T) {
	t.Parallel()

	ds := func() afclient.DataSource { return afclient.NewMockClient() }

	a := NewProviderCmd(ds)
	b := NewProviderCmd(ds)
	if a == b {
		t.Fatal("NewProviderCmd returned the same *cobra.Command pointer twice — calls must be independent")
	}
	if len(a.Commands()) == 0 || len(b.Commands()) == 0 {
		t.Fatalf("expected non-empty subcommand trees; a=%d b=%d", len(a.Commands()), len(b.Commands()))
	}
	// Subcommand pointers must also be distinct so a Hidden/Deprecated
	// flag set on one parent's tree doesn't leak into the other.
	if a.Commands()[0] == b.Commands()[0] {
		t.Fatal("subcommand pointers shared between two NewProviderCmd calls — graft would leak state")
	}
}

// TestNewHostWatchCmd_NotHiddenNotDeprecated is the export-gap regression: a
// downstream embedder that previously had no clean constructor for the
// host-watch dashboard had to relocate the alias-registered `fleet-watch`
// command off root instead, which silently dragged along that command's
// Hidden and Deprecated fields — so the embedder's own first-class command
// inherited a deprecation notice pointing at itself. NewHostWatchCmd must
// return a plain, non-deprecated, visible tree so constructing it can never
// reproduce that defect.
func TestNewHostWatchCmd_NotHiddenNotDeprecated(t *testing.T) {
	t.Parallel()

	cmd := NewHostWatchCmd()

	if cmd.Use != "watch" {
		t.Errorf("Use = %q, want %q", cmd.Use, "watch")
	}
	if cmd.Hidden {
		t.Error("NewHostWatchCmd must not be Hidden — it is the first-class dashboard command")
	}
	if cmd.Deprecated != "" {
		t.Errorf("NewHostWatchCmd must carry no Deprecated notice, got %q", cmd.Deprecated)
	}
}

// TestNewHostWatchCmd_ReturnsFreshTrees mirrors the other exported
// factories' independence guarantee: a caller may graft the same logical
// surface under more than one parent.
func TestNewHostWatchCmd_ReturnsFreshTrees(t *testing.T) {
	t.Parallel()

	a := NewHostWatchCmd()
	b := NewHostWatchCmd()
	if a == b {
		t.Fatal("NewHostWatchCmd returned the same *cobra.Command pointer twice")
	}
}
