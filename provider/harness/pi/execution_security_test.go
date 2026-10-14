package pi

import (
	"context"
	"errors"
	"os"
	"reflect"
	"testing"

	"github.com/RenseiAI/donmai/agent"
	"github.com/RenseiAI/donmai/runtime/confinement"
)

func confinedSecurityStamp() *agent.ExecutionSecurity {
	levels := agent.IndexZeroExecutionSecurityLevels()
	levels.FileWrite = agent.FileWriteWorkarea
	levels.Isolation = agent.IsolationOSSandbox
	return &agent.ExecutionSecurity{Version: agent.ExecutionSecurityWireVersion, Levels: levels}
}

func TestExecutionSecurityUnattestedPiRefusesBeforeSpawn(t *testing.T) {
	old := piConfinementBackend
	piConfinementBackend = func() confinement.Backend { return nil }
	t.Cleanup(func() { piConfinementBackend = old })
	for _, host := range []struct {
		name string
		opts Options
	}{
		{"unconfined", Options{}},
		{"backend absent", Options{RequireConfinement: true}},
		{"processless", Options{RequireConfinement: true, skipProcess: true}},
	} {
		for _, mode := range []string{"spawn", "resume", "interactive"} {
			t.Run(host.name+"/"+mode, func(t *testing.T) {
				p := &Provider{binary: "/must-not-execute", opts: host.opts}
				spec := agent.Spec{Cwd: t.TempDir(), Prompt: "probe", ExecutionSecurity: confinedSecurityStamp()}
				if mode == "interactive" {
					spec.Interactive = &agent.InteractiveSpec{Cols: 80, Rows: 24}
				}
				var err error
				if mode == "resume" {
					_, err = p.Resume(context.Background(), "session", spec)
				} else {
					_, err = p.Spawn(context.Background(), spec)
				}
				var refusal *agent.ExecutionSecurityError
				want := []agent.ExecutionSecurityDimension{agent.ExecutionSecurityFileWrite, agent.ExecutionSecurityIsolation}
				if !errors.Is(err, agent.ErrSpawnFailed) || !errors.As(err, &refusal) || refusal.Code != agent.ExecutionSecurityUnrenderable || !reflect.DeepEqual(refusal.Dimensions, want) {
					t.Fatalf("entry point refusal = %v, want unrenderable write and isolation", err)
				}
				if _, err := os.Stat(sessionStateRoot(spec)); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("refused session materialized state: %v", err)
				}
			})
		}
	}
}
