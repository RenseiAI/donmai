package kit

import (
	"errors"
	"reflect"
	"strings"
	"testing"
)

const (
	digestTS   = "1111111111111111111111111111111111111111111111111111111111111111"
	digestNext = "2222222222222222222222222222222222222222222222222222222222222222"
)

func commandView(id, digest, scope string) ManifestView {
	return ManifestView{
		ID: id, PackageDigest: digest, PathScope: scope,
		SupportedOS: []string{OSLinux, OSMacOS},
		Commands: map[string]string{
			"build": id + " build", "test": id + " test", "validate": id + " validate",
		},
	}
}

func identity(id, name, digest string) CommandIdentity {
	return CommandIdentity{KitID: id, Name: name, DigestKind: "package", Digest: digest}
}

func TestResolveCommandCompositionTypescriptNextCollisionAndLock(t *testing.T) {
	t.Parallel()
	views := []ManifestView{
		commandView("default/typescript", digestTS, "."),
		commandView("default/ts-nextjs", digestNext, "."),
	}
	target := CompositionTarget{OS: OSLinux, WorkType: "implementation", PathScope: "."}
	_, err := ResolveCommandComposition(views, target, nil, nil)
	if !errors.Is(err, ErrCommandCompositionConflict) {
		t.Fatalf("collision error = %v, want ErrCommandCompositionConflict", err)
	}
	for _, want := range []string{"build", "default/typescript", "default/ts-nextjs", "add an exact operator lock binding"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("collision error %q missing %q", err, want)
		}
	}

	bindings := make([]LockedCommandBinding, 0, 3)
	for _, alias := range []string{"build", "test", "validate"} {
		bindings = append(bindings, LockedCommandBinding{
			Alias: alias, PathScope: ".", Selected: identity("default/ts-nextjs", alias, digestNext),
		})
	}
	lock := &CompositionLock{Schema: CompositionLockSchema, Targets: []LockedCompositionTarget{{Target: target, Bindings: bindings}}}
	plan, err := ResolveCommandComposition(views, target, lock, nil)
	if err != nil {
		t.Fatalf("locked composition: %v", err)
	}
	if len(plan.Commands) != 6 || len(plan.Bindings) != 3 {
		t.Fatalf("commands/bindings = %d/%d, want 6/3", len(plan.Commands), len(plan.Bindings))
	}
	for _, binding := range plan.Bindings {
		if binding.Selected.KitID != "default/ts-nextjs" {
			t.Fatalf("binding %+v did not select nextjs", binding)
		}
	}
	if len(plan.Digest) != 64 {
		t.Fatalf("composition digest = %q", plan.Digest)
	}
}

func TestResolveCommandCompositionDisjointScopesDoNotConflict(t *testing.T) {
	t.Parallel()
	a := commandView("a", digestTS, "apps/api")
	b := commandView("b", digestNext, "apps/web")
	a.Commands = map[string]string{"build": "build-api"}
	b.Commands = map[string]string{"build": "build-web"}
	plan, err := ResolveCommandComposition([]ManifestView{a, b}, CompositionTarget{OS: OSLinux, PathScope: "."}, nil, nil)
	if err != nil {
		t.Fatalf("disjoint composition: %v", err)
	}
	if len(plan.Bindings) != 2 {
		t.Fatalf("bindings = %+v, want two disjoint build bindings", plan.Bindings)
	}
	if plan.Bindings[0].PathScope == plan.Bindings[1].PathScope {
		t.Fatalf("disjoint scopes collapsed: %+v", plan.Bindings)
	}
}

func TestResolveCommandCompositionDelegationCycle(t *testing.T) {
	t.Parallel()
	a := commandView("a", digestTS, ".")
	b := commandView("b", digestNext, ".")
	a.Commands, b.Commands = map[string]string{"build": "a"}, map[string]string{"build": "b"}
	ia, ib := identity("a", "build", digestTS), identity("b", "build", digestNext)
	delegations := []CommandDelegation{
		{Alias: "build", Target: CompositionTarget{OS: OSLinux, PathScope: "."}, From: ia, To: ib, AuthorizedBy: ia},
		{Alias: "build", Target: CompositionTarget{OS: OSLinux, PathScope: "."}, From: ib, To: ia, AuthorizedBy: ib},
	}
	_, err := ResolveCommandComposition([]ManifestView{a, b}, CompositionTarget{OS: OSLinux, PathScope: "."}, nil, delegations)
	if !errors.Is(err, ErrCommandCompositionConflict) || !strings.Contains(err.Error(), "cyclic") {
		t.Fatalf("cycle error = %v", err)
	}
}

func TestResolveCommandCompositionMissingDelegationTarget(t *testing.T) {
	t.Parallel()
	a := commandView("a", digestTS, ".")
	b := commandView("b", digestNext, ".")
	a.Commands, b.Commands = map[string]string{"build": "a"}, map[string]string{"build": "b"}
	ia := identity("a", "build", digestTS)
	missing := identity("missing", "build", strings.Repeat("3", 64))
	_, err := ResolveCommandComposition([]ManifestView{a, b}, CompositionTarget{OS: OSLinux, PathScope: "."}, nil, []CommandDelegation{{Alias: "build", Target: CompositionTarget{OS: OSLinux, PathScope: "."}, From: ia, To: missing, AuthorizedBy: ia}})
	if !errors.Is(err, ErrCommandCompositionConflict) || !strings.Contains(err.Error(), "missing target") {
		t.Fatalf("missing-target error = %v", err)
	}
}

func TestResolveCommandCompositionUnauthorizedSelfReplacement(t *testing.T) {
	t.Parallel()
	a := commandView("a", digestTS, ".")
	b := commandView("b", digestNext, ".")
	a.Commands, b.Commands = map[string]string{"build": "a"}, map[string]string{"build": "b"}
	ia, ib := identity("a", "build", digestTS), identity("b", "build", digestNext)
	_, err := ResolveCommandComposition([]ManifestView{a, b}, CompositionTarget{OS: OSLinux, PathScope: "."}, nil, []CommandDelegation{{Alias: "build", Target: CompositionTarget{OS: OSLinux, PathScope: "."}, From: ia, To: ib, AuthorizedBy: ib}})
	if !errors.Is(err, ErrCommandCompositionConflict) || !strings.Contains(err.Error(), "cannot authorize displacement") {
		t.Fatalf("unauthorized error = %v", err)
	}
}

func TestResolveCommandCompositionOwnerDelegation(t *testing.T) {
	t.Parallel()
	a := commandView("a", digestTS, ".")
	b := commandView("b", digestNext, ".")
	a.Commands, b.Commands = map[string]string{"build": "a"}, map[string]string{"build": "b"}
	ia, ib := identity("a", "build", digestTS), identity("b", "build", digestNext)
	plan, err := ResolveCommandComposition([]ManifestView{a, b}, CompositionTarget{OS: OSLinux, PathScope: "."}, nil, []CommandDelegation{{Alias: "build", Target: CompositionTarget{OS: OSLinux, PathScope: "."}, From: ia, To: ib, AuthorizedBy: ia}})
	if err != nil {
		t.Fatalf("delegated composition: %v", err)
	}
	if !sameIdentity(plan.Bindings[0].Selected, ib) {
		t.Fatalf("selected = %+v, want %+v", plan.Bindings[0].Selected, ib)
	}
}

func TestResolveCommandCompositionDelegationTargetDimensions(t *testing.T) {
	t.Parallel()
	a := commandView("a", digestTS, ".")
	b := commandView("b", digestNext, ".")
	a.Commands, b.Commands = map[string]string{"build": "a"}, map[string]string{"build": "b"}
	ia, ib := identity("a", "build", digestTS), identity("b", "build", digestNext)
	target := CompositionTarget{OS: OSLinux, WorkType: "implementation", PathScope: "."}
	for name, delegationTarget := range map[string]CompositionTarget{
		"wrong OS":        {OS: OSMacOS, WorkType: target.WorkType, PathScope: "."},
		"wrong work type": {OS: OSLinux, WorkType: "qa", PathScope: "."},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := ResolveCommandComposition([]ManifestView{a, b}, target, nil, []CommandDelegation{{Alias: "build", Target: delegationTarget, From: ia, To: ib, AuthorizedBy: ia}})
			if !errors.Is(err, ErrCommandCompositionConflict) {
				t.Fatalf("cross-target delegation error = %v", err)
			}
		})
	}
}

func TestResolveCommandCompositionDelegationScopeContainment(t *testing.T) {
	t.Parallel()
	ia, ib := identity("a", "build", digestTS), identity("b", "build", digestNext)
	delegation := func(scope string) []CommandDelegation {
		return []CommandDelegation{{Alias: "build", Target: CompositionTarget{OS: OSLinux, PathScope: scope}, From: ia, To: ib, AuthorizedBy: ia}}
	}

	a := commandView("a", digestTS, "apps/api")
	b := commandView("b", digestNext, "apps/api")
	a.Commands, b.Commands = map[string]string{"build": "a"}, map[string]string{"build": "b"}
	plan, err := ResolveCommandComposition([]ManifestView{a, b}, CompositionTarget{OS: OSLinux, PathScope: "."}, nil, delegation("."))
	if err != nil || len(plan.Bindings) != 1 || !sameIdentity(plan.Bindings[0].Selected, ib) {
		t.Fatalf("root delegation did not apply to subpath: plan=%+v err=%v", plan, err)
	}

	a.PathScope, b.PathScope = ".", "."
	_, err = ResolveCommandComposition([]ManifestView{a, b}, CompositionTarget{OS: OSLinux, PathScope: "."}, nil, delegation("apps/api"))
	if !errors.Is(err, ErrCommandCompositionConflict) {
		t.Fatalf("subpath delegation unexpectedly applied at root: %v", err)
	}
}

func TestResolveCommandCompositionRejectsOmittedOrNoncanonicalDelegationScope(t *testing.T) {
	t.Parallel()
	a := commandView("a", digestTS, ".")
	b := commandView("b", digestNext, ".")
	a.Commands, b.Commands = map[string]string{"build": "a"}, map[string]string{"build": "b"}
	ia, ib := identity("a", "build", digestTS), identity("b", "build", digestNext)
	for _, scope := range []string{"", "./", "apps//api", "apps/api/", "../apps"} {
		_, err := ResolveCommandComposition([]ManifestView{a, b}, CompositionTarget{OS: OSLinux, PathScope: "."}, nil, []CommandDelegation{{
			Alias: "build", Target: CompositionTarget{OS: OSLinux, PathScope: scope}, From: ia, To: ib, AuthorizedBy: ia,
		}})
		if !errors.Is(err, ErrCommandCompositionConflict) || !strings.Contains(err.Error(), "delegation path scope") {
			t.Fatalf("scope %q error = %v", scope, err)
		}
	}
}

func TestResolveCommandCompositionOSOverrideAndWorkType(t *testing.T) {
	t.Parallel()
	v := commandView("a", digestTS, ".")
	v.Commands = map[string]string{"build": "generic"}
	v.CommandsOverride = map[string]map[string]string{OSLinux: {"build": "linux"}}
	v.WorkTypes = []string{"implementation"}
	plan, err := ResolveCommandComposition([]ManifestView{v}, CompositionTarget{OS: OSLinux, WorkType: "implementation", PathScope: "."}, nil, nil)
	if err != nil || len(plan.Commands) != 1 || plan.Commands[0].Shell != "linux" {
		t.Fatalf("OS override plan = %+v, err=%v", plan, err)
	}
	empty, err := ResolveCommandComposition([]ManifestView{v}, CompositionTarget{OS: OSLinux, WorkType: "qa", PathScope: "."}, nil, nil)
	if err != nil || len(empty.Commands) != 0 {
		t.Fatalf("work-type filtered plan = %+v, err=%v", empty, err)
	}
	unfiltered, err := ResolveCommandComposition([]ManifestView{v}, CompositionTarget{OS: OSLinux, PathScope: "."}, nil, nil)
	if err != nil || len(unfiltered.Commands) != 1 {
		t.Fatalf("empty work type should be unfiltered: plan=%+v err=%v", unfiltered, err)
	}
}

func TestCanonicalCompositionLockRoundTripAndDigestDeterminism(t *testing.T) {
	t.Parallel()
	target := CompositionTarget{OS: OSLinux, WorkType: "implementation", PathScope: "."}
	lock := CompositionLock{Targets: []LockedCompositionTarget{{
		Target: target,
		Bindings: []LockedCommandBinding{
			{Alias: "test", PathScope: ".", Selected: identity("a", "test", digestTS)},
			{Alias: "build", PathScope: ".", Selected: identity("a", "build", digestTS)},
		},
	}}}
	raw, err := CanonicalCompositionLock(lock)
	if err != nil {
		t.Fatalf("canonical lock: %v", err)
	}
	parsed, err := ParseCompositionLock(raw)
	if err != nil {
		t.Fatalf("parse canonical lock: %v", err)
	}
	if parsed.Targets[0].Bindings[0].Alias != "build" {
		t.Fatalf("bindings not canonical: %+v", parsed.Targets[0].Bindings)
	}
	if _, err := ParseCompositionLock(append([]byte(" "), raw...)); !errors.Is(err, ErrCompositionLockInvalid) {
		t.Fatalf("noncanonical lock error = %v", err)
	}

	views := []ManifestView{commandView("a", digestTS, ".")}
	p1, _ := ResolveCommandComposition(views, target, parsed, nil)
	p2, _ := ResolveCommandComposition(views, target, parsed, nil)
	if p1.Digest != p2.Digest || !reflect.DeepEqual(p1, p2) {
		t.Fatalf("composition is not deterministic: %#v != %#v", p1, p2)
	}
}

func TestResolveCommandCompositionRejectsUnsafeScopeAndNewOverride(t *testing.T) {
	t.Parallel()
	v := commandView("a", digestTS, "../escape")
	if _, err := ResolveCommandComposition([]ManifestView{v}, CompositionTarget{OS: OSLinux, PathScope: "."}, nil, nil); err == nil || !strings.Contains(err.Error(), "unsafe path scope") {
		t.Fatalf("unsafe view scope error = %v", err)
	}
	v.PathScope = "."
	v.Commands = map[string]string{"build": "build"}
	v.CommandsOverride = map[string]map[string]string{OSLinux: {"deploy": "deploy"}}
	if _, err := ResolveCommandComposition([]ManifestView{v}, CompositionTarget{OS: OSLinux, PathScope: "."}, nil, nil); err == nil || !strings.Contains(err.Error(), "no same-owner base command") {
		t.Fatalf("new override error = %v", err)
	}
	if _, err := ResolveCommandComposition([]ManifestView{v}, CompositionTarget{OS: OSLinux, PathScope: "/absolute"}, nil, nil); err == nil || !strings.Contains(err.Error(), "unsafe") {
		t.Fatalf("unsafe target error = %v", err)
	}
}

func TestResolveCommandCompositionRejectsStaleLockBinding(t *testing.T) {
	t.Parallel()
	target := CompositionTarget{OS: OSLinux, PathScope: "."}
	v := commandView("a", digestTS, ".")
	v.Commands = map[string]string{"build": "build"}
	lock := &CompositionLock{Schema: CompositionLockSchema, Targets: []LockedCompositionTarget{{
		Target: target,
		Bindings: []LockedCommandBinding{{
			Alias: "test", PathScope: ".", Selected: identity("a", "test", digestTS),
		}},
	}}}
	_, err := ResolveCommandComposition([]ManifestView{v}, target, lock, nil)
	if !errors.Is(err, ErrCompositionLockInvalid) || !strings.Contains(err.Error(), "stale or unused") {
		t.Fatalf("stale lock error = %v", err)
	}
}
