package afcli

import (
	"bytes"
	"context"
	"log/slog"
	"net"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/RenseiAI/donmai/agent"
	"github.com/RenseiAI/donmai/daemon"
	"github.com/RenseiAI/donmai/runner"
	"github.com/RenseiAI/donmai/runtime/statehome"
)

func TestHostRunFirstWizardPrecedesLocalRuntimeComposition(t *testing.T) {
	testHostRunFirstWizardComposition(t, false)
}

func TestHostRunDefaultConfigPersistsBeforeLocalRuntimeComposition(t *testing.T) {
	testHostRunFirstWizardComposition(t, true)
}

func testHostRunFirstWizardComposition(t *testing.T, useDefault bool) {
	t.Helper()
	// This command control stops at the existing unsupported-decorator boundary,
	// before local authority bootstrap, a native harness or service installation.
	// The legacy ordering may bind only the explicitly reserved fixture port.
	home := t.TempDir()
	priorHome := statehome.BaseHome()
	statehome.SetBaseHome(home)
	t.Cleanup(func() { statehome.SetBaseHome(priorHome) })
	t.Chdir(t.TempDir())
	t.Setenv("GITHUB_TOKEN", "synthetic-setup-token")
	oldFactory, oldTTY, oldRegistry := localSetupResolverFactory, setupWizardTTYOverride, daemonRegistryBuilder
	tty := true
	setupWizardTTYOverride = &tty
	calls := 0
	localSetupResolverFactory = func() daemon.LocalRuntimeSetupResolver { calls++; return commandSetupResolver{} }
	daemonRegistryBuilder = func(*slog.Logger, agent.ExtensionDecorator) *runner.Registry { return runner.NewRegistry() }
	t.Cleanup(func() {
		localSetupResolverFactory = oldFactory
		setupWizardTTYOverride = oldTTY
		daemonRegistryBuilder = oldRegistry
	})
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	if err = listener.Close(); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "daemon.yaml")
	if useDefault {
		path = daemon.DefaultConfigPath()
	}
	command := newDaemonRunCmd(Config{AgentSpecExtensionDecorator: func(agent.Spec) []agent.ExtensionDelivery { return nil }})
	input := strings.Join([]string{"", "", "y", "", "", "", "y", "2", "y", "", "acme/repo", "", "", "y", "", "", ""}, "\n") + "\n"
	var output bytes.Buffer
	command.SetIn(strings.NewReader(input))
	command.SetOut(&output)
	command.SetErr(&output)
	args := []string{"--port", strconv.Itoa(port), "--standalone-creds", "off"}
	if !useDefault {
		args = append(args, "--config", path)
	}
	command.SetArgs(args)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	command.SetContext(ctx)
	err = command.Execute()
	if err == nil || !strings.Contains(err.Error(), "local runtime does not yet admit additional extension decorators") {
		t.Fatalf("host run did not compose selected local mode before startup: %v", err)
	}
	cfg, err := daemon.LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg == nil || cfg.LocalRuntime == nil || cfg.LocalRuntime.Repositories[0].RepositoryID != 1234 {
		t.Fatal("first-run resolver output was not persisted before composition")
	}
	if calls != 1 {
		t.Fatalf("first-run wizard resolver factory count=%d, want1 before refusal", calls)
	}
}
