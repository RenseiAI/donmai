package matrix

import (
	"fmt"
	"sort"

	"github.com/RenseiAI/donmai/agent"
)

// CapabilityRealizationRow is the derived matrix projection of a declaration.
type CapabilityRealizationRow struct {
	Declaration        agent.CapabilityRealizationDeclaration `json:"declaration"`
	ProductionEligible bool                                   `json:"productionEligible"`
}

// CompileCapabilityRealizations derives sorted production rows from fixture evidence.
func CompileCapabilityRealizations(declarations []agent.CapabilityRealizationDeclaration) ([]CapabilityRealizationRow, error) {
	if _, err := agent.NewCapabilityRealizationRegistry(declarations); err != nil {
		return nil, err
	}
	rows := make([]CapabilityRealizationRow, 0, len(declarations))
	for _, d := range declarations {
		if !d.ProductionEligible() {
			return nil, fmt.Errorf("capability realization %q lacks production evidence", d.CapabilityID)
		}
		rows = append(rows, CapabilityRealizationRow{Declaration: d, ProductionEligible: true})
	}
	sort.Slice(rows, func(i, j int) bool {
		a, b := rows[i].Declaration, rows[j].Declaration
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
	return rows, nil
}
