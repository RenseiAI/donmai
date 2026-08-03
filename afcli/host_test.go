package afcli

import (
	"bytes"
	"sort"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/RenseiAI/donmai/afclient"
)

// wantHostLeaves pins the `host` noun's immediate children.
//
// This is a deliberate tripwire, not a restatement: `host` is the surface a
// composing downstream binary consumes wholesale via NewHostCmd, so a leaf
// silently dropped here silently disappears from that binary too, with no
// compile error anywhere. Adding a leaf is a one-line edit to this list;
// dropping one has to be argued for.
//
// Grouped by what D2 assigns to the noun:
//   - daemon lifecycle
//   - this machine's capacity envelope (set, stats) and workarea pool
//   - installed providers and kits
//   - project admission
//   - the local live-session dashboard
var wantHostLeaves = []string{
	// lifecycle
	"doctor", "drain", "evict", "install", "logs", "pause", "resume", "run",
	"set", "setup", "stats", "status", "stop", "uninstall", "update",
	// pool / providers / kits / projects / dashboard
	"kit", "project", "provider", "watch", "workarea",
}

// wantDaemonAliasLeaves pins what the hidden `daemon` alias forwards. It is the
// lifecycle subset of `host` — exactly what `daemon` carried before it became an
// alias, so no existing invocation lost a subcommand.
var wantDaemonAliasLeaves = []string{
	"doctor", "drain", "evict", "install", "logs", "pause", "resume", "run",
	"set", "setup", "stats", "status", "stop", "uninstall", "update",
}

func testHostConfig() Config {
	return Config{
		ClientFactory:     func() afclient.DataSource { return afclient.NewMockClient() },
		HostBinaryVersion: "test",
	}
}

func testDataSource() func() afclient.DataSource {
	return func() afclient.DataSource { return afclient.NewMockClient() }
}

// TestNewHostCmd_LeafSet pins the exported factory's leaf set.
func TestNewHostCmd_LeafSet(t *testing.T) {
	t.Parallel()

	cmd := NewHostCmd(testDataSource(), testHostConfig())

	if cmd.Name() != "host" {
		t.Fatalf("Name() = %q, want host", cmd.Name())
	}
	if cmd.Hidden {
		t.Error("host must be visible — it is the canonical noun for this machine")
	}
	if cmd.Deprecated != "" {
		t.Errorf("host must not be deprecated, got %q", cmd.Deprecated)
	}

	got := subcommandNames(cmd)
	want := append([]string(nil), wantHostLeaves...)
	sort.Strings(want)
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("host leaf set drifted.\n got: %v\nwant: %v", got, want)
	}
}

// TestNewHostCmd_ReturnsFreshTrees guards the property exports.go promises: the
// factory can be grafted under more than one parent because each call builds
// independent cobra commands (a cobra command has a single parent pointer).
func TestNewHostCmd_ReturnsFreshTrees(t *testing.T) {
	t.Parallel()

	a := NewHostCmd(testDataSource(), testHostConfig())
	b := NewHostCmd(testDataSource(), testHostConfig())
	if a == b {
		t.Fatal("NewHostCmd returned the same instance twice")
	}
	for _, ac := range a.Commands() {
		for _, bc := range b.Commands() {
			if ac.Name() == bc.Name() && ac == bc {
				t.Errorf("subcommand %q is shared between trees", ac.Name())
			}
		}
	}
}

// TestHostSubcommandsResolve asserts every leaf resolves by path through cobra's
// own dispatcher — the thing an operator's `donmai host install` depends on.
func TestHostSubcommandsResolve(t *testing.T) {
	t.Parallel()

	nested := []string{"kit", "project", "provider", "workarea"}

	for _, leaf := range wantHostLeaves {
		t.Run(leaf, func(t *testing.T) {
			t.Parallel()

			root := &cobra.Command{Use: "donmai"}
			root.AddCommand(NewHostCmd(testDataSource(), testHostConfig()))

			found, _, err := root.Find([]string{"host", leaf})
			if err != nil {
				t.Fatalf("Find(host %s): %v", leaf, err)
			}
			if found.Name() != leaf {
				t.Fatalf("Find(host %s) resolved to %q", leaf, found.Name())
			}
			if got := found.CommandPath(); got != "donmai host "+leaf {
				t.Errorf("CommandPath() = %q, want %q", got, "donmai host "+leaf)
			}
			// Every leaf must do something: either run, or parent further
			// subcommands (the four nested families).
			runnable := found.RunE != nil || found.Run != nil
			if !runnable && !containsString(nested, leaf) {
				t.Errorf("host %s has neither Run/RunE nor subcommands", leaf)
			}
			if containsString(nested, leaf) && len(found.Commands()) == 0 {
				t.Errorf("host %s must carry subcommands", leaf)
			}
		})
	}
}

// TestDaemonIsHiddenDeprecatedAliasOfHost is the D2/D5 acceptance: `daemon`
// survives, hidden, and its Deprecated string names a concrete removal version
// rather than "the next release".
func TestDaemonIsHiddenDeprecatedAliasOfHost(t *testing.T) {
	t.Parallel()

	cmd := newDaemonCmd(testHostConfig())

	if !cmd.Hidden {
		t.Error("daemon must be hidden from the top-level help")
	}
	if cmd.Deprecated == "" {
		t.Fatal("daemon must carry a Deprecated string")
	}
	if !strings.Contains(cmd.Deprecated, hostAliasRemovalVersion) {
		t.Errorf("Deprecated %q must name the removal version %q",
			cmd.Deprecated, hostAliasRemovalVersion)
	}
	// D5.4: a version, not a promise.
	for _, banned := range []string{"next release", "a future release", "soon"} {
		if strings.Contains(strings.ToLower(cmd.Deprecated), banned) {
			t.Errorf("Deprecated %q must not say %q — name a version", cmd.Deprecated, banned)
		}
	}
	if !strings.HasPrefix(hostAliasRemovalVersion, "v") {
		t.Errorf("removal version %q must be a concrete vX.Y.Z tag", hostAliasRemovalVersion)
	}
	if !strings.Contains(cmd.Deprecated, "host") {
		t.Errorf("Deprecated %q must point at `host`", cmd.Deprecated)
	}
}

// TestDaemonAliasKeepsEveryLifecycleSubcommand asserts no existing
// `donmai daemon <verb>` invocation lost its subcommand in the move.
func TestDaemonAliasKeepsEveryLifecycleSubcommand(t *testing.T) {
	t.Parallel()

	root := &cobra.Command{Use: "donmai"}
	root.AddCommand(newDaemonCmd(testHostConfig()))

	got := subcommandNames(root.Commands()[0])
	want := append([]string(nil), wantDaemonAliasLeaves...)
	sort.Strings(want)
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("daemon alias leaf set drifted.\n got: %v\nwant: %v", got, want)
	}

	for _, leaf := range wantDaemonAliasLeaves {
		found, _, err := root.Find([]string{"daemon", leaf})
		if err != nil {
			t.Errorf("Find(daemon %s): %v", leaf, err)
			continue
		}
		if found.Name() != leaf {
			t.Errorf("Find(daemon %s) resolved to %q", leaf, found.Name())
		}
	}

	// Every lifecycle leaf under the alias also exists under `host`.
	for _, leaf := range wantDaemonAliasLeaves {
		if !containsString(wantHostLeaves, leaf) {
			t.Errorf("daemon leaf %q has no `host` equivalent", leaf)
		}
	}
}

// TestDaemonAliasBehavesLikeHost runs the same subcommand through both nouns
// against the same mock and asserts identical stdout. The alias is only an
// alias if the output matches.
func TestDaemonAliasBehavesLikeHost(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		args []string
	}{
		{"status", []string{"status"}},
		{"status_json", []string{"status", "--json"}},
		{"stats", []string{"stats"}},
		{"pause", []string{"pause"}},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			hostOut := runNounCmd(t, "host", tc.args)
			aliasOut, aliasErr := runAliasCmd(t, tc.args)

			if aliasOut != hostOut {
				t.Errorf("stdout differs between nouns.\nhost:  %q\nalias: %q", hostOut, aliasOut)
			}
			// The notice belongs on stderr, so `daemon status --json | jq`
			// keeps working while the operator still sees the warning.
			if !strings.Contains(aliasErr, "deprecated") {
				t.Errorf("alias stderr missing deprecation notice, got %q", aliasErr)
			}
			if !strings.Contains(aliasErr, hostAliasRemovalVersion) {
				t.Errorf("alias stderr must name %q, got %q", hostAliasRemovalVersion, aliasErr)
			}
			wantPath := "donmai host " + tc.args[0]
			if !strings.Contains(aliasErr, wantPath) {
				t.Errorf("alias stderr must name the replacement %q, got %q", wantPath, aliasErr)
			}
		})
	}
}

// TestRegisterCommandsWiresHostAndAliases asserts the standalone binary's tree:
// `host` visible, `daemon` and `fleet-watch` present but hidden and deprecated.
func TestRegisterCommandsWiresHostAndAliases(t *testing.T) {
	t.Parallel()

	root := &cobra.Command{Use: "donmai"}
	RegisterCommands(root, testHostConfig())

	byName := map[string]*cobra.Command{}
	for _, c := range root.Commands() {
		byName[c.Name()] = c
	}

	tests := []struct {
		name          string
		wantHidden    bool
		wantDeprecate bool
	}{
		{"host", false, false},
		{"daemon", true, true},
		{"fleet-watch", true, true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cmd, ok := byName[tc.name]
			if !ok {
				t.Fatalf("RegisterCommands did not wire %q", tc.name)
			}
			if cmd.Hidden != tc.wantHidden {
				t.Errorf("Hidden = %v, want %v", cmd.Hidden, tc.wantHidden)
			}
			if (cmd.Deprecated != "") != tc.wantDeprecate {
				t.Errorf("Deprecated = %q, want deprecated=%v", cmd.Deprecated, tc.wantDeprecate)
			}
			if tc.wantDeprecate && !strings.Contains(cmd.Deprecated, hostAliasRemovalVersion) {
				t.Errorf("Deprecated %q must name %q", cmd.Deprecated, hostAliasRemovalVersion)
			}
		})
	}
}

// TestCodeHostIsUnaffectedByTopLevelHost guards the one real name collision
// risk: `code host` is the code-intelligence warm host, a different concept from
// the new top-level `host`. Both must resolve, independently.
func TestCodeHostIsUnaffectedByTopLevelHost(t *testing.T) {
	t.Parallel()

	root := &cobra.Command{Use: "donmai"}
	RegisterCommands(root, testHostConfig())

	codeHost, _, err := root.Find([]string{"code", "host"})
	if err != nil {
		t.Fatalf("Find(code host): %v", err)
	}
	if got := codeHost.CommandPath(); got != "donmai code host" {
		t.Fatalf("CommandPath() = %q, want donmai code host", got)
	}
	if codeHost.RunE == nil {
		t.Error("code host lost its RunE")
	}

	topHost, _, err := root.Find([]string{"host"})
	if err != nil {
		t.Fatalf("Find(host): %v", err)
	}
	if topHost == codeHost {
		t.Fatal("top-level host and code host resolved to the same command")
	}
	// `code host` must not have acquired the daemon-lifecycle leaves, and the
	// top-level `host` must not have acquired the warm-host flags.
	if hasSubcommand(codeHost, "install") {
		t.Error("code host must not carry the daemon lifecycle")
	}
	if topHost.Flags().Lookup("binding-file") != nil {
		t.Error("top-level host must not carry code-host flags")
	}
}

// ── helpers ───────────────────────────────────────────────────────────────────

func containsString(haystack []string, needle string) bool {
	for _, s := range haystack {
		if s == needle {
			return true
		}
	}
	return false
}

// runNounCmd executes args under the given noun with a mock daemon and returns
// stdout.
func runNounCmd(t *testing.T, noun string, args []string) string {
	t.Helper()

	mock := &mockDaemon{
		statusResp: fixtureStatusResp(),
		statsResp:  fixtureStatsResp(),
		actionResp: &afclient.DaemonActionResponse{OK: true, Message: "ok"},
	}
	factory := func(_ afclient.DaemonConfig) daemonDoer { return mock }

	var cmd *cobra.Command
	switch noun {
	case "host":
		cmd = newHostCmdWithFactory(factory, testDataSource(), testHostConfig())
	default:
		cmd = newDaemonCmdWithFactory(factory, testHostConfig())
	}

	out, errBuf := &bytes.Buffer{}, &bytes.Buffer{}
	cmd.SetOut(out)
	cmd.SetErr(errBuf)
	cmd.SetArgs(args)
	if err := cmd.Execute(); err != nil {
		t.Fatalf("%s %v: %v", noun, args, err)
	}
	return out.String()
}

// runAliasCmd executes args under the `daemon` alias and returns (stdout, stderr)
// separately, so the test can prove the notice never lands on stdout.
func runAliasCmd(t *testing.T, args []string) (string, string) {
	t.Helper()

	mock := &mockDaemon{
		statusResp: fixtureStatusResp(),
		statsResp:  fixtureStatsResp(),
		actionResp: &afclient.DaemonActionResponse{OK: true, Message: "ok"},
	}
	factory := func(_ afclient.DaemonConfig) daemonDoer { return mock }
	cmd := newDaemonCmdWithFactory(factory, testHostConfig())

	// Root the alias so CommandPath() renders the binary name the notice quotes.
	root := &cobra.Command{Use: "donmai"}
	root.AddCommand(cmd)

	out, errBuf := &bytes.Buffer{}, &bytes.Buffer{}
	root.SetOut(out)
	root.SetErr(errBuf)
	root.SetArgs(append([]string{"daemon"}, args...))
	if err := root.Execute(); err != nil {
		t.Fatalf("daemon %v: %v", args, err)
	}
	return out.String(), errBuf.String()
}
