package kit

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path"
	"sort"
	"strings"

	jsoncanonicalizer "github.com/cyberphone/json-canonicalization/go/src/webpki.org/jsoncanonicalizer"
)

const (
	// CompositionLockSchema is the canonical operator command-binding lock.
	CompositionLockSchema = "donmai.dev/kit-composition-lock/v1"
	// CompositionPlanSchema is the canonical resolved command plan.
	CompositionPlanSchema = "donmai.dev/kit-command-composition/v1"
)

var (
	// ErrCommandCompositionConflict marks unresolved or unauthorized ownership.
	ErrCommandCompositionConflict = errors.New("kit command composition conflict")
	// ErrCompositionLockInvalid marks malformed or noncanonical operator locks.
	ErrCompositionLockInvalid = errors.New("kit composition lock invalid")
)

// CompositionTarget is the complete dimension set used to resolve command
// aliases. PathScope is a slash-separated repository-relative scope; "." is
// the repository root. Empty WorkType means the command plan is not filtered
// by workflow type.
type CompositionTarget struct {
	OS        string `json:"os"`
	WorkType  string `json:"workType,omitempty"`
	PathScope string `json:"pathScope"`
}

// Selection identifies one exact kit version selected by an authoritative
// upstream lifecycle demand.
type Selection struct {
	ID      string
	Version string
}

// CommandIdentity is the canonical, delimiter-free identity retained in plans
// and audit data. Digest is the verified package digest, or the exact legacy
// manifest content digest while legacy compatibility remains enabled.
type CommandIdentity struct {
	KitID      string `json:"kitId"`
	Name       string `json:"name"`
	DigestKind string `json:"digestKind"`
	Digest     string `json:"contentDigest"`
}

// QualifiedCommand retains executable text under its exact owner and scope.
type QualifiedCommand struct {
	Identity  CommandIdentity `json:"identity"`
	Shell     string          `json:"shell"`
	PathScope string          `json:"pathScope"`
}

// GenericCommandBinding maps one generic alias/scope to one exact owner.
type GenericCommandBinding struct {
	Alias     string          `json:"alias"`
	PathScope string          `json:"pathScope"`
	Selected  CommandIdentity `json:"selected"`
}

// CommandPackageRef binds a contributing kit to its exact content digest.
type CommandPackageRef struct {
	KitID  string `json:"kitId"`
	Digest string `json:"packageDigest"`
}

// CommandDelegation represents already-authenticated owner/catalog policy for
// one exact OS/work type and a repository scope subtree. AuthorizedBy must
// equal From: a replacement cannot authorize displacing a command owned by
// another package.
type CommandDelegation struct {
	Alias        string            `json:"alias"`
	Target       CompositionTarget `json:"target"`
	From         CommandIdentity   `json:"from"`
	To           CommandIdentity   `json:"to"`
	AuthorizedBy CommandIdentity   `json:"authorizedBy"`
}

// LockedCommandBinding is one operator-approved exact alias selection.
type LockedCommandBinding struct {
	Alias     string          `json:"alias"`
	PathScope string          `json:"pathScope"`
	Selected  CommandIdentity `json:"selected"`
}

// LockedCompositionTarget groups bindings for one exact dimension set.
type LockedCompositionTarget struct {
	Target   CompositionTarget      `json:"target"`
	Bindings []LockedCommandBinding `json:"bindings"`
}

// CompositionLock is the operator-owned canonical command authority record.
type CompositionLock struct {
	Schema  string                    `json:"schema"`
	Targets []LockedCompositionTarget `json:"targets"`
}

// CommandComposition is the canonical resolved owner-qualified command plan.
type CommandComposition struct {
	Schema   string                  `json:"schema"`
	Target   CompositionTarget       `json:"target"`
	Packages []CommandPackageRef     `json:"packages"`
	Commands []QualifiedCommand      `json:"commands"`
	Bindings []GenericCommandBinding `json:"bindings"`
	Digest   string                  `json:"digest"`
}

// ParseCompositionLock accepts only exact RFC 8785 bytes. Lock files are
// durable authority records, not permissive operator config; canonical bytes
// prevent duplicate-key and representation ambiguity.
func ParseCompositionLock(raw []byte) (*CompositionLock, error) {
	canonical, err := jsoncanonicalizer.Transform(raw)
	if err != nil || !bytes.Equal(raw, canonical) {
		return nil, fmt.Errorf("%w: lock is not exact RFC 8785 JSON", ErrCompositionLockInvalid)
	}
	var lock CompositionLock
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&lock); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrCompositionLockInvalid, err)
	}
	var trailing any
	if err := dec.Decode(&trailing); !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("%w: trailing data", ErrCompositionLockInvalid)
	}
	if err := validateCompositionLock(&lock); err != nil {
		return nil, err
	}
	return &lock, nil
}

// CanonicalCompositionLock validates, sorts, and RFC 8785-encodes a lock.
func CanonicalCompositionLock(lock CompositionLock) ([]byte, error) {
	lock.Schema = CompositionLockSchema
	for i := range lock.Targets {
		if !safeScopeInput(lock.Targets[i].Target.PathScope) {
			return nil, fmt.Errorf("%w: unsafe target pathScope", ErrCompositionLockInvalid)
		}
		lock.Targets[i].Target.PathScope = normalizeScope(lock.Targets[i].Target.PathScope)
		for j := range lock.Targets[i].Bindings {
			if !safeScopeInput(lock.Targets[i].Bindings[j].PathScope) {
				return nil, fmt.Errorf("%w: unsafe binding pathScope", ErrCompositionLockInvalid)
			}
			lock.Targets[i].Bindings[j].PathScope = normalizeScope(lock.Targets[i].Bindings[j].PathScope)
		}
		sort.Slice(lock.Targets[i].Bindings, func(a, b int) bool {
			x, y := lock.Targets[i].Bindings[a], lock.Targets[i].Bindings[b]
			if x.Alias != y.Alias {
				return x.Alias < y.Alias
			}
			if x.PathScope != y.PathScope {
				return x.PathScope < y.PathScope
			}
			return identityKey(x.Selected) < identityKey(y.Selected)
		})
	}
	sort.Slice(lock.Targets, func(i, j int) bool {
		return targetKey(lock.Targets[i].Target) < targetKey(lock.Targets[j].Target)
	})
	if err := validateCompositionLock(&lock); err != nil {
		return nil, err
	}
	raw, err := json.Marshal(lock)
	if err != nil {
		return nil, err
	}
	return jsoncanonicalizer.Transform(raw)
}

func validateCompositionLock(lock *CompositionLock) error {
	if lock == nil || lock.Schema != CompositionLockSchema {
		return fmt.Errorf("%w: unsupported schema", ErrCompositionLockInvalid)
	}
	seenTargets := map[string]struct{}{}
	for _, entry := range lock.Targets {
		if entry.Target.OS == "" || !validScope(entry.Target.PathScope) || entry.Target.PathScope != normalizeScope(entry.Target.PathScope) {
			return fmt.Errorf("%w: target OS and canonical pathScope are required", ErrCompositionLockInvalid)
		}
		key := targetKey(entry.Target)
		if _, ok := seenTargets[key]; ok {
			return fmt.Errorf("%w: duplicate target %s", ErrCompositionLockInvalid, key)
		}
		seenTargets[key] = struct{}{}
		seenBindings := map[string]struct{}{}
		for _, binding := range entry.Bindings {
			if binding.Alias == "" || !validScope(binding.PathScope) || binding.PathScope != normalizeScope(binding.PathScope) || !validIdentity(binding.Selected) {
				return fmt.Errorf("%w: malformed binding", ErrCompositionLockInvalid)
			}
			bindingKey := binding.Alias + "\x00" + binding.PathScope
			if _, ok := seenBindings[bindingKey]; ok {
				return fmt.Errorf("%w: duplicate binding for %s", ErrCompositionLockInvalid, binding.Alias)
			}
			seenBindings[bindingKey] = struct{}{}
		}
	}
	return nil
}

// ResolveCommandComposition builds the exact command plan or fails closed when
// an overlapping generic alias lacks one authorized terminal owner.
func ResolveCommandComposition(views []ManifestView, target CompositionTarget, lock *CompositionLock, delegations []CommandDelegation) (*CommandComposition, error) {
	if target.OS == "" {
		return nil, errors.New("kit command compose: target OS is required")
	}
	if !safeScopeInput(target.PathScope) {
		return nil, errors.New("kit command compose: target path scope is unsafe")
	}
	target.PathScope = normalizeScope(target.PathScope)
	if !validScope(target.PathScope) {
		return nil, errors.New("kit command compose: target path scope is invalid")
	}
	if lock != nil {
		if err := validateCompositionLock(lock); err != nil {
			return nil, err
		}
	}
	plan := &CommandComposition{Schema: CompositionPlanSchema, Target: target}
	seenCommands := map[string]struct{}{}

	for _, view := range views {
		if !supportsOS(view.SupportedOS, target.OS) || !supportsWorkType(view.WorkTypes, target.WorkType) {
			continue
		}
		if !safeScopeInput(view.PathScope) {
			return nil, fmt.Errorf("kit command compose: %s has unsafe path scope %q", view.ID, view.PathScope)
		}
		scope := normalizeScope(view.PathScope)
		if !validScope(scope) {
			return nil, fmt.Errorf("kit command compose: %s has invalid path scope %q", view.ID, view.PathScope)
		}
		if target.PathScope != "." && !scopeContains(scope, target.PathScope) {
			continue
		}
		if view.PackageDigest != "" {
			plan.Packages = append(plan.Packages, CommandPackageRef{KitID: view.ID, Digest: view.PackageDigest})
		}
		commands := copyCommands(view.Commands)
		for name, shell := range view.CommandsOverride[target.OS] {
			if _, exists := commands[name]; !exists {
				return nil, fmt.Errorf("kit command compose: %s OS override %q has no same-owner base command", view.ID, name)
			}
			commands[name] = shell
		}
		for name, shell := range commands {
			commandIdentity := commandIdentityForView(view, name)
			if shell == "" || !validIdentity(commandIdentity) {
				return nil, fmt.Errorf("kit command compose: %s has incomplete command identity", view.ID)
			}
			commandKey := identityKey(commandIdentity) + "\x00" + scope
			if _, exists := seenCommands[commandKey]; exists {
				return nil, fmt.Errorf("kit command compose: duplicate command identity %s in scope %q", renderIdentity(commandIdentity), scope)
			}
			seenCommands[commandKey] = struct{}{}
			plan.Commands = append(plan.Commands, QualifiedCommand{
				Identity: commandIdentity,
				Shell:    shell, PathScope: scope,
			})
		}
	}
	sort.Slice(plan.Commands, func(i, j int) bool {
		x, y := plan.Commands[i], plan.Commands[j]
		if x.Identity.KitID != y.Identity.KitID {
			return x.Identity.KitID < y.Identity.KitID
		}
		if x.Identity.Name != y.Identity.Name {
			return x.Identity.Name < y.Identity.Name
		}
		if x.Identity.Digest != y.Identity.Digest {
			return x.Identity.Digest < y.Identity.Digest
		}
		return x.PathScope < y.PathScope
	})

	byAlias := map[string][]QualifiedCommand{}
	for _, command := range plan.Commands {
		byAlias[command.Identity.Name] = append(byAlias[command.Identity.Name], command)
	}
	aliases := make([]string, 0, len(byAlias))
	for alias := range byAlias {
		aliases = append(aliases, alias)
	}
	sort.Strings(aliases)
	for _, alias := range aliases {
		for _, scoped := range claimSetsByScope(byAlias[alias], target.PathScope) {
			binding, err := resolveAliasComponent(alias, scoped.scope, scoped.claims, target, lock, delegations)
			if err != nil {
				return nil, err
			}
			plan.Bindings = append(plan.Bindings, binding)
		}
	}
	sort.Slice(plan.Bindings, func(i, j int) bool {
		x, y := plan.Bindings[i], plan.Bindings[j]
		if x.Alias != y.Alias {
			return x.Alias < y.Alias
		}
		if x.PathScope != y.PathScope {
			return x.PathScope < y.PathScope
		}
		return identityKey(x.Selected) < identityKey(y.Selected)
	})
	if err := validateAppliedLock(plan.Bindings, target, lock); err != nil {
		return nil, err
	}

	digest, err := compositionDigest(*plan)
	if err != nil {
		return nil, err
	}
	plan.Digest = digest
	return plan, nil
}

func resolveAliasComponent(alias, scope string, claims []QualifiedCommand, target CompositionTarget, lock *CompositionLock, delegations []CommandDelegation) (GenericCommandBinding, error) {
	if len(claims) == 1 {
		return GenericCommandBinding{Alias: alias, PathScope: scope, Selected: claims[0].Identity}, nil
	}

	claimByID := map[string]CommandIdentity{}
	for _, claim := range claims {
		claimByID[identityKey(claim.Identity)] = claim.Identity
	}
	if selected, ok := lockedSelection(lock, target, alias, scope); ok {
		if _, exists := claimByID[identityKey(selected)]; !exists {
			return GenericCommandBinding{}, fmt.Errorf("%w: binding for alias %q selects missing target %s", ErrCompositionLockInvalid, alias, renderIdentity(selected))
		}
		return GenericCommandBinding{Alias: alias, PathScope: scope, Selected: selected}, nil
	}

	edges := map[string]string{}
	for _, delegation := range delegations {
		if delegation.Alias != alias {
			continue
		}
		if !canonicalDelegationScope(delegation.Target.PathScope) {
			return GenericCommandBinding{}, fmt.Errorf("%w: alias %q has invalid or noncanonical delegation path scope %q", ErrCommandCompositionConflict, alias, delegation.Target.PathScope)
		}
		if delegation.Target.OS != target.OS || delegation.Target.WorkType != target.WorkType {
			continue
		}
		delegationScope := normalizeScope(delegation.Target.PathScope)
		if !scopeContains(delegationScope, scope) {
			continue
		}
		if !validIdentity(delegation.From) || !validIdentity(delegation.To) || !validIdentity(delegation.AuthorizedBy) {
			return GenericCommandBinding{}, fmt.Errorf("%w: alias %q has malformed delegation identity", ErrCommandCompositionConflict, alias)
		}
		fromKey, toKey := identityKey(delegation.From), identityKey(delegation.To)
		if _, ok := claimByID[fromKey]; !ok {
			continue
		}
		if _, ok := claimByID[toKey]; !ok {
			return GenericCommandBinding{}, fmt.Errorf("%w: delegation for alias %q names missing target %s", ErrCommandCompositionConflict, alias, renderIdentity(delegation.To))
		}
		if !sameIdentity(delegation.AuthorizedBy, delegation.From) {
			return GenericCommandBinding{}, fmt.Errorf("%w: %s cannot authorize displacement of %s", ErrCommandCompositionConflict, renderIdentity(delegation.AuthorizedBy), renderIdentity(delegation.From))
		}
		if existing, ok := edges[fromKey]; ok && existing != toKey {
			return GenericCommandBinding{}, fmt.Errorf("%w: alias %q has ambiguous delegations from %s", ErrCommandCompositionConflict, alias, renderIdentity(delegation.From))
		}
		edges[fromKey] = toKey
	}
	if hasDelegationCycle(edges) {
		return GenericCommandBinding{}, fmt.Errorf("%w: alias %q replacement chain is cyclic", ErrCommandCompositionConflict, alias)
	}
	terminals := map[string]CommandIdentity{}
	for key := range claimByID {
		cursor := key
		for edges[cursor] != "" {
			cursor = edges[cursor]
		}
		terminals[cursor] = claimByID[cursor]
	}
	if len(terminals) == 1 {
		for _, selected := range terminals {
			return GenericCommandBinding{Alias: alias, PathScope: scope, Selected: selected}, nil
		}
	}

	claimants := make([]string, 0, len(claims))
	for _, claim := range claims {
		claimants = append(claimants, renderIdentity(claim.Identity))
	}
	sort.Strings(claimants)
	return GenericCommandBinding{}, fmt.Errorf("%w: alias %q scope %q claimed by [%s]; add an exact operator lock binding for target os=%q workType=%q pathScope=%q", ErrCommandCompositionConflict, alias, scope, strings.Join(claimants, ", "), target.OS, target.WorkType, target.PathScope)
}

func lockedSelection(lock *CompositionLock, target CompositionTarget, alias, scope string) (CommandIdentity, bool) {
	if lock == nil {
		return CommandIdentity{}, false
	}
	for _, entry := range lock.Targets {
		if targetKey(entry.Target) != targetKey(target) {
			continue
		}
		for _, binding := range entry.Bindings {
			if binding.Alias == alias && binding.PathScope == scope {
				return binding.Selected, true
			}
		}
	}
	return CommandIdentity{}, false
}

func validateAppliedLock(bindings []GenericCommandBinding, target CompositionTarget, lock *CompositionLock) error {
	if lock == nil {
		return nil
	}
	resolved := map[string]CommandIdentity{}
	for _, binding := range bindings {
		resolved[binding.Alias+"\x00"+binding.PathScope] = binding.Selected
	}
	for _, entry := range lock.Targets {
		if targetKey(entry.Target) != targetKey(target) {
			continue
		}
		for _, binding := range entry.Bindings {
			selected, ok := resolved[binding.Alias+"\x00"+binding.PathScope]
			if !ok || !sameIdentity(selected, binding.Selected) {
				return fmt.Errorf("%w: stale or unused binding for alias %q scope %q", ErrCompositionLockInvalid, binding.Alias, binding.PathScope)
			}
		}
	}
	return nil
}

type scopedCommandClaims struct {
	scope  string
	claims []QualifiedCommand
}

func claimSetsByScope(claims []QualifiedCommand, targetScope string) []scopedCommandClaims {
	if targetScope != "." {
		var active []QualifiedCommand
		for _, claim := range claims {
			if scopeContains(claim.PathScope, targetScope) {
				active = append(active, claim)
			}
		}
		return []scopedCommandClaims{{scope: targetScope, claims: active}}
	}

	scopeSet := map[string]struct{}{}
	for _, claim := range claims {
		scopeSet[claim.PathScope] = struct{}{}
	}
	scopes := make([]string, 0, len(scopeSet))
	for scope := range scopeSet {
		scopes = append(scopes, scope)
	}
	sort.Slice(scopes, func(i, j int) bool {
		di, dj := scopeDepth(scopes[i]), scopeDepth(scopes[j])
		if di != dj {
			return di < dj
		}
		return scopes[i] < scopes[j]
	})

	out := make([]scopedCommandClaims, 0, len(scopes))
	for _, scope := range scopes {
		var active []QualifiedCommand
		for _, claim := range claims {
			if scopeContains(claim.PathScope, scope) {
				active = append(active, claim)
			}
		}
		out = append(out, scopedCommandClaims{scope: scope, claims: active})
	}
	return out
}

func compositionDigest(plan CommandComposition) (string, error) {
	plan.Digest = ""
	raw, err := json.Marshal(plan)
	if err != nil {
		return "", err
	}
	raw, err = jsoncanonicalizer.Transform(raw)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:]), nil
}

func normalizeScope(value string) string {
	value = strings.ReplaceAll(strings.TrimSpace(value), "\\", "/")
	value = strings.TrimPrefix(value, "./")
	clean := path.Clean(value)
	if clean == "" || clean == "/" || clean == "." {
		return "."
	}
	return strings.TrimPrefix(clean, "/")
}

func safeScopeInput(value string) bool {
	value = strings.TrimSpace(value)
	if value == "" || value == "." {
		return true
	}
	if strings.HasPrefix(value, "/") || strings.Contains(value, "\\") {
		return false
	}
	for _, segment := range strings.Split(value, "/") {
		if segment == ".." {
			return false
		}
	}
	return true
}

func canonicalDelegationScope(value string) bool {
	return value != "" && safeScopeInput(value) && normalizeScope(value) == value
}

func validScope(value string) bool {
	if value == "." {
		return true
	}
	return value != "" && !strings.HasPrefix(value, "/") && value != ".." && !strings.HasPrefix(value, "../") && !strings.Contains(value, "/../") && !strings.Contains(value, "\\")
}

func scopeContains(scope, target string) bool {
	scope, target = normalizeScope(scope), normalizeScope(target)
	return scope == "." || target == scope || strings.HasPrefix(target, scope+"/")
}

func scopeDepth(scope string) int {
	if scope == "." {
		return 0
	}
	return len(strings.Split(scope, "/"))
}

func supportsWorkType(supported []string, target string) bool {
	if target == "" || len(supported) == 0 {
		return true
	}
	for _, value := range supported {
		if value == target {
			return true
		}
	}
	return false
}

func targetKey(target CompositionTarget) string {
	return target.OS + "\x00" + target.WorkType + "\x00" + normalizeScope(target.PathScope)
}

func identityKey(identity CommandIdentity) string {
	return identity.KitID + "\x00" + identity.Name + "\x00" + identity.DigestKind + "\x00" + identity.Digest
}

func sameIdentity(a, b CommandIdentity) bool { return identityKey(a) == identityKey(b) }
func validIdentity(identity CommandIdentity) bool {
	return identity.KitID != "" && identity.Name != "" && (identity.DigestKind == "package" || identity.DigestKind == "legacy-manifest") && validDigest(identity.Digest)
}

func validDigest(value string) bool {
	if len(value) != sha256.Size*2 || strings.ToLower(value) != value {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}

func renderIdentity(identity CommandIdentity) string {
	return fmt.Sprintf("%s:%s@%s:%s", identity.KitID, identity.Name, identity.DigestKind, identity.Digest)
}

func commandIdentityForView(view ManifestView, name string) CommandIdentity {
	identity := CommandIdentity{KitID: view.ID, Name: name, DigestKind: "package", Digest: view.PackageDigest}
	if identity.Digest == "" {
		identity.DigestKind = "legacy-manifest"
		identity.Digest = view.LegacyManifestDigest
	}
	return identity
}

func hasDelegationCycle(edges map[string]string) bool {
	for start := range edges {
		seen := map[string]struct{}{}
		for cursor := start; cursor != ""; cursor = edges[cursor] {
			if _, ok := seen[cursor]; ok {
				return true
			}
			seen[cursor] = struct{}{}
		}
	}
	return false
}

func copyCommands(in map[string]string) map[string]string {
	out := make(map[string]string, len(in))
	for key, value := range in {
		out[key] = value
	}
	return out
}
