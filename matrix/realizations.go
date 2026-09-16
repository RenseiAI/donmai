package matrix

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"sort"
	"strings"

	"github.com/RenseiAI/donmai/agent"
)

// CapabilityRealizationSource is the generated realization-catalog surface.
type CapabilityRealizationSource struct {
	Declaration agent.CapabilityRealizationDeclaration
	Observation agent.CapabilityFixtureObservation
}

// CapabilityRealizationRow is the generated realization-catalog surface.
type CapabilityRealizationRow struct {
	Compiled agent.CompiledCapabilityRealization `json:"compiled"`
}

// ErrCapabilityFixtureSkipped is returned by a downstream fixture executor
// when its real harness prerequisite is unavailable. A skip can never produce
// realization evidence or a capability eligibility row.
var ErrCapabilityFixtureSkipped = errors.New("capability fixture skipped")

// CapabilityFixtureExecutor executes a downstream fixture against the exact
// declaration being generated. Implementations must return an error for skip
// or failure; there is deliberately no caller-supplied pass flag.
type CapabilityFixtureExecutor interface {
	ExecuteCapabilityFixture(context.Context, agent.CapabilityRealizationDeclaration) (agent.CapabilityFixtureExecution, error)
}

// CapabilityFixtureExecutorFunc adapts a function to CapabilityFixtureExecutor.
type CapabilityFixtureExecutorFunc func(context.Context, agent.CapabilityRealizationDeclaration) (agent.CapabilityFixtureExecution, error)

// ExecuteCapabilityFixture implements CapabilityFixtureExecutor.
func (f CapabilityFixtureExecutorFunc) ExecuteCapabilityFixture(
	ctx context.Context,
	declaration agent.CapabilityRealizationDeclaration,
) (agent.CapabilityFixtureExecution, error) {
	return f(ctx, declaration)
}

// CapabilityRealizationEvidenceSource pairs one declaration with the fixture
// generator/release parity must execute to derive an eligible row.
type CapabilityRealizationEvidenceSource struct {
	Declaration agent.CapabilityRealizationDeclaration
	Executor    CapabilityFixtureExecutor
}

// CapabilityRealizationEvidenceRow is the generated realization catalog row.
type CapabilityRealizationEvidenceRow = agent.CompiledCapabilityRealizationEvidence

// CapabilityEligibilityRow is the derived capability axis. It contains no
// delivery-channel fields; ProductionEligible is emitted only after fixture
// execution and evidence compilation succeeds.
type CapabilityEligibilityRow struct {
	CapabilityID       string                       `json:"capabilityId"`
	HarnessID          agent.HarnessName            `json:"harnessId"`
	AdapterVersion     string                       `json:"adapterVersion"`
	Mode               agent.PromptSessionMode      `json:"mode"`
	EvidenceTier       agent.CapabilityEvidenceTier `json:"evidenceTier"`
	EvidenceDigest     string                       `json:"evidenceDigest"`
	ProductionEligible bool                         `json:"productionEligible"`
}

// CompileCapabilityRealizations is the generated realization-catalog surface.
func CompileCapabilityRealizations(sources []CapabilityRealizationSource) ([]CapabilityRealizationRow, error) {
	_, harnessByName, err := buildHarnesses()
	if err != nil {
		return nil, err
	}
	rows := make([]CapabilityRealizationRow, 0, len(sources))
	for i, s := range sources {
		if err := validateNamedExtensionProfile(s.Declaration, harnessByName); err != nil {
			return nil, fmt.Errorf("compile capability realization source %d: %w", i, err)
		}
		c, err := agent.CompileCapabilityRealization(s.Declaration, s.Observation)
		if err != nil {
			return nil, fmt.Errorf("compile capability realization: %w", err)
		}
		rows = append(rows, CapabilityRealizationRow{Compiled: c})
	}
	sort.Slice(rows, func(i, j int) bool {
		a, b := rows[i].Compiled.Declaration, rows[j].Compiled.Declaration
		if a.CapabilityID != b.CapabilityID {
			return a.CapabilityID < b.CapabilityID
		}
		if a.HarnessID != b.HarnessID {
			return a.HarnessID < b.HarnessID
		}
		if a.AdapterVersion != b.AdapterVersion {
			return a.AdapterVersion < b.AdapterVersion
		}
		return a.Mode < b.Mode
	})
	for i := 1; i < len(rows); i++ {
		a, b := rows[i-1].Compiled.Declaration, rows[i].Compiled.Declaration
		if a.CapabilityID == b.CapabilityID && a.HarnessID == b.HarnessID && a.AdapterVersion == b.AdapterVersion && a.Mode == b.Mode {
			return nil, fmt.Errorf("duplicate capability realization tuple")
		}
	}
	return rows, nil
}

// CompileCapabilityRealizationEvidence executes each fixture, compiles its
// canonical evidence and emits deterministic realization rows. An arbitrary
// executor is not itself proof of a real fixture: the generated artifact's
// producer source digest and mandatory downstream release re-execution form
// that trust boundary.
func CompileCapabilityRealizationEvidence(
	ctx context.Context,
	sources []CapabilityRealizationEvidenceSource,
) ([]CapabilityRealizationEvidenceRow, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := preflightCapabilityRealizationEvidenceSources(sources); err != nil {
		return nil, err
	}
	rows := make([]CapabilityRealizationEvidenceRow, 0, len(sources))
	for _, source := range sources {
		if source.Executor == nil {
			return nil, fmt.Errorf("execute capability fixture: executor is required")
		}
		execution, err := source.Executor.ExecuteCapabilityFixture(ctx, source.Declaration)
		if err != nil {
			return nil, fmt.Errorf("execute capability fixture: %w", err)
		}
		compiled, err := agent.CompileCapabilityRealizationEvidence(source.Declaration, execution)
		if err != nil {
			return nil, fmt.Errorf("compile capability realization evidence: %w", err)
		}
		rows = append(rows, compiled)
	}
	sort.Slice(rows, func(i, j int) bool {
		return realizationDeclarationLess(rows[i].Compiled.Declaration, rows[j].Compiled.Declaration)
	})
	if err := validateCapabilityRealizationEvidenceRows(rows); err != nil {
		return nil, err
	}
	return rows, nil
}

func preflightCapabilityRealizationEvidenceSources(sources []CapabilityRealizationEvidenceSource) error {
	_, harnessByName, err := buildHarnesses()
	if err != nil {
		return err
	}
	seen := make(map[string]struct{}, len(sources))
	for i, source := range sources {
		if source.Executor == nil {
			return fmt.Errorf("execute capability fixture: executor is required")
		}
		canonical, err := agent.NewCapabilityRealization(agent.CapabilityRealizationInput{
			CapabilityID:      source.Declaration.CapabilityID,
			HarnessID:         source.Declaration.HarnessID,
			AdapterVersion:    source.Declaration.AdapterVersion,
			Mode:              source.Declaration.Mode,
			RecipeID:          source.Declaration.Recipe.RecipeID,
			Entries:           source.Declaration.Recipe.Entries,
			DeclaredSurface:   source.Declaration.Recipe.DeclaredSurface,
			ParameterContract: source.Declaration.ParameterContract,
		})
		if err != nil {
			return fmt.Errorf("compile capability realization evidence source %d: %w", i, err)
		}
		if !reflect.DeepEqual(canonical, source.Declaration) {
			return fmt.Errorf("compile capability realization evidence source %d: capability declaration is not canonical", i)
		}
		if err := validateNamedExtensionProfile(canonical, harnessByName); err != nil {
			return fmt.Errorf("compile capability realization evidence source %d: %w", i, err)
		}
		key := capabilityRealizationTupleKey(canonical)
		if _, ok := seen[key]; ok {
			return fmt.Errorf("duplicate capability realization evidence tuple")
		}
		seen[key] = struct{}{}
	}
	return nil
}

func declarationRequestsNamedExtensions(declaration agent.CapabilityRealizationDeclaration) bool {
	for _, entry := range declaration.Recipe.Entries {
		if entry.Channel == agent.ToolChannelToolPlugin && strings.HasPrefix(entry.EntryID, agent.AdditionalExtensionCapabilityEntryPrefix) {
			return true
		}
	}
	return false
}

func validateNamedExtensionProfile(declaration agent.CapabilityRealizationDeclaration, harnessByName map[agent.HarnessName]HarnessRow) error {
	if !declarationRequestsNamedExtensions(declaration) {
		return nil
	}
	harness, ok := harnessByName[declaration.HarnessID]
	if !ok {
		return fmt.Errorf("named extension harness is unknown")
	}
	for _, profile := range harness.ToolLifecycle {
		if profile.ID == declaration.AdapterVersion && profile.Mode == declaration.Mode {
			if !profile.NamedExtensionEntries {
				return fmt.Errorf("selected profile does not opt in to named extension entries")
			}
			return nil
		}
	}
	return fmt.Errorf("selected named extension profile is unknown")
}

func capabilityRealizationTupleKey(d agent.CapabilityRealizationDeclaration) string {
	return d.CapabilityID + "\x00" + string(d.HarnessID) + "\x00" + d.AdapterVersion + "\x00" + string(d.Mode)
}

func capabilityEligibilityRows(realizations []CapabilityRealizationEvidenceRow) []CapabilityEligibilityRow {
	rows := make([]CapabilityEligibilityRow, len(realizations))
	for i, realization := range realizations {
		declaration := realization.Compiled.Declaration
		rows[i] = CapabilityEligibilityRow{
			CapabilityID: declaration.CapabilityID, HarnessID: declaration.HarnessID,
			AdapterVersion: declaration.AdapterVersion, Mode: declaration.Mode,
			EvidenceTier: realization.EvidenceTier, EvidenceDigest: realization.Evidence.EvidenceDigest,
			ProductionEligible: realization.ProductionEligible,
		}
	}
	return rows
}

func validateCapabilityRealizationEvidenceRows(rows []CapabilityRealizationEvidenceRow) error {
	for i, row := range rows {
		if err := agent.ValidateCompiledCapabilityRealizationEvidence(row); err != nil {
			return fmt.Errorf("validate capability realization evidence row %d: %w", i, err)
		}
		if i == 0 {
			continue
		}
		previous := rows[i-1].Compiled.Declaration
		current := row.Compiled.Declaration
		if sameRealizationTuple(previous, current) {
			return fmt.Errorf("duplicate capability realization evidence tuple")
		}
		if !realizationDeclarationLess(previous, current) {
			return fmt.Errorf("capability realization evidence rows are not canonically ordered")
		}
	}
	return nil
}

func deriveCapabilityEligibilityRows(realizations []CapabilityRealizationEvidenceRow) ([]CapabilityEligibilityRow, error) {
	if err := validateCapabilityRealizationEvidenceRows(realizations); err != nil {
		return nil, err
	}
	return capabilityEligibilityRows(realizations), nil
}

func cloneCapabilityRealizationEvidenceRows(rows []CapabilityRealizationEvidenceRow) []CapabilityRealizationEvidenceRow {
	cloned := make([]CapabilityRealizationEvidenceRow, len(rows))
	copy(cloned, rows)
	for i := range cloned {
		compiled := &cloned[i].Compiled
		if compiled.Declaration.ParameterContract != nil {
			contract := *compiled.Declaration.ParameterContract
			compiled.Declaration.ParameterContract = &contract
		}
		compiled.Declaration.Recipe.Entries = append([]agent.CapabilityRecipeEntry(nil), compiled.Declaration.Recipe.Entries...)
		for entryIndex := range compiled.Declaration.Recipe.Entries {
			entry := &compiled.Declaration.Recipe.Entries[entryIndex]
			entry.SurfaceRefs = append([]agent.CapabilitySurfaceIdentity(nil), entry.SurfaceRefs...)
		}
		compiled.Declaration.Recipe.DeclaredSurface = append([]agent.CapabilitySurfaceIdentity(nil), compiled.Declaration.Recipe.DeclaredSurface...)
		compiled.Observation.AppliedArtifacts = append([]agent.CapabilityAppliedArtifact(nil), compiled.Observation.AppliedArtifacts...)
		compiled.Observation.ObservedSurface = append([]agent.CapabilitySurfaceIdentity(nil), compiled.Observation.ObservedSurface...)
	}
	return cloned
}

func cloneCapabilityEligibilityRows(rows []CapabilityEligibilityRow) []CapabilityEligibilityRow {
	cloned := make([]CapabilityEligibilityRow, len(rows))
	copy(cloned, rows)
	return cloned
}

func realizationDeclarationLess(a, b agent.CapabilityRealizationDeclaration) bool {
	if a.CapabilityID != b.CapabilityID {
		return a.CapabilityID < b.CapabilityID
	}
	if a.HarnessID != b.HarnessID {
		return a.HarnessID < b.HarnessID
	}
	if a.AdapterVersion != b.AdapterVersion {
		return a.AdapterVersion < b.AdapterVersion
	}
	return a.Mode < b.Mode
}

func sameRealizationTuple(a, b agent.CapabilityRealizationDeclaration) bool {
	return a.CapabilityID == b.CapabilityID && a.HarnessID == b.HarnessID && a.AdapterVersion == b.AdapterVersion && a.Mode == b.Mode
}
