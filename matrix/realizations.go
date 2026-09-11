package matrix

import (
	"fmt"
	"sort"

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

// CompileCapabilityRealizations is the generated realization-catalog surface.
func CompileCapabilityRealizations(sources []CapabilityRealizationSource) ([]CapabilityRealizationRow, error) {
	rows := make([]CapabilityRealizationRow, 0, len(sources))
	for _, s := range sources {
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
