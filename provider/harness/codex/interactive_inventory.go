package codex

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"slices"
	"sort"
	"strings"

	"github.com/RenseiAI/donmai/agent"
)

// ErrInteractiveCodexMCPIsolation identifies an effective Codex configuration
// whose MCP surface differs from the exact server set the runner requested.
// The PTY is never started after this error.
var ErrInteractiveCodexMCPIsolation = errors.New("codex interactive MCP configuration is not exclusive")

type interactiveMCPInventoryRunner func(
	ctx context.Context,
	binary string,
	cwd string,
	env []string,
	configArgs []string,
	queryArgs []string,
) ([]byte, error)

type codexMCPInventoryEntry struct {
	Name           string                     `json:"name"`
	Enabled        bool                       `json:"enabled"`
	DisabledReason *string                    `json:"disabled_reason"`
	Transport      codexMCPInventoryTransport `json:"transport"`
	StartupTimeout *float64                   `json:"startup_timeout_sec"`
	ToolTimeout    *float64                   `json:"tool_timeout_sec"`
	EnabledTools   []string                   `json:"enabled_tools"`
	DisabledTools  []string                   `json:"disabled_tools"`
}

type codexMCPInventoryTransport struct {
	Type              string            `json:"type"`
	Command           string            `json:"command"`
	Args              []string          `json:"args"`
	Env               map[string]string `json:"env"`
	EnvVars           []string          `json:"env_vars"`
	Cwd               *string           `json:"cwd"`
	URL               string            `json:"url"`
	BearerTokenEnvVar *string           `json:"bearer_token_env_var"`
	HTTPHeaders       map[string]string `json:"http_headers"`
	EnvHTTPHeaders    map[string]string `json:"env_http_headers"`
	HTTPHeadersHelper *string           `json:"http_headers_helper"`
}

func runCodexMCPInventory(
	ctx context.Context,
	binary string,
	cwd string,
	env []string,
	configArgs []string,
	queryArgs []string,
) ([]byte, error) {
	args := append([]string(nil), configArgs...)
	args = append(args, queryArgs...)
	var stdout, stderr bytes.Buffer
	cmd := &exec.Cmd{
		Path:   binary,
		Args:   append([]string{binary}, args...),
		Dir:    cwd,
		Env:    env,
		Stdout: &stdout,
		Stderr: &stderr,
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start codex mcp inventory: %w", err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	var err error
	select {
	case err = <-done:
	case <-ctx.Done():
		_ = cmd.Process.Kill()
		<-done
		return nil, fmt.Errorf("codex mcp inventory: %w", ctx.Err())
	}
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			detail := strings.TrimSpace(stderr.String())
			if detail != "" {
				return nil, fmt.Errorf("codex mcp inventory exited unsuccessfully: %s", detail)
			}
		}
		return nil, fmt.Errorf("codex mcp inventory: %w", err)
	}
	return stdout.Bytes(), nil
}

func interactiveConfigArgs(argv []string) ([]string, error) {
	var out []string
	for i := 0; i < len(argv); i++ {
		if argv[i] == "--config" {
			if i+1 >= len(argv) {
				return nil, errors.New("interactive argv ends after --config")
			}
			out = append(out, "--config", argv[i+1])
			i++
		}
	}
	return out, nil
}

func verifyExclusiveInteractiveMCP(
	ctx context.Context,
	runner interactiveMCPInventoryRunner,
	binary string,
	spec agent.Spec,
	launch interactiveLaunch,
	ownedHome string,
) error {
	configArgs, err := interactiveConfigArgs(launch.argv)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrInteractiveCodexMCPIsolation, err)
	}
	if runner == nil {
		runner = runCodexMCPInventory
	}
	env := mergeEnv(nil, launch.env, ownedHome)
	body, err := runner(ctx, binary, spec.Cwd, env, configArgs, []string{"mcp", "list", "--json"})
	if err != nil {
		return fmt.Errorf("%w: effective-config readback failed: %v", ErrInteractiveCodexMCPIsolation, err)
	}
	var inventory []codexMCPInventoryEntry
	if err := json.Unmarshal(body, &inventory); err != nil {
		return fmt.Errorf("%w: decode effective-config readback: %v", ErrInteractiveCodexMCPIsolation, err)
	}
	if err := compareInteractiveMCPListNames(spec.MCPServers, inventory); err != nil {
		return fmt.Errorf("%w: %v", ErrInteractiveCodexMCPIsolation, err)
	}
	for _, server := range spec.MCPServers {
		body, err := runner(
			ctx,
			binary,
			spec.Cwd,
			env,
			configArgs,
			[]string{"mcp", "get", strings.TrimSpace(server.Name), "--json"},
		)
		if err != nil {
			return fmt.Errorf("%w: effective server readback for %q failed: %v", ErrInteractiveCodexMCPIsolation, server.Name, err)
		}
		entry, err := decodeStrictMCPInventoryEntry(body)
		if err != nil {
			return fmt.Errorf("%w: decode effective server %q: %v", ErrInteractiveCodexMCPIsolation, server.Name, err)
		}
		if entry.Name != strings.TrimSpace(server.Name) {
			return fmt.Errorf("%w: requested server %q read back as %q", ErrInteractiveCodexMCPIsolation, server.Name, entry.Name)
		}
		if err := compareInteractiveMCPEntry(server, entry); err != nil {
			return fmt.Errorf("%w: server %q: %v", ErrInteractiveCodexMCPIsolation, entry.Name, err)
		}
	}
	return nil
}

func compareInteractiveMCPListNames(want []agent.MCPServerConfig, got []codexMCPInventoryEntry) error {
	wantByName := make(map[string]agent.MCPServerConfig, len(want))
	for _, server := range want {
		wantByName[strings.TrimSpace(server.Name)] = server
	}
	if len(got) != len(wantByName) {
		return fmt.Errorf("effective MCP server count is %d, want %d", len(got), len(wantByName))
	}
	for _, entry := range got {
		if _, ok := wantByName[entry.Name]; !ok {
			return fmt.Errorf("effective MCP surface contains undeclared server %q", entry.Name)
		}
	}
	return nil
}

func decodeStrictMCPInventoryEntry(body []byte) (codexMCPInventoryEntry, error) {
	var entry codexMCPInventoryEntry
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&entry); err != nil {
		return codexMCPInventoryEntry{}, err
	}
	var trailing json.RawMessage
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return codexMCPInventoryEntry{}, errors.New("multiple JSON values in server readback")
	}
	return entry, nil
}

func compareInteractiveMCPEntry(want agent.MCPServerConfig, got codexMCPInventoryEntry) error {
	if !got.Enabled || got.DisabledReason != nil || got.StartupTimeout != nil || got.ToolTimeout != nil {
		return errors.New("effective status or timeout fields were widened by another config layer")
	}
	if len(got.EnabledTools) != 0 || len(got.DisabledTools) != 0 {
		return errors.New("effective tool filters were widened by another config layer")
	}
	t := got.Transport
	if t.Cwd != nil || len(t.Env) != 0 || t.BearerTokenEnvVar != nil || len(t.HTTPHeaders) != 0 || t.HTTPHeadersHelper != nil {
		return errors.New("effective transport contains undeclared fields from another config layer")
	}
	switch strings.ToLower(strings.TrimSpace(want.Type)) {
	case "", "stdio":
		wantEnvVars := sortedStringKeys(want.Env)
		gotEnvVars := append([]string(nil), t.EnvVars...)
		sort.Strings(gotEnvVars)
		if t.Type != "stdio" || t.Command != want.Command || !slices.Equal(t.Args, want.Args) || !slices.Equal(gotEnvVars, wantEnvVars) || t.URL != "" || len(t.EnvHTTPHeaders) != 0 {
			return errors.New("effective stdio transport differs from the requested server")
		}
	case "http":
		wantHeaders := make(map[string]string, len(want.Headers))
		for header := range want.Headers {
			wantHeaders[header] = codexHTTPHeaderEnvName(want.Name, header)
		}
		if t.Type != "streamable_http" || t.URL != want.URL || t.Command != "" || len(t.Args) != 0 || len(t.EnvVars) != 0 || !equalStringMap(t.EnvHTTPHeaders, wantHeaders) {
			return errors.New("effective HTTP transport differs from the requested server")
		}
	default:
		return fmt.Errorf("requested transport type %q is unsupported", want.Type)
	}
	return nil
}

func equalStringMap(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for key, value := range a {
		if b[key] != value {
			return false
		}
	}
	return true
}
