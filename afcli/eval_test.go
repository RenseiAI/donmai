package afcli

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/spf13/cobra"

	eval "github.com/RenseiAI/donmai/eval/codeintel"
)

// writeMinimalBenchmarkDir writes a one-case benchmark JSONL dir that passes
// eval.LoadCasesDir validation, so the command reaches driver construction.
func writeMinimalBenchmarkDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	line := `{"id":"codeintel-find-symbol-x-001","input":{"taskType":"find-symbol","repo":"r","ref":"deadbeef","prompt":"Where is X defined?"},"expectedOutput":{"file":"a.go","lineRange":[1,2]},"tags":["codeintel-eval","find-symbol","v1"]}` + "\n"
	if err := os.WriteFile(filepath.Join(dir, "find-symbol.jsonl"), []byte(line), 0o600); err != nil {
		t.Fatal(err)
	}
	return dir
}

// TestEvalCodeIntel_AdvertiseAllToolsPropagatesToDriverConfig proves the
// --advertise-all-tools flag reaches eval.Config.AdvertiseAllTools (and that
// the default leaves it false), via the evalNewDriver test seam — the same
// hook pattern the github/linear commands use.
func TestEvalCodeIntel_AdvertiseAllToolsPropagatesToDriverConfig(t *testing.T) {
	benchDir := writeMinimalBenchmarkDir(t)

	var captured []eval.Config
	sentinel := errors.New("driver config captured")
	orig := evalNewDriver
	evalNewDriver = func(cfg eval.Config) (*eval.Driver, error) {
		captured = append(captured, cfg)
		return nil, sentinel
	}
	defer func() { evalNewDriver = orig }()

	run := func(extra ...string) {
		t.Helper()
		root := &cobra.Command{Use: "donmai", SilenceUsage: true, SilenceErrors: true}
		root.AddCommand(newEvalCmd(Config{}))
		args := append([]string{
			"eval", "codeintel",
			"--benchmark-dir", benchDir,
			"--donmai-bin", "/bin/echo",
		}, extra...)
		root.SetArgs(args)
		if err := root.Execute(); !errors.Is(err, sentinel) {
			t.Fatalf("expected the captured-config sentinel error, got %v", err)
		}
	}

	run("--advertise-all-tools")
	run()

	if len(captured) != 2 {
		t.Fatalf("captured %d configs, want 2", len(captured))
	}
	if !captured[0].AdvertiseAllTools {
		t.Errorf("--advertise-all-tools must set Config.AdvertiseAllTools; got %+v", captured[0].AdvertiseAllTools)
	}
	if captured[1].AdvertiseAllTools {
		t.Errorf("default run must leave Config.AdvertiseAllTools false")
	}
	// Sanity on adjacent wiring: the default advertise mode reaches the Config.
	if captured[0].Advertise != eval.AdvertiseMCP {
		t.Errorf("Config.Advertise = %q, want %q", captured[0].Advertise, eval.AdvertiseMCP)
	}
}
