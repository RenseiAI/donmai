package workarea

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"
)

const (
	// ProtocolSessionRootV1 is the exact multi-repository workarea protocol.
	ProtocolSessionRootV1 Protocol = "session-root-v1"

	// DeclarationRecordSchemaV1 identifies the secret-free durable record under
	// <workareaRoot>/.workarea/declaration.json.
	DeclarationRecordSchemaV1 = "donmai.workarea-declaration.v1"
	// DeclarationDirName is the reserved metadata leaf under a workarea root.
	DeclarationDirName = ".workarea"
	// DeclarationFileName is the atomic declaration record name.
	DeclarationFileName = "declaration.json"

	// MaxRepositoryLeafBytes is the bounded repository-leaf namespace.
	MaxRepositoryLeafBytes = 128
	maxDeclarationBytes    = 1 << 20
)

// Protocol identifies one exact provisioner/executor contract.
type Protocol string

// RepositoryRole describes why one declared repository is present.
type RepositoryRole string

const (
	// RepositoryRolePrimary is the one default-selection repository.
	RepositoryRolePrimary RepositoryRole = "primary"
	// RepositoryRoleSecondary is an additional session repository.
	RepositoryRoleSecondary RepositoryRole = "secondary"
	// RepositoryRoleContext is reference material.
	RepositoryRoleContext RepositoryRole = "context"
)

// RepositoryAuthority is the immutable filesystem authority of one repository.
type RepositoryAuthority string

const (
	// RepositoryReadOnly denies harness mutation through executor isolation.
	RepositoryReadOnly RepositoryAuthority = "read-only"
	// RepositoryMutable permits harness mutation.
	RepositoryMutable RepositoryAuthority = "mutable"
)

// RepositoryAuthorityEnforcement identifies the executor-owned filesystem
// boundary. Absence and the zero value are exactly "none".
type RepositoryAuthorityEnforcement string

const (
	// RepositoryAuthorityNone is the absent or legacy enforcement value.
	RepositoryAuthorityNone RepositoryAuthorityEnforcement = "none"
	// RepositoryAuthorityIsolatedReadOnlyV1 is the exact non-widenable boundary.
	RepositoryAuthorityIsolatedReadOnlyV1 RepositoryAuthorityEnforcement = "isolated-read-only-v1"
)

// RepositorySource is ephemeral provision input. Repository may contain
// credentials and MUST NOT be copied into a declaration or shim record.
type RepositorySource struct {
	Repository string   `json:"repository"`
	Ref        string   `json:"ref"`
	Paths      []string `json:"paths,omitempty"`
}

// UnmarshalJSON rejects unknown source fields.
func (s *RepositorySource) UnmarshalJSON(data []byte) error {
	type alias RepositorySource
	var decoded alias
	if err := decodeClosedJSON(data, &decoded); err != nil {
		return err
	}
	*s = RepositorySource(decoded)
	return nil
}

// DeclaredRepositoryV1 is one repository on the session-root-v1 executor wire.
type DeclaredRepositoryV1 struct {
	Source    RepositorySource    `json:"source"`
	Name      string              `json:"name,omitempty"`
	Role      RepositoryRole      `json:"role"`
	Authority RepositoryAuthority `json:"authority"`
}

// UnmarshalJSON rejects unknown repository fields.
func (r *DeclaredRepositoryV1) UnmarshalJSON(data []byte) error {
	type alias DeclaredRepositoryV1
	var decoded alias
	if err := decodeClosedJSON(data, &decoded); err != nil {
		return err
	}
	*r = DeclaredRepositoryV1(decoded)
	return nil
}

// RepositoryFilterKind is the closed repository-selection grammar.
type RepositoryFilterKind string

const (
	// RepositoryFilterPrimary selects the one primary repository.
	RepositoryFilterPrimary RepositoryFilterKind = "primary"
	// RepositoryFilterNamed selects one declared name.
	RepositoryFilterNamed RepositoryFilterKind = "named"
	// RepositoryFilterRole selects every repository with one role.
	RepositoryFilterRole RepositoryFilterKind = "role"
	// RepositoryFilterAll selects every declared repository.
	RepositoryFilterAll RepositoryFilterKind = "all"
)

// RepositoryFilter selects declared repositories. An absent filter is the
// primary selection. It is never a fallback rule.
type RepositoryFilter struct {
	Kind RepositoryFilterKind `json:"kind"`
	Name string               `json:"name,omitempty"`
	Role RepositoryRole       `json:"role,omitempty"`
}

// UnmarshalJSON rejects unknown filter fields.
func (f *RepositoryFilter) UnmarshalJSON(data []byte) error {
	type alias RepositoryFilter
	var decoded alias
	if err := decodeClosedJSON(data, &decoded); err != nil {
		return err
	}
	*f = RepositoryFilter(decoded)
	return nil
}

// RepositoryDeclarationV1 is the additive, versioned provision carrier.
type RepositoryDeclarationV1 struct {
	Protocol     Protocol               `json:"protocol"`
	Repositories []DeclaredRepositoryV1 `json:"repositories"`
	Select       *RepositoryFilter      `json:"select,omitempty"`
}

// UnmarshalJSON rejects unknown fields and validates the declaration.
func (d *RepositoryDeclarationV1) UnmarshalJSON(data []byte) error {
	type alias RepositoryDeclarationV1
	var decoded alias
	if err := decodeClosedJSON(data, &decoded); err != nil {
		return err
	}
	declaration := RepositoryDeclarationV1(decoded)
	if _, err := declaration.Normalize(); err != nil {
		return err
	}
	*d = declaration
	return nil
}

// ExecutorWorkareaCapabilities are the exact positive attestations a producer
// and the bound executor compare. Missing protocols means []; missing
// enforcement means none.
type ExecutorWorkareaCapabilities struct {
	MultiRepositoryWorkareaProtocols []Protocol                     `json:"multiRepositoryWorkareaProtocols,omitempty"`
	RepositoryAuthorityEnforcement   RepositoryAuthorityEnforcement `json:"repositoryAuthorityEnforcement,omitempty"`
}

// ExecutorCapabilityAttestation binds workarea capabilities to one exact
// harness adapter reference and its supported session modes. A host-wide bool
// would let an enforcing harness accidentally admit work for a non-enforcing
// sibling harness.
type ExecutorCapabilityAttestation struct {
	HarnessID                   string   `json:"harnessId"`
	AdapterVersion              string   `json:"adapterVersion"`
	ManifestDigest              string   `json:"manifestDigest"`
	SessionModes                []string `json:"sessionModes,omitempty"`
	SupportsReadOnlySelectedCWD bool     `json:"supportsReadOnlySelectedCwd,omitempty"`
	ExecutorWorkareaCapabilities
}

// RepositoryRuleID is a stable contract rule identifier.
type RepositoryRuleID string

const (
	// RuleProtocolExact requires exact protocol negotiation.
	RuleProtocolExact RepositoryRuleID = "workarea.d8.3.protocol-exact"
	// RulePrimarySourceMatch prevents precedence between source carriers.
	RulePrimarySourceMatch RepositoryRuleID = "workarea.d8.5.primary-source-match"
	// RuleSinglePrimary requires one primary declaration.
	RuleSinglePrimary RepositoryRuleID = "workarea.d3.exactly-one-primary"
	// RuleLeafSafe validates one bounded filesystem leaf.
	RuleLeafSafe RepositoryRuleID = "workarea.d5.leaf-safe"
	// RuleLeafUnique refuses automatic disambiguation.
	RuleLeafUnique RepositoryRuleID = "workarea.d5.leaf-unique"
	// RuleFilterDeclared requires named selections to exist.
	RuleFilterDeclared RepositoryRuleID = "workarea.d4.3.named-declared"
	// RuleFilterNonEmpty refuses required zero-match selections.
	RuleFilterNonEmpty RepositoryRuleID = "workarea.d4.4.non-empty"
	// RuleFilterSingle refuses first-wins selection.
	RuleFilterSingle RepositoryRuleID = "workarea.d4.5.single-selection"
	// RuleReadOnlyExecutorEnforced requires a filesystem boundary.
	RuleReadOnlyExecutorEnforced RepositoryRuleID = "workarea.d6.6.executor-read-only"
	// RuleDeclarationRecordSecretFree keeps durable discovery credential-free.
	RuleDeclarationRecordSecretFree RepositoryRuleID = "workarea.d2.3.secret-free-record"
)

// RepositoryReasonCode is the closed error reason registry. Human-readable
// details are display-only and consumers must branch on Reason and RuleID.
type RepositoryReasonCode string

const (
	// ReasonProtocolUnsupported means the exact protocol was not attested.
	ReasonProtocolUnsupported RepositoryReasonCode = "workarea_protocol_unsupported"
	// ReasonPrimarySourceMismatch means additive and legacy sources disagree.
	ReasonPrimarySourceMismatch RepositoryReasonCode = "primary_source_mismatch"
	// ReasonPrimaryCardinality means there is not exactly one primary.
	ReasonPrimaryCardinality RepositoryReasonCode = "primary_cardinality"
	// ReasonRepositoryDeclarationInvalid means the declaration is malformed.
	ReasonRepositoryDeclarationInvalid RepositoryReasonCode = "repository_declaration_invalid"
	// ReasonRepositoryLeafInvalid means a leaf is unsafe or out of bounds.
	ReasonRepositoryLeafInvalid RepositoryReasonCode = "repository_leaf_invalid"
	// ReasonRepositoryLeafCollision means two entries alias one leaf.
	ReasonRepositoryLeafCollision RepositoryReasonCode = "repository_leaf_collision"
	// ReasonRepositoryUndeclared means an explicit name was not declared.
	ReasonRepositoryUndeclared RepositoryReasonCode = "repository_undeclared"
	// ReasonRepositoryFilterZeroMatch means a required filter matched nothing.
	ReasonRepositoryFilterZeroMatch RepositoryReasonCode = "repository_filter_zero_match"
	// ReasonRepositoryFilterAmbiguous means a single selection matched many.
	ReasonRepositoryFilterAmbiguous RepositoryReasonCode = "repository_filter_ambiguous"
	// ReasonAuthorityEnforcementMissing means read-only isolation was absent.
	ReasonAuthorityEnforcementMissing RepositoryReasonCode = "repository_authority_enforcement_missing"
	// ReasonDeclarationRecordInvalid means durable root metadata is unsafe.
	ReasonDeclarationRecordInvalid RepositoryReasonCode = "workarea_declaration_record_invalid"
)

// RepositoryContractError is a typed fail-closed declaration/filter error.
// Repository and OtherRepository are declared names, never source URLs.
type RepositoryContractError struct {
	Reason          RepositoryReasonCode
	RuleID          RepositoryRuleID
	Repository      string
	OtherRepository string
	Detail          string
}

func (e *RepositoryContractError) Error() string {
	if e == nil {
		return "runtime/workarea: repository contract error"
	}
	msg := fmt.Sprintf("runtime/workarea: repository contract denied (%s, rule=%s)", e.Reason, e.RuleID)
	if e.Repository != "" {
		msg += ": repository=" + e.Repository
	}
	if e.OtherRepository != "" {
		msg += ", other=" + e.OtherRepository
	}
	if e.Detail != "" {
		msg += ": " + e.Detail
	}
	return msg
}

// NormalizedRepository is a validated declaration entry. Source remains
// ephemeral and is deliberately absent from DeclarationRepositoryRecord.
type NormalizedRepository struct {
	Source    RepositorySource
	Name      string
	Leaf      string
	Role      RepositoryRole
	Authority RepositoryAuthority
	Index     int
}

// NormalizedDeclaration is the validated in-memory provision plan.
type NormalizedDeclaration struct {
	Protocol     Protocol
	Repositories []NormalizedRepository
	Selected     NormalizedRepository
}

var repositoryLeafPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)

// RepositoryLeaf derives the canonical leaf from a URL basename, stripping a
// trailing .git. Invalid derived names are refused; they are never sanitized or
// automatically disambiguated.
func RepositoryLeaf(repository string) (string, error) {
	trimmed := strings.TrimRight(strings.TrimSpace(repository), "/")
	if trimmed == "" {
		return "", repositoryError(ReasonRepositoryLeafInvalid, RuleLeafSafe, "", "repository source has no basename")
	}
	pathSource := trimmed
	if parsed, err := url.Parse(trimmed); err == nil && parsed.Path != "" {
		pathSource = parsed.Path
	}
	leaf := strings.TrimSuffix(path.Base(strings.ReplaceAll(pathSource, `\`, "/")), ".git")
	if err := ValidateRepositoryLeaf(leaf); err != nil {
		return "", err
	}
	return leaf, nil
}

// ValidateRepositoryLeaf enforces the refusal-based, per-session namespace.
func ValidateRepositoryLeaf(leaf string) error {
	switch {
	case leaf == "":
		return repositoryError(ReasonRepositoryLeafInvalid, RuleLeafSafe, leaf, "leaf is empty")
	case len(leaf) > MaxRepositoryLeafBytes:
		return repositoryError(ReasonRepositoryLeafInvalid, RuleLeafSafe, leaf, "leaf exceeds the declared length bound")
	case leaf == "." || leaf == ".." || strings.HasPrefix(leaf, "."):
		return repositoryError(ReasonRepositoryLeafInvalid, RuleLeafSafe, leaf, "dot and leading-dot leaves are reserved")
	case strings.ContainsAny(leaf, `/\`):
		return repositoryError(ReasonRepositoryLeafInvalid, RuleLeafSafe, leaf, "leaf contains a path separator")
	case !repositoryLeafPattern.MatchString(leaf):
		return repositoryError(ReasonRepositoryLeafInvalid, RuleLeafSafe, leaf, "leaf contains a character outside [A-Za-z0-9._-]")
	default:
		return nil
	}
}

// Normalize validates the declaration and resolves exactly one selected
// repository. It never falls back from an explicit selection to primary.
func (d RepositoryDeclarationV1) Normalize() (NormalizedDeclaration, error) {
	if d.Protocol != ProtocolSessionRootV1 {
		return NormalizedDeclaration{}, repositoryError(ReasonProtocolUnsupported, RuleProtocolExact, "", "exact protocol session-root-v1 is required")
	}
	if len(d.Repositories) == 0 {
		return NormalizedDeclaration{}, repositoryError(ReasonRepositoryDeclarationInvalid, RuleSinglePrimary, "", "repositories is empty")
	}

	result := NormalizedDeclaration{Protocol: d.Protocol, Repositories: make([]NormalizedRepository, 0, len(d.Repositories))}
	byLeaf := make(map[string]NormalizedRepository, len(d.Repositories))
	primaryCount := 0
	for i, declared := range d.Repositories {
		if strings.TrimSpace(declared.Source.Repository) == "" {
			return NormalizedDeclaration{}, repositoryError(ReasonRepositoryDeclarationInvalid, RuleLeafSafe, displayRepositoryName(declared, i), "repository source is empty")
		}
		for _, sparsePath := range declared.Source.Paths {
			if err := validateSparsePath(sparsePath); err != nil {
				return NormalizedDeclaration{}, repositoryError(ReasonRepositoryDeclarationInvalid, RuleLeafSafe, displayRepositoryName(declared, i), err.Error())
			}
		}
		leaf := strings.TrimSpace(declared.Name)
		var err error
		if leaf == "" {
			leaf, err = RepositoryLeaf(declared.Source.Repository)
		} else {
			err = ValidateRepositoryLeaf(leaf)
		}
		if err != nil {
			var contractErr *RepositoryContractError
			if errors.As(err, &contractErr) {
				contractErr.Repository = displayRepositoryName(declared, i)
			}
			return NormalizedDeclaration{}, err
		}
		role := declared.Role
		switch role {
		case RepositoryRolePrimary:
			primaryCount++
		case RepositoryRoleSecondary, RepositoryRoleContext:
		default:
			return NormalizedDeclaration{}, repositoryError(ReasonRepositoryDeclarationInvalid, RuleSinglePrimary, leaf, "unknown repository role")
		}
		authority := declared.Authority
		if authority == "" {
			authority = RepositoryReadOnly
		}
		if authority != RepositoryReadOnly && authority != RepositoryMutable {
			return NormalizedDeclaration{}, repositoryError(ReasonRepositoryDeclarationInvalid, RuleReadOnlyExecutorEnforced, leaf, "unknown repository authority")
		}
		normalized := NormalizedRepository{Source: declared.Source, Name: leaf, Leaf: leaf, Role: role, Authority: authority, Index: i}
		leafKey := strings.ToLower(leaf)
		if other, exists := byLeaf[leafKey]; exists {
			return NormalizedDeclaration{}, &RepositoryContractError{
				Reason: ReasonRepositoryLeafCollision, RuleID: RuleLeafUnique,
				Repository:      fmt.Sprintf("repositories[%d]:%s", other.Index, other.Name),
				OtherRepository: fmt.Sprintf("repositories[%d]:%s", normalized.Index, normalized.Name),
				Detail:          "two declared repositories resolve to the same leaf",
			}
		}
		byLeaf[leafKey] = normalized
		result.Repositories = append(result.Repositories, normalized)
	}
	if primaryCount != 1 {
		return NormalizedDeclaration{}, repositoryError(ReasonPrimaryCardinality, RuleSinglePrimary, "", fmt.Sprintf("got %d primary repositories; want exactly one", primaryCount))
	}
	selected, err := result.ResolveOne(d.Select)
	if err != nil {
		return NormalizedDeclaration{}, err
	}
	result.Selected = selected
	return result, nil
}

// ValidatePrimarySource proves the additive declaration agrees with the legacy
// singular source. Neither side wins by precedence.
func (d NormalizedDeclaration) ValidatePrimarySource(source RepositorySource) error {
	for _, repository := range d.Repositories {
		if repository.Role != RepositoryRolePrimary {
			continue
		}
		pathsMatch := len(source.Paths) == 0 || equalStrings(repository.Source.Paths, source.Paths)
		if repository.Source.Repository != source.Repository || repository.Source.Ref != source.Ref || !pathsMatch {
			return repositoryError(ReasonPrimarySourceMismatch, RulePrimarySourceMatch, repository.Name, "primary source does not match the legacy singular source")
		}
		return nil
	}
	return repositoryError(ReasonPrimaryCardinality, RuleSinglePrimary, "", "primary repository is absent")
}

// Resolve resolves a filter against the declaration record. Callers whose
// contract admits multiple or zero results can inspect the returned slice.
func (d NormalizedDeclaration) Resolve(filter *RepositoryFilter) ([]NormalizedRepository, error) {
	effective := RepositoryFilter{Kind: RepositoryFilterPrimary}
	if filter != nil {
		effective = *filter
	}
	var matches []NormalizedRepository
	switch effective.Kind {
	case RepositoryFilterPrimary:
		if effective.Name != "" || effective.Role != "" {
			return nil, repositoryError(ReasonRepositoryDeclarationInvalid, RuleFilterDeclared, "", "primary filter carries unrelated fields")
		}
		for _, repository := range d.Repositories {
			if repository.Role == RepositoryRolePrimary {
				matches = append(matches, repository)
			}
		}
	case RepositoryFilterNamed:
		if effective.Name == "" || effective.Role != "" {
			return nil, repositoryError(ReasonRepositoryDeclarationInvalid, RuleFilterDeclared, effective.Name, "named filter requires only name")
		}
		for _, repository := range d.Repositories {
			if repository.Name == effective.Name {
				matches = append(matches, repository)
			}
		}
		if len(matches) == 0 {
			return nil, repositoryError(ReasonRepositoryUndeclared, RuleFilterDeclared, effective.Name, "named repository is not declared")
		}
	case RepositoryFilterRole:
		if effective.Name != "" || !knownRole(effective.Role) {
			return nil, repositoryError(ReasonRepositoryDeclarationInvalid, RuleFilterNonEmpty, effective.Name, "role filter requires one known role")
		}
		for _, repository := range d.Repositories {
			if repository.Role == effective.Role {
				matches = append(matches, repository)
			}
		}
	case RepositoryFilterAll:
		if effective.Name != "" || effective.Role != "" {
			return nil, repositoryError(ReasonRepositoryDeclarationInvalid, RuleFilterNonEmpty, "", "all filter carries unrelated fields")
		}
		matches = append(matches, d.Repositories...)
	default:
		return nil, repositoryError(ReasonRepositoryDeclarationInvalid, RuleFilterDeclared, "", "unknown repository filter kind")
	}
	return matches, nil
}

// ResolveOne requires exactly one repository. Zero and multiple matches are
// typed failures rather than empty success or first-wins selection.
func (d NormalizedDeclaration) ResolveOne(filter *RepositoryFilter) (NormalizedRepository, error) {
	matches, err := d.Resolve(filter)
	if err != nil {
		return NormalizedRepository{}, err
	}
	if len(matches) == 0 {
		return NormalizedRepository{}, repositoryError(ReasonRepositoryFilterZeroMatch, RuleFilterNonEmpty, "", "repository filter matched zero declarations")
	}
	if len(matches) != 1 {
		return NormalizedRepository{}, repositoryError(ReasonRepositoryFilterAmbiguous, RuleFilterSingle, "", fmt.Sprintf("repository filter matched %d declarations", len(matches)))
	}
	return matches[0], nil
}

// HasReadOnly reports whether the executor must enforce a read-only leaf.
func (d NormalizedDeclaration) HasReadOnly() bool {
	for _, repository := range d.Repositories {
		if repository.Authority == RepositoryReadOnly {
			return true
		}
	}
	return false
}

// ValidateFor checks exact protocol and read-only enforcement attestations.
func (c ExecutorWorkareaCapabilities) ValidateFor(declaration NormalizedDeclaration) error {
	supported := false
	for _, protocol := range c.MultiRepositoryWorkareaProtocols {
		if protocol == declaration.Protocol {
			supported = true
			break
		}
	}
	if !supported {
		return repositoryError(ReasonProtocolUnsupported, RuleProtocolExact, declaration.Selected.Name, "bound executor did not attest the exact workarea protocol")
	}
	if declaration.HasReadOnly() && c.RepositoryAuthorityEnforcement != RepositoryAuthorityIsolatedReadOnlyV1 {
		return repositoryError(ReasonAuthorityEnforcementMissing, RuleReadOnlyExecutorEnforced, declaration.Selected.Name, "bound executor did not attest isolated-read-only-v1")
	}
	return nil
}

// DeclarationRepositoryRecord is the durable secret-free projection of one
// repository. It intentionally has no URL/source field.
type DeclarationRepositoryRecord struct {
	Name         string              `json:"name"`
	Leaf         string              `json:"leaf"`
	Role         RepositoryRole      `json:"role"`
	Authority    RepositoryAuthority `json:"authority"`
	RequestedRef string              `json:"requestedRef,omitempty"`
	ResolvedRef  string              `json:"resolvedRef,omitempty"`
}

// DeclarationRecord is the durable source of truth for root ownership and
// restart adoption. Directory listings never substitute for this record.
type DeclarationRecord struct {
	SchemaVersion      string                        `json:"schemaVersion"`
	Protocol           Protocol                      `json:"protocol"`
	SessionID          string                        `json:"sessionId"`
	WorkareaID         string                        `json:"workareaId"`
	SelectedRepository string                        `json:"selectedRepository"`
	Repositories       []DeclarationRepositoryRecord `json:"repositories"`
}

// NewDeclarationRecord builds the secret-free durable projection. Resolved
// refs are keyed by normalized repository name; absent means not yet resolved.
func NewDeclarationRecord(sessionID, workareaID string, declaration NormalizedDeclaration, resolvedRefs map[string]string) DeclarationRecord {
	record := DeclarationRecord{
		SchemaVersion: DeclarationRecordSchemaV1,
		Protocol:      declaration.Protocol, SessionID: sessionID, WorkareaID: workareaID,
		SelectedRepository: declaration.Selected.Name,
		Repositories:       make([]DeclarationRepositoryRecord, 0, len(declaration.Repositories)),
	}
	for _, repository := range declaration.Repositories {
		record.Repositories = append(record.Repositories, DeclarationRepositoryRecord{
			Name: repository.Name, Leaf: repository.Leaf, Role: repository.Role,
			Authority: repository.Authority, RequestedRef: repository.Source.Ref,
			ResolvedRef: resolvedRefs[repository.Name],
		})
	}
	return record
}

// Validate proves the durable record is internally coherent and secret-free by
// construction (there is no source/URL member in its closed type).
func (r DeclarationRecord) Validate() error {
	if r.SchemaVersion != DeclarationRecordSchemaV1 || r.Protocol != ProtocolSessionRootV1 || r.SessionID == "" || r.WorkareaID == "" || len(r.Repositories) == 0 {
		return repositoryError(ReasonDeclarationRecordInvalid, RuleDeclarationRecordSecretFree, r.SelectedRepository, "record header is incomplete or unsupported")
	}
	selected := false
	primary := 0
	seen := make(map[string]struct{}, len(r.Repositories))
	for _, repository := range r.Repositories {
		if repository.Name == "" || repository.Name != repository.Leaf {
			return repositoryError(ReasonDeclarationRecordInvalid, RuleLeafSafe, repository.Name, "record name and leaf must match")
		}
		if err := ValidateRepositoryLeaf(repository.Leaf); err != nil {
			return err
		}
		leafKey := strings.ToLower(repository.Leaf)
		if _, exists := seen[leafKey]; exists {
			return repositoryError(ReasonRepositoryLeafCollision, RuleLeafUnique, repository.Name, "record contains a duplicate leaf")
		}
		seen[leafKey] = struct{}{}
		if repository.Role == RepositoryRolePrimary {
			primary++
		} else if repository.Role != RepositoryRoleSecondary && repository.Role != RepositoryRoleContext {
			return repositoryError(ReasonDeclarationRecordInvalid, RuleSinglePrimary, repository.Name, "record contains an unknown role")
		}
		if repository.Authority != RepositoryReadOnly && repository.Authority != RepositoryMutable {
			return repositoryError(ReasonDeclarationRecordInvalid, RuleReadOnlyExecutorEnforced, repository.Name, "record contains an unknown authority")
		}
		selected = selected || repository.Name == r.SelectedRepository
	}
	if primary != 1 || !selected {
		return repositoryError(ReasonDeclarationRecordInvalid, RuleSinglePrimary, r.SelectedRepository, "record must contain one primary and its selected repository")
	}
	return nil
}

// DeclarationPath returns the exact durable record path.
func DeclarationPath(root RootPath) string {
	return filepath.Join(root.String(), DeclarationDirName, DeclarationFileName)
}

// WriteDeclaration atomically persists a mode-0600 secret-free record under a
// mode-0700 metadata directory, with file and directory fsync before success.
func WriteDeclaration(_ context.Context, root RootPath, record DeclarationRecord) error {
	if !filepath.IsAbs(root.String()) {
		return repositoryError(ReasonDeclarationRecordInvalid, RuleDeclarationRecordSecretFree, record.SelectedRepository, "workarea root is not absolute")
	}
	if err := record.Validate(); err != nil {
		return err
	}
	rootInfo, err := os.Lstat(root.String())
	if err != nil {
		return fmt.Errorf("runtime/workarea: inspect declaration root: %w", err)
	}
	if !rootInfo.IsDir() || rootInfo.Mode()&os.ModeSymlink != 0 {
		return repositoryError(ReasonDeclarationRecordInvalid, RuleDeclarationRecordSecretFree, record.SelectedRepository, "workarea root is not a real directory")
	}
	rootHandle, err := os.OpenRoot(root.String())
	if err != nil {
		return fmt.Errorf("runtime/workarea: open declaration root: %w", err)
	}
	defer func() { _ = rootHandle.Close() }()
	body, err := json.Marshal(record)
	if err != nil {
		return fmt.Errorf("runtime/workarea: encode declaration record: %w", err)
	}
	metadataDir := filepath.Join(root.String(), DeclarationDirName)
	if err := rootHandle.MkdirAll(DeclarationDirName, 0o700); err != nil {
		return fmt.Errorf("runtime/workarea: create declaration directory: %w", err)
	}
	metadataInfo, err := rootHandle.Lstat(DeclarationDirName)
	if err != nil {
		return fmt.Errorf("runtime/workarea: inspect declaration directory: %w", err)
	}
	if !metadataInfo.IsDir() || metadataInfo.Mode()&os.ModeSymlink != 0 || metadataInfo.Mode().Perm() != 0o700 {
		return repositoryError(ReasonDeclarationRecordInvalid, RuleDeclarationRecordSecretFree, record.SelectedRepository, "declaration directory is not a real directory")
	}
	tmp, err := os.CreateTemp(metadataDir, ".declaration-*.tmp")
	if err != nil {
		return fmt.Errorf("runtime/workarea: create declaration temp file: %w", err)
	}
	tmpPath := tmp.Name()
	committed := false
	defer func() {
		_ = tmp.Close()
		if !committed {
			_ = os.Remove(tmpPath)
		}
	}()
	if err := tmp.Chmod(0o600); err != nil {
		return fmt.Errorf("runtime/workarea: secure declaration temp file: %w", err)
	}
	if _, err := tmp.Write(body); err != nil {
		return fmt.Errorf("runtime/workarea: write declaration: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		return fmt.Errorf("runtime/workarea: fsync declaration: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("runtime/workarea: close declaration: %w", err)
	}
	finalPath := DeclarationPath(root)
	if err := os.Rename(tmpPath, finalPath); err != nil {
		return fmt.Errorf("runtime/workarea: publish declaration: %w", err)
	}
	dir, err := rootHandle.Open(DeclarationDirName)
	if err != nil {
		return fmt.Errorf("runtime/workarea: open declaration directory for fsync: %w", err)
	}
	if err := dir.Sync(); err != nil {
		_ = dir.Close()
		return fmt.Errorf("runtime/workarea: fsync declaration directory: %w", err)
	}
	if err := dir.Close(); err != nil {
		return fmt.Errorf("runtime/workarea: close declaration directory: %w", err)
	}
	committed = true
	return nil
}

// ReadDeclaration loads the closed, bounded durable record.
func ReadDeclaration(root RootPath) (DeclarationRecord, error) {
	declarationPath := DeclarationPath(root)
	info, err := os.Lstat(declarationPath)
	if err != nil {
		return DeclarationRecord{}, fmt.Errorf("runtime/workarea: inspect declaration: %w", err)
	}
	if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm()&0o077 != 0 {
		return DeclarationRecord{}, repositoryError(ReasonDeclarationRecordInvalid, RuleDeclarationRecordSecretFree, "", "declaration file type or mode is unsafe")
	}
	rootHandle, err := os.OpenRoot(root.String())
	if err != nil {
		return DeclarationRecord{}, fmt.Errorf("runtime/workarea: open declaration root: %w", err)
	}
	defer func() { _ = rootHandle.Close() }()
	file, err := rootHandle.Open(filepath.Join(DeclarationDirName, DeclarationFileName))
	if err != nil {
		return DeclarationRecord{}, fmt.Errorf("runtime/workarea: open declaration: %w", err)
	}
	defer func() { _ = file.Close() }()
	body, err := io.ReadAll(io.LimitReader(file, maxDeclarationBytes+1))
	if err != nil {
		return DeclarationRecord{}, fmt.Errorf("runtime/workarea: read declaration: %w", err)
	}
	if len(body) > maxDeclarationBytes {
		return DeclarationRecord{}, repositoryError(ReasonDeclarationRecordInvalid, RuleDeclarationRecordSecretFree, "", "declaration exceeds size bound")
	}
	var record DeclarationRecord
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&record); err != nil {
		return DeclarationRecord{}, fmt.Errorf("runtime/workarea: decode declaration: %w", err)
	}
	if err := ensureJSONEOF(decoder); err != nil {
		return DeclarationRecord{}, err
	}
	if err := record.Validate(); err != nil {
		return DeclarationRecord{}, err
	}
	return record, nil
}

func ensureJSONEOF(decoder *json.Decoder) error {
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("runtime/workarea: trailing declaration value")
		}
		return fmt.Errorf("runtime/workarea: trailing declaration data: %w", err)
	}
	return nil
}

func decodeClosedJSON(data []byte, target any) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	return ensureJSONEOF(decoder)
}

func knownRole(role RepositoryRole) bool {
	return role == RepositoryRolePrimary || role == RepositoryRoleSecondary || role == RepositoryRoleContext
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func validateSparsePath(value string) error {
	cleaned := filepath.Clean(strings.TrimSpace(value))
	if cleaned == "" || cleaned == "." || filepath.IsAbs(cleaned) || cleaned == ".." || strings.HasPrefix(cleaned, ".."+string(filepath.Separator)) {
		return fmt.Errorf("sparse path %q is not a relative repository path", value)
	}
	return nil
}

func displayRepositoryName(repository DeclaredRepositoryV1, index int) string {
	if name := strings.TrimSpace(repository.Name); name != "" {
		return name
	}
	if leaf, err := RepositoryLeaf(repository.Source.Repository); err == nil {
		return leaf
	}
	return fmt.Sprintf("repositories[%d]", index)
}

func repositoryError(reason RepositoryReasonCode, rule RepositoryRuleID, repository, detail string) *RepositoryContractError {
	return &RepositoryContractError{Reason: reason, RuleID: rule, Repository: repository, Detail: detail}
}
