//go:build codex_integration

// Package codex integration tests run against a real `codex
// app-server` subprocess. They are gated behind the build tag
// `codex_integration` so the default `go test ./...` run never tries
// to spawn codex (it requires network access + a configured OpenAI
// key).
//
// To run: `go test -tags codex_integration -timeout 120s ./provider/codex/`.
//
// Pre-requisites:
//   - `codex` on PATH (see https://developers.openai.com/codex/)
//   - OPENAI_API_KEY (or whatever auth codex requires) configured
//   - network access
//
// The test does the bare minimum lifecycle smoke: spawn a session
// against a `read-only` sandbox with a trivial prompt, verify we get
// at least one InitEvent, then Stop the session and Shutdown the
// Provider.

package codex

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/RenseiAI/donmai/agent"
	"github.com/RenseiAI/donmai/attachwire"
)

func TestIntegration_RealCodexPlatformMCPAndEnvironmentAuthIsolation(t *testing.T) {
	binary, err := exec.LookPath("codex")
	if err != nil {
		t.Fatalf("real effective-config proof requires codex on PATH: %v", err)
	}
	for _, key := range codexEnvironmentAuthKeys {
		t.Setenv(key, "")
	}
	hostHome := t.TempDir()
	project := t.TempDir()
	boundaryParent := t.TempDir()
	t.Setenv("CODEX_HOME", hostHome)
	if err := os.WriteFile(
		filepath.Join(hostHome, "config.toml"),
		[]byte("cli_auth_credentials_store = \"keyring\"\n[mcp_servers.user_poison]\ncommand = \"/usr/bin/false\"\n"),
		0o600,
	); err != nil {
		t.Fatal(err)
	}
	for _, dir := range []string{filepath.Join(project, ".git"), filepath.Join(project, ".codex")} {
		if err := os.Mkdir(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(
		filepath.Join(project, ".codex", "config.toml"),
		[]byte("[mcp_servers.project_poison]\ncommand = \"/usr/bin/false\"\n"),
		0o600,
	); err != nil {
		t.Fatal(err)
	}

	spec := agent.Spec{
		Cwd: project,
		Env: map[string]string{"OPENAI_API_KEY": "integration-fixture"},
		MCPServers: []agent.MCPServerConfig{{
			Name: "donmai-platform",
			Type: "http",
			URL:  "https://platform.example/api/mcp/session",
			Headers: map[string]string{
				"Authorization": "Bearer session-fixture",
			},
		}},
	}
	launch, err := buildInteractiveLaunch(spec)
	if err != nil {
		t.Fatal(err)
	}
	boundary, auth, err := newInteractiveCodexConfigBoundary(boundaryParent, launch.env)
	if err != nil {
		t.Fatalf("private boundary: %v", err)
	}
	t.Cleanup(func() { _ = boundary.remove() })
	launch.env["CODEX_HOME"] = boundary.home
	if err := seedInteractiveCodexEnvironmentAuth(t.Context(), binary, boundary.home, auth); err != nil {
		t.Fatalf("seed real environment auth: %v", err)
	}

	if err := verifyExclusiveInteractiveMCP(
		t.Context(),
		runCodexMCPInventory,
		binary,
		spec,
		launch,
		boundary.home,
	); err != nil {
		t.Fatalf("real Codex effective MCP inventory: %v", err)
	}

	configArgs, err := interactiveConfigArgs(launch.argv)
	if err != nil {
		t.Fatal(err)
	}
	doctorArgs := append(configArgs, "doctor", "--json")
	cmd := exec.CommandContext(t.Context(), binary, doctorArgs...)
	cmd.Dir = project
	cmd.Env = mergeEnv(nil, launch.env, boundary.home)
	doctorBody, err := cmd.Output()
	if err != nil {
		t.Fatalf("real Codex startup/auth diagnostic: %v", err)
	}
	var doctor struct {
		Checks map[string]struct {
			Status  string `json:"status"`
			Summary string `json:"summary"`
		} `json:"checks"`
	}
	if err := json.Unmarshal(doctorBody, &doctor); err != nil {
		t.Fatalf("decode real Codex doctor output: %v", err)
	}
	foundSeededAuth := false
	if check, ok := doctor.Checks["auth.credentials"]; ok && check.Status == "ok" && strings.Contains(check.Summary, "configured") {
		foundSeededAuth = true
	}
	if !foundSeededAuth {
		t.Fatalf("real Codex did not start from seeded environment auth: %+v", doctor.Checks)
	}
	if _, err := os.Stat(filepath.Join(boundary.home, codexAuthFileName)); err != nil {
		t.Fatalf("environment auth was not seeded into the private store: %v", err)
	}
}

func TestIntegration_RealCodexFileAuthProjectionStarts(t *testing.T) {
	binary, err := exec.LookPath("codex")
	if err != nil {
		t.Fatalf("real file-auth proof requires codex on PATH: %v", err)
	}
	for _, key := range codexEnvironmentAuthKeys {
		t.Setenv(key, "")
	}
	hostAuthFile, err := resolveHostSessionAuthFile()
	if err != nil {
		t.Fatal(err)
	}
	hostInfo, err := os.Stat(hostAuthFile)
	if err != nil {
		t.Fatalf("real file-auth proof requires a host auth.json: %v", err)
	}
	boundary, _, err := newInteractiveCodexConfigBoundary("", nil)
	if err != nil {
		t.Fatalf("project real file auth: %v", err)
	}
	t.Cleanup(func() { _ = boundary.remove() })
	linkedInfo, err := os.Stat(boundary.authPath)
	if err != nil || !os.SameFile(hostInfo, linkedInfo) {
		t.Fatalf("isolated auth is not the host auth inode: err=%v", err)
	}

	cmd := exec.CommandContext(t.Context(), binary, "doctor", "--json")
	cmd.Dir = t.TempDir()
	cmd.Env = mergeEnv(nil, nil, boundary.home)
	doctorBody, err := cmd.Output()
	if err != nil {
		t.Fatalf("real Codex file-auth startup diagnostic: %v", err)
	}
	var doctor struct {
		Checks map[string]struct {
			Status string `json:"status"`
		} `json:"checks"`
	}
	if err := json.Unmarshal(doctorBody, &doctor); err != nil {
		t.Fatalf("decode real Codex doctor output: %v", err)
	}
	if doctor.Checks["auth.credentials"].Status != "ok" {
		t.Fatalf("real Codex did not accept projected file auth: %+v", doctor.Checks["auth.credentials"])
	}
}

func TestIntegration_RealCodexPlatformPTYStartsWithoutProjectTrustReview(t *testing.T) {
	binary, err := exec.LookPath("codex")
	if err != nil {
		t.Fatalf("real PTY startup proof requires codex on PATH: %v", err)
	}
	for _, key := range codexEnvironmentAuthKeys {
		t.Setenv(key, "")
	}
	hostHome := t.TempDir()
	project := t.TempDir()
	t.Setenv("CODEX_HOME", hostHome)
	if err := os.WriteFile(
		filepath.Join(hostHome, "config.toml"),
		[]byte("cli_auth_credentials_store = \"keyring\"\n[mcp_servers.user_poison]\ncommand = \"/usr/bin/false\"\n"),
		0o600,
	); err != nil {
		t.Fatal(err)
	}
	for _, dir := range []string{filepath.Join(project, ".git"), filepath.Join(project, ".codex")} {
		if err := os.Mkdir(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(
		filepath.Join(project, ".codex", "config.toml"),
		[]byte("[mcp_servers.project_poison]\ncommand = \"/usr/bin/false\"\n"),
		0o600,
	); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()
	h, err := SpawnInteractive(ctx, Options{CodexBin: binary}, agent.Spec{
		Cwd: project,
		Env: map[string]string{"OPENAI_API_KEY": "integration-fixture"},
		MCPServers: []agent.MCPServerConfig{{
			Name: "donmai-platform", Type: "http", URL: "http://127.0.0.1:1/api/mcp/session",
		}},
		Interactive: &agent.InteractiveSpec{Cols: 100, Rows: 30},
	})
	if err != nil {
		t.Fatalf("real platform PTY spawn: %v", err)
	}
	t.Cleanup(func() { _ = h.Stop(context.Background()) })
	interactive, ok := h.(agent.InteractiveCapable)
	if !ok {
		t.Fatal("real platform PTY handle is not interactive")
	}

	deadline := time.Now().Add(8 * time.Second)
	var screenText string
	for time.Now().Before(deadline) {
		screen, _, snapshotErr := interactive.InteractiveSession().Snapshot()
		if snapshotErr != nil {
			t.Fatalf("real PTY snapshot: %v", snapshotErr)
		}
		var rendered strings.Builder
		for _, cell := range append(append([]attachwire.Cell(nil), screen.Primary...), screen.Alt...) {
			rendered.Write(cell.RuneBytes)
		}
		for _, row := range screen.Scrollback {
			for _, cell := range row {
				rendered.Write(cell.RuneBytes)
			}
		}
		screenText = rendered.String()
		if strings.Contains(screenText, "OpenAI Codex") {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if !strings.Contains(screenText, "OpenAI Codex") {
		t.Fatalf("real Codex TUI did not reach its startup screen: %q", screenText)
	}
	if strings.Contains(strings.ToLower(screenText), "trust the contents") {
		t.Fatalf("real Codex TUI parked on project trust review: %q", screenText)
	}
}

func TestIntegration_RealCodexRepositoryAuthorityNegativeAttempts(t *testing.T) {
	if os.Getenv("DONMAI_CODEX_WORKAREA_AUTHORITY_INTEGRATION") != "1" {
		t.Fatal("real executor authority proof is mandatory; set DONMAI_CODEX_WORKAREA_AUTHORITY_INTEGRATION=1")
	}
	if _, err := exec.LookPath("codex"); err != nil {
		t.Fatalf("real executor authority proof requires codex on PATH: %v", err)
	}
	cacheDir, err := os.UserCacheDir()
	if err != nil {
		t.Fatalf("UserCacheDir: %v", err)
	}
	root, err := os.MkdirTemp(cacheDir, "donmai-workarea-authority-")
	if err != nil {
		t.Fatalf("MkdirTemp authority root: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	mutable := filepath.Join(root, "mutable")
	readOnly := filepath.Join(root, "context")
	if err := os.MkdirAll(mutable, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(readOnly, 0o700); err != nil {
		t.Fatal(err)
	}
	sentinel := filepath.Join(readOnly, "sentinel")
	if err := os.WriteFile(sentinel, []byte("locked"), 0o600); err != nil {
		t.Fatal(err)
	}

	remount := "mount -u -w " + strconv.Quote(readOnly)
	if runtime.GOOS == "linux" {
		remount = "mount -o remount,rw " + strconv.Quote(readOnly)
	}
	script := fmt.Sprintf(`set +e
printf mutable-control > %[1]s/control
(printf forbidden > %[2]s/write-attempt); printf '%%s' "$?" > %[1]s/write.status
(mv %[2]s/sentinel %[2]s/renamed); printf '%%s' "$?" > %[1]s/rename.status
(rm -f %[2]s/sentinel); printf '%%s' "$?" > %[1]s/remove.status
(chmod 0777 %[2]s/sentinel); printf '%%s' "$?" > %[1]s/chmod.status
(%[3]s); printf '%%s' "$?" > %[1]s/remount.status
exit 0`, strconv.Quote(mutable), strconv.Quote(readOnly), remount)

	provider, err := New(Options{Cwd: mutable, HandshakeTimeout: 30 * time.Second, HostSessionAuth: true})
	if err != nil {
		t.Fatalf("New host-session provider: %v", err)
	}
	t.Cleanup(func() { _ = provider.Shutdown(context.Background()) })
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	handle, err := provider.Spawn(ctx, agent.Spec{
		Prompt: "Run exactly this shell script once with the shell tool, do not alter it, do not retry any failed operation, then report done:\n\n" + script,
		Cwd:    mutable, Autonomous: true, SandboxEnabled: true, SandboxLevel: agent.SandboxWorkspaceWrite,
		Model: "gpt-5.6-terra", Effort: agent.EffortLow,
		RepositoryAuthority: &agent.RepositoryAuthorityPolicy{
			Protocol: "session-root-v1", WorkareaRoot: root, SelectedPath: mutable,
			MutablePaths: []string{mutable}, ReadOnlyPaths: []string{readOnly}, Enforcement: "isolated-read-only-v1",
		},
	})
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	defer func() { _ = handle.Stop(context.Background()) }()
	var terminalProviderError *agent.ErrorEvent
	for {
		select {
		case event, ok := <-handle.Events():
			if !ok {
				t.Fatal("events closed before result")
			}
			switch value := event.(type) {
			case agent.ResultEvent:
				if !value.Success {
					t.Fatalf("executor result failed: %+v", value)
				}
				goto verify
			case agent.ErrorEvent:
				if value.Code != "mcp_cleanup_failed" {
					t.Fatalf("executor error: %+v", value)
				}
				terminalProviderError = &value
				goto verify
			}
		case <-ctx.Done():
			t.Fatalf("authority proof timed out: %v", ctx.Err())
		}
	}

verify:
	if body, err := os.ReadFile(filepath.Join(mutable, "control")); err != nil || string(body) != "mutable-control" {
		t.Fatalf("mutable control = %q, %v", body, err)
	}
	for _, operation := range []string{"write", "rename", "remove", "chmod", "remount"} {
		body, err := os.ReadFile(filepath.Join(mutable, operation+".status"))
		if err != nil {
			t.Fatalf("%s status: %v", operation, err)
		}
		if strings.TrimSpace(string(body)) == "0" {
			t.Fatalf("%s attempt unexpectedly succeeded", operation)
		}
	}
	if body, err := os.ReadFile(sentinel); err != nil || string(body) != "locked" {
		t.Fatalf("read-only sentinel changed: %q, %v", body, err)
	}
	if _, err := os.Stat(filepath.Join(readOnly, "write-attempt")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("write-attempt exists or stat failed unexpectedly: %v", err)
	}
	if _, err := os.Stat(filepath.Join(readOnly, "renamed")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("renamed target exists or stat failed unexpectedly: %v", err)
	}
	info, err := os.Stat(sentinel)
	if err != nil {
		t.Fatalf("stat sentinel mode: %v", err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("sentinel mode = %v; want 0600", info.Mode())
	}
	if terminalProviderError != nil {
		t.Fatalf("authority attempts were enforced, but provider cleanup failed: %+v", *terminalProviderError)
	}
	_ = handle.Stop(context.Background())
	selectedScript := fmt.Sprintf(`set +e
printf selected-read-only-control > %[1]s/selected-control
(printf forbidden > %[2]s/selected-write-attempt); printf '%%s' "$?" > %[1]s/selected-write.status
(mv %[2]s/sentinel %[2]s/selected-renamed); printf '%%s' "$?" > %[1]s/selected-rename.status
(rm -f %[2]s/sentinel); printf '%%s' "$?" > %[1]s/selected-remove.status
(chmod 0777 %[2]s/sentinel); printf '%%s' "$?" > %[1]s/selected-chmod.status
(%[3]s); printf '%%s' "$?" > %[1]s/selected-remount.status
exit 0`, strconv.Quote(mutable), strconv.Quote(readOnly), remount)
	selectedHandle, err := provider.Spawn(ctx, agent.Spec{
		Prompt: "Run exactly this shell script once with the shell tool, do not alter it, do not retry any failed operation, then report done:\n\n" + selectedScript,
		Cwd:    readOnly, Autonomous: true, SandboxEnabled: true, SandboxLevel: agent.SandboxWorkspaceWrite,
		Model: "gpt-5.6-terra", Effort: agent.EffortLow,
		RepositoryAuthority: &agent.RepositoryAuthorityPolicy{
			Protocol: "session-root-v1", WorkareaRoot: root, SelectedPath: readOnly,
			MutablePaths: []string{mutable}, ReadOnlyPaths: []string{readOnly}, Enforcement: "isolated-read-only-v1",
		},
	})
	if err != nil {
		t.Fatalf("Spawn selected read-only CWD: %v", err)
	}
	defer func() { _ = selectedHandle.Stop(context.Background()) }()
	selectedDone := false
	for !selectedDone {
		select {
		case event, ok := <-selectedHandle.Events():
			if !ok {
				t.Fatal("selected read-only events closed before result")
			}
			switch value := event.(type) {
			case agent.ResultEvent:
				if !value.Success {
					t.Fatalf("selected read-only result failed: %+v", value)
				}
				selectedDone = true
			case agent.ErrorEvent:
				t.Fatalf("selected read-only executor error: %+v", value)
			}
		case <-ctx.Done():
			t.Fatalf("selected read-only authority proof timed out: %v", ctx.Err())
		}
	}
	if body, err := os.ReadFile(filepath.Join(mutable, "selected-control")); err != nil || string(body) != "selected-read-only-control" {
		t.Fatalf("selected read-only mutable control = %q, %v", body, err)
	}
	selectedWrite, err := os.ReadFile(filepath.Join(mutable, "selected-write.status"))
	if err != nil || strings.TrimSpace(string(selectedWrite)) != "0" {
		t.Fatalf("selected read-only executor capability probe = %q, %v; want demonstrated unsafe write", selectedWrite, err)
	}
	if provider.Manifest().Caps.SupportsReadOnlySelectedCWD {
		t.Fatal("Codex advertised selected read-only CWD support despite the real unsafe write")
	}
}

func TestIntegration_RealCodexAppServer_SmokeLifecycle(t *testing.T) {
	if _, err := exec.LookPath("codex"); err != nil {
		t.Skip("codex binary not on PATH")
	}

	cwd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	p, err := New(Options{Cwd: cwd, HandshakeTimeout: 30 * time.Second})
	if err != nil {
		if errors.Is(err, agent.ErrProviderUnavailable) {
			t.Skipf("codex unavailable: %v", err)
		}
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = p.Shutdown(context.Background()) })

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	h, err := p.Spawn(ctx, agent.Spec{
		Prompt:         "say hello",
		Cwd:            cwd,
		Autonomous:     true,
		SandboxEnabled: true,
		SandboxLevel:   agent.SandboxReadOnly,
		MaxTurns:       intPtr(1),
		Effort:         agent.EffortLow,
	})
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	defer func() { _ = h.Stop(context.Background()) }()

	deadline := time.NewTimer(60 * time.Second)
	defer deadline.Stop()

	var sawInit bool
	for !sawInit {
		select {
		case ev, ok := <-h.Events():
			if !ok {
				if !sawInit {
					t.Fatalf("events channel closed before InitEvent")
				}
				return
			}
			if ev.Kind() == agent.EventInit {
				sawInit = true
			}
		case <-deadline.C:
			t.Fatalf("timed out waiting for InitEvent")
		}
	}
}

func TestIntegration_RealCodexAppServer_HostSessionAuth(t *testing.T) {
	if os.Getenv("DONMAI_CODEX_HOST_SESSION_INTEGRATION") != "1" {
		t.Skip("set DONMAI_CODEX_HOST_SESSION_INTEGRATION=1 to spend a live host-session turn")
	}
	if _, err := exec.LookPath("codex"); err != nil {
		t.Skip("codex binary not on PATH")
	}

	cwd := t.TempDir()
	p, err := New(Options{
		Cwd:              cwd,
		HandshakeTimeout: 30 * time.Second,
		HostSessionAuth:  true,
	})
	if err != nil {
		t.Fatalf("New host-session provider: %v", err)
	}
	t.Cleanup(func() { _ = p.Shutdown(context.Background()) })

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	const marker = "HOST-SESSION-APP-SERVER-OK"
	h, err := p.Spawn(ctx, agent.Spec{
		Prompt:         "Reply with exactly " + marker + " and do not use tools.",
		Cwd:            cwd,
		Autonomous:     true,
		SandboxEnabled: true,
		SandboxLevel:   agent.SandboxReadOnly,
		Model:          "gpt-5.6-sol",
		Effort:         agent.EffortLow,
	})
	if err != nil {
		t.Fatalf("Spawn host-session turn: %v", err)
	}
	defer func() { _ = h.Stop(context.Background()) }()

	var transcript strings.Builder
	for {
		select {
		case ev, ok := <-h.Events():
			if !ok {
				t.Fatalf("events closed before a successful result; transcript=%q", transcript.String())
			}
			switch event := ev.(type) {
			case agent.AssistantTextEvent:
				transcript.WriteString(event.Text)
			case agent.ResultEvent:
				transcript.WriteString(event.Message)
				if !event.Success {
					t.Fatalf("host-session result failed: errors=%v subtype=%s", event.Errors, event.ErrorSubtype)
				}
				if !strings.Contains(transcript.String(), marker) {
					t.Fatalf("host-session transcript missing %s: %q", marker, transcript.String())
				}
				return
			case agent.ErrorEvent:
				t.Fatalf("host-session provider error: %s (%s)", event.Message, event.Code)
			}
		case <-ctx.Done():
			t.Fatalf("host-session turn timed out: %v; transcript=%q", ctx.Err(), transcript.String())
		}
	}
}

func TestIntegration_RealCodexAppServer_PreparedHarnessReadyPath(t *testing.T) {
	if _, err := exec.LookPath("codex"); err != nil {
		t.Skip("codex binary not on PATH")
	}
	cwd := t.TempDir()
	p, err := New(Options{Cwd: cwd, HandshakeTimeout: 30 * time.Second})
	if err != nil {
		t.Skipf("codex unavailable: %v", err)
	}
	t.Cleanup(func() { _ = p.Shutdown(context.Background()) })

	source := agent.Spec{
		PromptMode: agent.PromptModeAutonomous, Autonomous: true,
		SandboxEnabled: true, SandboxLevel: agent.SandboxReadOnly,
		PromptPlan: &agent.PromptPlan{
			ContractVersion:  agent.PromptContractVersion,
			BaseInstructions: agent.BaseInstructionPlan{Strategy: agent.BaseInstructionsPreserve},
			UserPrompt:       agent.PromptContent{ID: "prepared-ready-smoke", Text: "Reply only with ready.", Required: true},
		},
	}
	const operationalDigest = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	var materializations []agent.HarnessMaterialization
	for _, channel := range []string{"worktree", "environment", "credentials", "config", "endpoint_delivery", "services", "child_process", "runtime", "cleanup"} {
		materializations = append(materializations, agent.HarnessMaterialization{Channel: channel, SourceDigest: operationalDigest, Required: true})
	}
	plan, err := agent.CompilePreparedHarness(source, p.Manifest(), operationalDigest, nil, materializations)
	if err != nil {
		t.Fatalf("host CompilePreparedHarness: %v", err)
	}
	if err := agent.ValidatePreparedHarness(plan, operationalDigest); err != nil {
		t.Fatalf("host prepared plan: %v", err)
	}

	materialized := source
	materialized.Cwd = cwd
	materialized.PreparedHarness = plan
	materialized.OnPromptAdapted = func(agent.PromptDeliveryReceipt) error {
		return errors.New("provider attempted to mint a second prompt authority")
	}
	materialized.OnToolLifecycleAdapted = func(agent.ToolLifecycleReceipt) error {
		return errors.New("provider attempted to mint a second tool authority")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	h, err := p.Spawn(ctx, materialized)
	if err != nil {
		t.Fatalf("Spawn prepared ready path: %v", err)
	}
	if h == nil {
		t.Fatal("real Codex accepted the prepared ready path without returning a handle")
	}
	if err := h.Stop(context.Background()); err != nil {
		t.Fatalf("stop prepared ready handle: %v", err)
	}
}

func TestIntegration_RealCodexPTY_PreparedHumanReadyPath(t *testing.T) {
	if _, err := exec.LookPath("codex"); err != nil {
		t.Skip("codex binary not on PATH")
	}
	cwd := t.TempDir()
	p, err := New(Options{Cwd: cwd, HandshakeTimeout: 30 * time.Second})
	if err != nil {
		t.Skipf("codex unavailable: %v", err)
	}
	t.Cleanup(func() { _ = p.Shutdown(context.Background()) })
	source := agent.Spec{
		PromptMode:  agent.PromptModeHumanControlled,
		Interactive: &agent.InteractiveSpec{Cols: 80, Rows: 24},
		PromptPlan: &agent.PromptPlan{
			ContractVersion:  agent.PromptContractVersion,
			BaseInstructions: agent.BaseInstructionPlan{Strategy: agent.BaseInstructionsPreserve},
			UserPrompt:       agent.PromptContent{ID: "prepared-human-smoke", Text: "Reply only with ready.", Required: true},
		},
	}
	const operationalDigest = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	var materializations []agent.HarnessMaterialization
	for _, channel := range []string{"worktree", "environment", "credentials", "config", "endpoint_delivery", "services", "child_process", "runtime", "cleanup"} {
		materializations = append(materializations, agent.HarnessMaterialization{Channel: channel, SourceDigest: operationalDigest, Required: true})
	}
	prepared, err := agent.CompilePreparedHarness(source, p.Manifest(), operationalDigest, nil, materializations)
	if err != nil {
		t.Fatalf("host CompilePreparedHarness: %v", err)
	}
	materialized := source
	materialized.Cwd = cwd
	materialized.PreparedHarness = prepared
	materialized.OnPromptAdapted = func(agent.PromptDeliveryReceipt) error { return errors.New("second prompt authority") }
	materialized.OnToolLifecycleAdapted = func(agent.ToolLifecycleReceipt) error { return errors.New("second tool authority") }
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	h, err := p.Spawn(ctx, materialized)
	if err != nil {
		t.Fatalf("Spawn prepared human PTY path: %v", err)
	}
	if _, ok := h.(agent.InteractiveCapable); !ok {
		_ = h.Stop(context.Background())
		t.Fatal("prepared human path did not return an interactive handle")
	}
	if err := h.Stop(context.Background()); err != nil {
		t.Fatalf("stop prepared human PTY handle: %v", err)
	}
}

func intPtr(i int) *int { return &i }

func TestIntegration_RealCodexAppServer_PromptProvenance(t *testing.T) {
	if _, err := exec.LookPath("codex"); err != nil {
		t.Skip("codex binary not on PATH")
	}
	cwd := t.TempDir()
	p, err := New(Options{Cwd: cwd, HandshakeTimeout: 30 * time.Second})
	if err != nil {
		t.Skipf("codex unavailable: %v", err)
	}
	t.Cleanup(func() { _ = p.Shutdown(context.Background()) })

	plan := agent.PromptPlan{
		ContractVersion:  agent.PromptContractVersion,
		HarnessProtocol:  &agent.PromptContent{ID: "system", Text: "Remember system marker REN2040DXS3. Report it when asked. Do not use tools.", Required: true},
		BaseInstructions: agent.BaseInstructionPlan{Strategy: agent.BaseInstructionsPreserve},
		InitialContext:   []agent.PromptContent{{ID: "context", Text: "Initial-context marker REN2040DXC5.", Required: true}},
		UserPrompt:       agent.PromptContent{ID: "user", Text: "User-task marker REN2040DXU7. Reply with all four opaque markers and do not use tools.", Required: true},
		UserAmendments: []agent.UserPromptAmendment{{
			ID: "amendment", Position: agent.UserPromptAppend,
			Content: agent.PromptContent{ID: "amendment-content", Text: "Trailing amendment marker REN2040DXA9.", Required: true},
		}},
	}
	var receipt agent.PromptDeliveryReceipt
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	h, err := p.Spawn(ctx, agent.Spec{
		PromptPlan: &plan, Cwd: cwd, Autonomous: true, SandboxEnabled: true, SandboxLevel: agent.SandboxReadOnly,
		Model:           "gpt-5.6-terra",
		OnPromptAdapted: func(got agent.PromptDeliveryReceipt) error { receipt = got; return nil },
	})
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	defer func() { _ = h.Stop(context.Background()) }()

	var transcript strings.Builder
	for {
		select {
		case ev, ok := <-h.Events():
			if !ok {
				goto done
			}
			switch event := ev.(type) {
			case agent.AssistantTextEvent:
				transcript.WriteString(event.Text)
			case agent.ResultEvent:
				transcript.WriteString(event.Message)
				goto done
			case agent.ErrorEvent:
				t.Fatalf("provider error: %s", event.Message)
			}
		case <-ctx.Done():
			t.Fatalf("prompt provenance smoke timed out: %v", ctx.Err())
		}
	}

done:
	if receipt.Decision != "ready" || receipt.ProfileID != "codex/headless/app-server-v2" {
		t.Fatalf("receipt = %+v", receipt)
	}
	for _, marker := range []string{"REN2040DXS3", "REN2040DXC5", "REN2040DXU7", "REN2040DXA9"} {
		if !strings.Contains(transcript.String(), marker) {
			t.Fatalf("transcript missing %s: %q", marker, transcript.String())
		}
	}
	t.Logf("receipt=%+v transcript=%q", receipt, transcript.String())
}
