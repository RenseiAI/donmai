package confinement

import (
	"fmt"
	"os"
)

// nestedCheckEnv re-executes the test binary to run Backend.Check from inside
// an outer profile.
const nestedCheckEnv = "DONMAI_CONFINEMENT_TEST_NESTED_CHECK"

func runNestedCheckFromEnv() (bool, int) {
	if os.Getenv(nestedCheckEnv) == "" {
		return false, 0
	}
	backend := DefaultBackend()
	if backend == nil {
		fmt.Println("reason=backend_absent")
		return true, 0
	}
	err := backend.Check()
	reason, _ := ReasonOf(err)
	fmt.Printf("reason=%s\n", reason)
	return true, 0
}
