package codex

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"
)

const (
	codexMCPStatusMethod       = "mcpServerStatus/list"
	codexMCPStatusPageLimit    = 100
	codexMCPActivationPollWait = 50 * time.Millisecond
	codexMCPDiagnosticNameMax  = 8
	codexMCPDiagnosticRuneMax  = 64
)

type mcpServerInventory struct {
	present     map[string]struct{}
	initialized map[string]struct{}
}

type mcpActivationDeadlineError struct {
	ready         []string
	uninitialized []string
	absent        []string
	unexpected    int
}

func (e *mcpActivationDeadlineError) Error() string {
	return fmt.Sprintf(
		"isolated config activation deadline exceeded; ready=%s; uninitialized=%s; absent=%s; unexpected=%d",
		formatMCPDiagnosticNames(e.ready),
		formatMCPDiagnosticNames(e.uninitialized),
		formatMCPDiagnosticNames(e.absent),
		e.unexpected,
	)
}

func (e *mcpActivationDeadlineError) Unwrap() error { return context.DeadlineExceeded }

// waitForMCPActivation closes the asynchronous gap between config/batchWrite
// and thread/start. Codex reloads user config in the background; config/read
// can confirm the new file contents before the corresponding MCP clients have
// initialized. mcpServerStatus/list supplies the initialize metadata needed to
// prove the requested session surface is active and that servers retired from
// the preceding Provider-managed set are no longer present. Codex-owned ambient
// entries are outside the isolated config boundary and are ignored.
func (p *Provider) waitForMCPActivation(ctx context.Context, desired map[string]any, retired map[string]struct{}) error {
	want := make(map[string]struct{}, len(desired))
	for name := range desired {
		want[name] = struct{}{}
	}

	activationCtx, cancel := context.WithTimeout(ctx, p.opts.RPCTimeout)
	defer cancel()
	var last mcpServerInventory
	haveInventory := false
	for {
		inventory, err := p.listMCPServerInventory(activationCtx)
		if err != nil {
			if errors.Is(activationCtx.Err(), context.DeadlineExceeded) && haveInventory {
				return newMCPActivationDeadlineError(want, last)
			}
			return err
		}
		last = inventory
		haveInventory = true
		if inventory.matches(want, retired) {
			return nil
		}

		timer := time.NewTimer(codexMCPActivationPollWait)
		select {
		case <-activationCtx.Done():
			timer.Stop()
			if errors.Is(activationCtx.Err(), context.DeadlineExceeded) {
				return newMCPActivationDeadlineError(want, last)
			}
			return activationCtx.Err()
		case <-timer.C:
		}
	}
}

func (p *Provider) listActiveMCPServers(ctx context.Context) (map[string]struct{}, error) {
	inventory, err := p.listMCPServerInventory(ctx)
	if err != nil {
		return nil, err
	}
	return inventory.initialized, nil
}

func (p *Provider) listMCPServerInventory(ctx context.Context) (mcpServerInventory, error) {
	inventory := mcpServerInventory{
		present:     map[string]struct{}{},
		initialized: map[string]struct{}{},
	}
	cursor := ""
	seenCursors := map[string]struct{}{}
	for {
		params := map[string]any{
			// Full is required because the reduced tools/auth view may omit
			// serverInfo, the initialize-handshake readiness proof.
			"detail": "full",
			"limit":  codexMCPStatusPageLimit,
		}
		if cursor != "" {
			params["cursor"] = cursor
		}
		raw, err := p.client.RequestWithRetry(ctx, codexMCPStatusMethod, params, p.opts.RPCTimeout)
		if err != nil {
			return mcpServerInventory{}, err
		}
		var response struct {
			Data []struct {
				Name       string    `json:"name"`
				ServerInfo *struct{} `json:"serverInfo"`
			} `json:"data"`
			NextCursor *string `json:"nextCursor"`
		}
		if err := json.Unmarshal(raw, &response); err != nil || response.Data == nil {
			return mcpServerInventory{}, errors.New("mcpServerStatus/list returned an invalid inventory")
		}
		for _, server := range response.Data {
			if server.Name == "" {
				return mcpServerInventory{}, errors.New("mcpServerStatus/list returned an invalid server name")
			}
			inventory.present[server.Name] = struct{}{}
			// Configured servers can appear in the status inventory while they
			// are still starting or after startup failed. serverInfo is the
			// metadata advertised by a completed MCP initialize handshake, and
			// therefore also works as the readiness signal for resource-only
			// servers whose tool inventory is legitimately empty.
			if server.ServerInfo == nil {
				continue
			}
			inventory.initialized[server.Name] = struct{}{}
		}
		if response.NextCursor == nil || *response.NextCursor == "" {
			return inventory, nil
		}
		cursor = *response.NextCursor
		if _, duplicate := seenCursors[cursor]; duplicate {
			return mcpServerInventory{}, errors.New("mcpServerStatus/list returned a repeated cursor")
		}
		seenCursors[cursor] = struct{}{}
	}
}

func (i mcpServerInventory) matches(want, retired map[string]struct{}) bool {
	for name := range want {
		if _, initialized := i.initialized[name]; !initialized {
			return false
		}
	}
	for name := range retired {
		if _, present := i.present[name]; present {
			return false
		}
	}
	return true
}

func newMCPActivationDeadlineError(want map[string]struct{}, inventory mcpServerInventory) error {
	diagnostic := &mcpActivationDeadlineError{}
	for name := range want {
		if _, ok := inventory.initialized[name]; ok {
			diagnostic.ready = append(diagnostic.ready, name)
			continue
		}
		if _, ok := inventory.present[name]; ok {
			diagnostic.uninitialized = append(diagnostic.uninitialized, name)
			continue
		}
		diagnostic.absent = append(diagnostic.absent, name)
	}
	for name := range inventory.present {
		if _, ok := want[name]; !ok {
			diagnostic.unexpected++
		}
	}
	return diagnostic
}

func mcpServerNames(config map[string]any) map[string]struct{} {
	names := make(map[string]struct{}, len(config))
	for name := range config {
		names[name] = struct{}{}
	}
	return names
}

func retiredMCPServerNames(previous map[string]struct{}, desired map[string]any) map[string]struct{} {
	retired := map[string]struct{}{}
	for name := range previous {
		if _, remains := desired[name]; !remains {
			retired[name] = struct{}{}
		}
	}
	return retired
}

func formatMCPDiagnosticNames(names []string) string {
	sorted := append([]string(nil), names...)
	sort.Strings(sorted)
	limit := len(sorted)
	if limit > codexMCPDiagnosticNameMax {
		limit = codexMCPDiagnosticNameMax
	}
	quoted := make([]string, 0, limit+1)
	for _, name := range sorted[:limit] {
		runes := []rune(name)
		if len(runes) > codexMCPDiagnosticRuneMax {
			name = string(runes[:codexMCPDiagnosticRuneMax]) + "..."
		}
		quoted = append(quoted, strconv.QuoteToASCII(name))
	}
	if omitted := len(sorted) - limit; omitted > 0 {
		quoted = append(quoted, strconv.Quote(fmt.Sprintf("...(+%d)", omitted)))
	}
	return "[" + strings.Join(quoted, ",") + "]"
}

func sameMCPServerNames(got, want map[string]struct{}) bool {
	if len(got) != len(want) {
		return false
	}
	for name := range want {
		if _, ok := got[name]; !ok {
			return false
		}
	}
	return true
}
