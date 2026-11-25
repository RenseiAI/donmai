// Package tempown records which process owns each temporary artefact donmai
// creates under the shared temp folder, and reclaims the ones whose owner is
// gone.
//
// Probes, harness sessions and the runner create short-lived directories and
// files under os.TempDir(). Each one is removed by its owner when the owner
// ends normally (a deferred RemoveAll, a session close). A crash, a SIGKILL or
// a daemon restart skips that cleanup, and on a long-lived host the leftovers
// accumulate in a folder the operating system may have to empty before the
// next boot can finish.
//
// The record is the artefact's own name. MkdirTemp and CreateTemp insert an
// owner tag, "p<pid>-", between a registered prefix and the random suffix:
//
//	donmai-claude-usage-p48213-3920481723
//
// The name is written in the same syscall that creates the artefact, so there
// is no window in which an artefact exists without its record. Sweep (see
// janitor.go) acts only on names that carry a registered prefix AND a valid
// owner tag; everything else in the temp folder, including shared tool caches
// and the untagged names older releases created, is never touched.
package tempown

import (
	"errors"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
)

// Kind is the filesystem shape a producer creates.
type Kind uint8

const (
	// KindDir is a directory created with MkdirTemp.
	KindDir Kind = iota + 1
	// KindFile is a regular file created with CreateTemp.
	KindFile
)

// MarshalText renders the kind as its name in JSON.
func (k Kind) MarshalText() ([]byte, error) { return []byte(k.String()), nil }

// String renders the kind for reports.
func (k Kind) String() string {
	switch k {
	case KindDir:
		return "dir"
	case KindFile:
		return "file"
	default:
		return "unknown"
	}
}

// Registered prefixes. Each call site creates its artefact through MkdirTemp
// or CreateTemp with one of these, so the creation and the janitor's fence
// share one literal.
const (
	PrefixClaudeLogin       = "donmai-claude-login-"
	PrefixClaudeUsage       = "donmai-claude-usage-"
	PrefixClaudeVersion     = "donmai-claude-version-"
	PrefixClaudeStopHook    = "donmai-stophook-"
	PrefixClaudeSkills      = "donmai-skills-"
	PrefixClaudeMCPConfig   = "donmai-claude-mcp-"
	PrefixActivityRelay     = "donmai-activity-relay-"
	PrefixSignalGuard       = "donmai-signal-guard-"
	PrefixMCPBearer         = "donmai-mcp-bearer-" //nolint:gosec // G101: a temp directory name prefix, not a credential.
	PrefixCheckpointIndex   = "provider-error-checkpoint-"
	PrefixRescueIndex       = "rescue-index-"
	PrefixRescueVerifyIndex = "rescue-verify-"
	PrefixMuseSession       = "muse-session-"
	PrefixMuseInteractive   = "muse-interactive-"
	PrefixMuseLogin         = "donmai-muse-login-"
	PrefixPiCatalogProbe    = "donmai-pi-catalog-probe-"
	PrefixOpenCodeConfig    = "donmai-opencode-config-"
	PrefixCodeSurvival      = "code-survival-"
)

// Producer describes one family of daemon-owned temp artefacts.
type Producer struct {
	// Prefix is the name prefix before the owner tag.
	Prefix string `json:"prefix"`
	// Suffix is a fixed name suffix after the random part (".json"), or "".
	Suffix string `json:"suffix,omitempty"`
	// Kind is the shape the producer creates.
	Kind Kind `json:"kind"`
	// Area names the component that creates it (claude, runner, muse, pi).
	Area string `json:"area"`
	// Label is a short human description for reports.
	Label string `json:"label"`
}

var producers = []Producer{
	{Prefix: PrefixClaudeLogin, Kind: KindDir, Area: "claude", Label: "Claude login probe"},
	{Prefix: PrefixClaudeUsage, Kind: KindDir, Area: "claude", Label: "Claude usage probe"},
	{Prefix: PrefixClaudeVersion, Kind: KindDir, Area: "claude", Label: "Claude version probe"},
	{Prefix: PrefixClaudeStopHook, Kind: KindDir, Area: "claude", Label: "Claude stop-hook channel"},
	{Prefix: PrefixClaudeSkills, Kind: KindDir, Area: "claude", Label: "Claude skill plugin dir"},
	{Prefix: PrefixClaudeMCPConfig, Suffix: ".json", Kind: KindFile, Area: "claude", Label: "Claude MCP config"},
	{Prefix: PrefixActivityRelay, Kind: KindDir, Area: "harness", Label: "activity relay"},
	{Prefix: PrefixSignalGuard, Kind: KindDir, Area: "runner", Label: "signal guard"},
	{Prefix: PrefixMCPBearer, Kind: KindDir, Area: "runner", Label: "MCP bearer token"},
	{Prefix: PrefixCheckpointIndex, Kind: KindDir, Area: "runner", Label: "checkpoint scratch index"},
	{Prefix: PrefixRescueIndex, Kind: KindDir, Area: "runner", Label: "rescue scratch index"},
	{Prefix: PrefixRescueVerifyIndex, Kind: KindDir, Area: "runner", Label: "rescue verify index"},
	{Prefix: PrefixMuseSession, Kind: KindDir, Area: "muse", Label: "muse session scratch"},
	{Prefix: PrefixMuseInteractive, Kind: KindDir, Area: "muse", Label: "muse interactive scratch"},
	{Prefix: PrefixMuseLogin, Kind: KindDir, Area: "muse", Label: "muse login probe"},
	{Prefix: PrefixPiCatalogProbe, Kind: KindDir, Area: "pi", Label: "pi catalog probe"},
	{Prefix: PrefixOpenCodeConfig, Kind: KindDir, Area: "opencode", Label: "opencode config home"},
	{Prefix: PrefixCodeSurvival, Kind: KindDir, Area: "worker", Label: "code-survival scan workarea"},
}

// byLongestPrefix lists producers longest prefix first, so a name is always
// attributed to the most specific registered prefix it carries.
var byLongestPrefix = func() []Producer {
	out := append([]Producer(nil), producers...)
	sort.SliceStable(out, func(i, j int) bool { return len(out[i].Prefix) > len(out[j].Prefix) })
	return out
}()

// Producers returns every registered producer.
func Producers() []Producer {
	return append([]Producer(nil), producers...)
}

// ErrUnregisteredPrefix reports a MkdirTemp or CreateTemp call whose prefix
// is not registered. Creating an artefact the janitor cannot recognise would
// reintroduce exactly the untracked leftovers this package exists to end, so
// the call fails instead.
var ErrUnregisteredPrefix = errors.New("tempown: unregistered temp prefix")

func lookup(prefix string, kind Kind) (Producer, error) {
	for _, p := range producers {
		if p.Prefix == prefix {
			if p.Kind != kind {
				return Producer{}, fmt.Errorf("%w: %q is registered as a %s", ErrUnregisteredPrefix, prefix, p.Kind)
			}
			return p, nil
		}
	}
	return Producer{}, fmt.Errorf("%w: %q", ErrUnregisteredPrefix, prefix)
}

// ownerTag is the record inserted after the prefix: "p<pid>-".
func ownerTag(pid int) string {
	return "p" + strconv.Itoa(pid) + "-"
}

// MkdirTemp is os.MkdirTemp for a registered directory prefix: it creates
// dir/<prefix>p<pid>-<random> with mode 0700. A trailing "*" in prefix is
// accepted and ignored. dir "" means os.TempDir().
func MkdirTemp(dir, prefix string) (string, error) {
	prefix = strings.TrimSuffix(prefix, "*")
	p, err := lookup(prefix, KindDir)
	if err != nil {
		return "", err
	}
	return os.MkdirTemp(dir, p.Prefix+ownerTag(os.Getpid())+"*")
}

// CreateTemp is os.CreateTemp for a registered file prefix: it creates
// dir/<prefix>p<pid>-<random><suffix> with mode 0600, where suffix is the
// producer's registered suffix. dir "" means os.TempDir().
func CreateTemp(dir, prefix string) (*os.File, error) {
	p, err := lookup(prefix, KindFile)
	if err != nil {
		return nil, err
	}
	return os.CreateTemp(dir, p.Prefix+ownerTag(os.Getpid())+"*"+p.Suffix)
}

// Parse reports the producer and recorded owner PID of a base name created by
// MkdirTemp or CreateTemp. ok is false for any other name, including the
// untagged names releases before the owner tag created (see Legacy).
func Parse(name string) (producer Producer, pid int, ok bool) {
	for _, p := range byLongestPrefix {
		if !strings.HasPrefix(name, p.Prefix) || !strings.HasSuffix(name, p.Suffix) {
			continue
		}
		rest := name[len(p.Prefix):]
		if len(rest) < len(p.Suffix) {
			continue
		}
		rest = rest[:len(rest)-len(p.Suffix)]
		pid, ok := parseOwnerTag(rest)
		if !ok {
			continue
		}
		return p, pid, true
	}
	return Producer{}, 0, false
}

// parseOwnerTag parses "p<pid>-<random>". The random part is the decimal
// string os.MkdirTemp and os.CreateTemp generate.
func parseOwnerTag(rest string) (int, bool) {
	if !strings.HasPrefix(rest, "p") {
		return 0, false
	}
	pidText, random, found := strings.Cut(rest[1:], "-")
	if !found || !allDigits(pidText) || !allDigits(random) {
		return 0, false
	}
	pid, err := strconv.Atoi(pidText)
	if err != nil || pid <= 0 {
		return 0, false
	}
	return pid, true
}

func allDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// Legacy reports whether name carries a registered prefix but no owner tag:
// an artefact a release before the owner tag created. Nothing records its
// owner, so the janitor never removes one; reports count them so an operator
// can see how much untracked debris remains.
func Legacy(name string) (Producer, bool) {
	if _, _, ok := Parse(name); ok {
		return Producer{}, false
	}
	for _, p := range byLongestPrefix {
		if strings.HasPrefix(name, p.Prefix) && strings.HasSuffix(name, p.Suffix) {
			return p, true
		}
	}
	return Producer{}, false
}
