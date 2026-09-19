package runner

import (
	"fmt"

	"github.com/RenseiAI/donmai/prompt"
)

// resolveCLIExecutableName captures the process-owned executable basename.
// Validation is shared with prompt.Builder so every construction path accepts
// exactly the same grammar.
func resolveCLIExecutableName(configured string) (string, error) {
	name, err := prompt.ResolveCLIExecutableName(configured)
	if err != nil {
		return "", fmt.Errorf("runner: CLI executable name is malformed: %w", err)
	}
	return name, nil
}
