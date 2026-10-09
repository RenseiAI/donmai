package runner

import (
	"errors"
	"log/slog"
	"strings"
	"testing"

	"github.com/RenseiAI/donmai/internal/kit"
	"github.com/RenseiAI/donmai/prompt"
)

func TestResolveKitDemandTargetAwareComposer(t *testing.T) {
	t.Parallel()
	const digest = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	r := &Runner{
		logger:      slog.Default(),
		kitTargetOS: kit.OSLinux,
		kitComposer: func(repoRoot string, target kit.CompositionTarget, selected []kit.Selection) (*kit.ToolchainDemand, error) {
			if repoRoot != "/work/repo" {
				t.Fatalf("repoRoot = %q", repoRoot)
			}
			if target.OS != kit.OSLinux || target.WorkType != "implementation" || target.PathScope != "." {
				t.Fatalf("target = %+v", target)
			}
			if selected != nil {
				t.Fatalf("selected = %+v, want nil detection fallback", selected)
			}
			return &kit.ToolchainDemand{
				OS: target.OS,
				Commands: []kit.QualifiedCommand{{
					Identity: kit.CommandIdentity{KitID: "go", Name: "test", DigestKind: "package", Digest: digest},
					Shell:    "go test ./...", PathScope: ".",
				}},
				CommandBindings: []kit.GenericCommandBinding{{
					Alias: "test", PathScope: ".",
					Selected: kit.CommandIdentity{KitID: "go", Name: "test", DigestKind: "package", Digest: digest},
				}},
				CompositionDigest: digest,
			}, nil
		},
	}
	res := &Result{}
	demand := r.resolveKitDemand(QueuedWork{QueuedWork: prompt.QueuedWork{SessionID: "session-1", WorkType: "implementation"}}, "/work/repo", res)
	if demand == nil || demand.CompositionDigest != digest || len(demand.CommandBindings) != 1 {
		t.Fatalf("demand = %+v", demand)
	}
	if res.Status == "failed" {
		t.Fatalf("unexpected failure: %+v", res)
	}
}

func TestResolveKitDemandComposerConflictFailsBeforeProvision(t *testing.T) {
	t.Parallel()
	r := &Runner{
		logger:      slog.Default(),
		kitTargetOS: kit.OSLinux,
		kitComposer: func(string, kit.CompositionTarget, []kit.Selection) (*kit.ToolchainDemand, error) {
			return nil, errors.Join(kit.ErrCommandCompositionConflict, errors.New("build claimed by a and b"))
		},
	}
	res := &Result{}
	demand := r.resolveKitDemand(QueuedWork{QueuedWork: prompt.QueuedWork{SessionID: "session-2"}}, "/work/repo", res)
	if demand == nil || res.Status != "failed" || res.FailureMode != FailureKitProvision {
		t.Fatalf("demand/result = %+v / %+v", demand, res)
	}
	if res.Error == "" {
		t.Fatal("missing actionable composition error")
	}
}

func TestResolveKitDemandPlatformLifecycleRequiresExactComposition(t *testing.T) {
	t.Parallel()
	const digest = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	platform := &kit.ToolchainDemand{
		Kits: []string{"default/go@1.0.0"}, OS: kit.OSLinux,
		ToolchainInstall: []string{"install-go"}, PostAcquire: []string{"prepare"},
		PreRelease: []string{"clean"}, Env: map[string]string{"GOFLAGS": "-mod=readonly"},
	}
	r := &Runner{
		logger: slog.Default(),
		kitComposer: func(repoRoot string, target kit.CompositionTarget, selected []kit.Selection) (*kit.ToolchainDemand, error) {
			if repoRoot != "/work/repo" || target.OS != kit.OSLinux || target.WorkType != "implementation" || target.PathScope != "." {
				t.Fatalf("repo/target = %q / %+v", repoRoot, target)
			}
			if len(selected) != 1 || selected[0] != (kit.Selection{ID: "default/go", Version: "1.0.0"}) {
				t.Fatalf("selected = %+v", selected)
			}
			return &kit.ToolchainDemand{
				Commands:          []kit.QualifiedCommand{{Identity: kit.CommandIdentity{KitID: "default/go", Name: "test", DigestKind: "package", Digest: digest}, Shell: "go test ./...", PathScope: "."}},
				CommandBindings:   []kit.GenericCommandBinding{{Alias: "test", PathScope: ".", Selected: kit.CommandIdentity{KitID: "default/go", Name: "test", DigestKind: "package", Digest: digest}}},
				CompositionDigest: digest,
			}, nil
		},
	}
	res := &Result{}
	demand := r.resolveKitDemand(QueuedWork{QueuedWork: prompt.QueuedWork{SessionID: "session-platform", WorkType: "implementation", Kits: platform}}, "/work/repo", res)
	if res.Status == "failed" || demand == nil {
		t.Fatalf("demand/result = %+v / %+v", demand, res)
	}
	if len(demand.ToolchainInstall) != 1 || demand.ToolchainInstall[0] != "install-go" || len(demand.PostAcquire) != 1 || len(demand.PreRelease) != 1 || demand.Env["GOFLAGS"] != "-mod=readonly" {
		t.Fatalf("platform lifecycle not preserved: %+v", demand)
	}
	if len(demand.Commands) != 1 || len(demand.CommandBindings) != 1 || demand.CompositionDigest != digest {
		t.Fatalf("composition not merged: %+v", demand)
	}
	if platform.CompositionDigest != "" || len(platform.Commands) != 0 {
		t.Fatalf("platform input mutated: %+v", platform)
	}
}

// TestResolveKitDemandPlatformStorePlanIsLocal pins that a payload's own
// dependency_stores plan never survives the local composition preflight:
// the locally composed plan and digest replace it, as commands do.
func TestResolveKitDemandPlatformStorePlanIsLocal(t *testing.T) {
	t.Parallel()
	platform := &kit.ToolchainDemand{
		Kits: []string{"node/pnpm@1.0.0"}, OS: kit.OSLinux,
		DependencyStores: []kit.ComposedDependencyStore{{
			Manager: "pnpm", KitID: "payload/kit",
			Commands: map[string]string{"fetch": "payload fetch", "install": "payload install"},
		}},
		DependencyStoresDigest: "payload-digest",
	}
	local := kit.ComposedDependencyStore{
		Manager: "pnpm", KitID: "node/pnpm",
		Commands: map[string]string{"fetch": "pnpm fetch", "install": "pnpm install --offline --frozen-lockfile"},
	}
	r := &Runner{
		logger: slog.Default(),
		kitComposer: func(string, kit.CompositionTarget, []kit.Selection) (*kit.ToolchainDemand, error) {
			return &kit.ToolchainDemand{
				OS:                     kit.OSLinux,
				DependencyStores:       []kit.ComposedDependencyStore{local},
				DependencyStoresDigest: "local-digest",
			}, nil
		},
	}
	res := &Result{}
	demand := r.resolveKitDemand(QueuedWork{QueuedWork: prompt.QueuedWork{SessionID: "session-stores", Kits: platform}}, "/work/repo", res)
	if res.Status == "failed" || demand == nil {
		t.Fatalf("demand/result = %+v / %+v", demand, res)
	}
	if demand.DependencyStoresDigest != "local-digest" || len(demand.DependencyStores) != 1 ||
		demand.DependencyStores[0].KitID != "node/pnpm" || demand.DependencyStores[0].Commands["fetch"] != "pnpm fetch" {
		t.Fatalf("store plan = %+v (digest %q), want the local composition", demand.DependencyStores, demand.DependencyStoresDigest)
	}
	if platform.DependencyStoresDigest != "payload-digest" || platform.DependencyStores[0].KitID != "payload/kit" {
		t.Fatalf("platform input mutated: %+v", platform)
	}
}

func TestResolveKitDemandPlatformConflictFailsBeforeProvision(t *testing.T) {
	t.Parallel()
	r := &Runner{
		logger: slog.Default(),
		kitComposer: func(string, kit.CompositionTarget, []kit.Selection) (*kit.ToolchainDemand, error) {
			return nil, errors.Join(kit.ErrCommandCompositionConflict, errors.New("build claimed by a and b"))
		},
	}
	res := &Result{}
	demand := r.resolveKitDemand(QueuedWork{QueuedWork: prompt.QueuedWork{SessionID: "session-platform-conflict", Kits: &kit.ToolchainDemand{Kits: []string{"go@1.0.0"}, ToolchainInstall: []string{"must-not-run"}}}}, "/work/repo", res)
	if demand == nil || res.Status != "failed" || res.FailureMode != FailureKitProvision || !strings.Contains(res.Error, "build claimed") {
		t.Fatalf("demand/result = %+v / %+v", demand, res)
	}
}

func TestResolveKitDemandPlatformDemandCannotBypassMissingComposer(t *testing.T) {
	t.Parallel()
	r := &Runner{logger: slog.Default(), kitTargetOS: kit.OSLinux}
	res := &Result{}
	demand := r.resolveKitDemand(QueuedWork{QueuedWork: prompt.QueuedWork{SessionID: "session-no-composer", Kits: &kit.ToolchainDemand{Kits: []string{"go@1.0.0"}, ToolchainInstall: []string{"must-not-run"}}}}, "/work/repo", res)
	if demand == nil || res.Status != "failed" || res.FailureMode != FailureKitProvision || !strings.Contains(res.Error, "requires local command composition") {
		t.Fatalf("demand/result = %+v / %+v", demand, res)
	}
}

func TestParseExactKitSelections(t *testing.T) {
	t.Parallel()
	got, err := parseExactKitSelections([]string{"owner/go@1.2.3"})
	if err != nil || len(got) != 1 || got[0] != (kit.Selection{ID: "owner/go", Version: "1.2.3"}) {
		t.Fatalf("selection = %+v, err=%v", got, err)
	}
	for _, refs := range [][]string{nil, {"go"}, {"@1.0.0"}, {"go@"}, {"go@1.0.0", "go@1.0.0"}} {
		if _, err := parseExactKitSelections(refs); err == nil {
			t.Fatalf("refs %+v unexpectedly accepted", refs)
		}
	}
}
