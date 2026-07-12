package attachclient

import (
	"os"
	"strings"
	"testing"

	"github.com/RenseiAI/donmai/attachclient/attachtest"
)

// TestMain lets the test binary re-exec itself as the stub relay subprocess for
// the kill -9 end-to-end test. When ATTACHTEST_RELAY_MAIN=1 the process runs the
// relay (via attachtest.Main) and never runs the test suite.
func TestMain(m *testing.M) {
	if os.Getenv("ATTACHTEST_RELAY_MAIN") == "1" {
		args := strings.Fields(os.Getenv("ATTACHTEST_RELAY_ARGS"))
		os.Exit(attachtest.Main(args))
	}
	os.Exit(m.Run())
}
