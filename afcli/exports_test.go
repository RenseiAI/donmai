package afcli

import (
	"testing"

	"github.com/RenseiAI/agentfactory-tui/afclient"
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
