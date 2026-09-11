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
		return rows[i].Compiled.Declaration.CapabilityID < rows[j].Compiled.Declaration.CapabilityID
	})
	return rows, nil
}
