package daemon

// machine_id.go — the daemon's stable machine identity.
//
// WHY THIS EXISTS
//
// The value the daemon reports as `machineId` at registration is what an
// orchestrator keys host identity on: one host row per (machine, tenant).
// Historically the daemon reported a hostname-derived string
// (DeriveDefaultMachineID → os.Hostname()), which is NOT a stable identifier:
//
//   - os.Hostname() on macOS returns the DHCP/mDNS-supplied name, which flips
//     between "<name>.local" and "<name>.localdomain" (and to a router-supplied
//     generic name) depending on the network the machine is attached to.
//   - `hostname`, the Bonjour LocalHostName, and the fully-qualified name are
//     three different lookups that can disagree at the same instant.
//   - Users rename machines.
//
// Every variant that has ever been observed becomes a SEPARATE host identity
// upstream: one physical machine fans out into several host rows, capacity is
// double-counted, and re-installs fork yet another identity instead of
// reclaiming the existing one.
//
// RESOLUTION ORDER
//
//  1. An explicit operator override (MachineIDEnvVar). Containers and CI need
//     to pin identity from the outside; nothing below can beat an explicit
//     declaration.
//  2. A hash of the OS-native machine identifier (darwin IOPlatformUUID, Linux
//     /etc/machine-id). This is the preferred source: it is stable across
//     reboot, rename, network change and daemon re-install, and — because it is
//     not stored in the state directory — it CANNOT be duplicated by copying or
//     syncing a config/state directory between machines. Wiping the state dir
//     and re-installing reclaims the SAME identity.
//  3. A random identifier generated once and persisted. Only reached when the
//     OS exposes no native identifier. It is written to the machine-LOCAL data
//     directory (~/Library/Application Support, %LOCALAPPDATA%, $XDG_STATE_HOME)
//     rather than the state directory, precisely because the state directory is
//     the sort of thing operators sync between machines and a synced identity
//     file would collapse two machines into one identity.
//
// PRIVACY
//
// The OS-native identifier is NEVER transmitted. It is domain-separated and
// hashed (SHA-256, truncated) before it leaves this file, so the wire value is
// an opaque token that cannot be correlated with the hardware id or with any
// other identifier derived from it.
//
// The resolved value is computed once per process and memoized so that every
// registration lane in the process — however many tenants it serves — reports
// exactly one machine identity.

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/RenseiAI/donmai/runtime/statehome"
)

const (
	// MachineIDEnvVar pins the machine identity from the environment. Set it
	// on hosts where the daemon's own resolution cannot work (immutable
	// containers that must present a caller-chosen identity, test fixtures).
	MachineIDEnvVar = "DONMAI_MACHINE_ID"

	// machineIDPrefix tags the wire value so an operator reading a host table
	// can tell a resolved machine identity from a legacy hostname string.
	machineIDPrefix = "mid_"

	// machineIDHashDomain domain-separates the hash of the OS-native machine
	// identifier. Without it, anything else that hashes the same OS value
	// would produce a correlatable token.
	machineIDHashDomain = "donmai/machine-id/v1\x00"

	// machineIDHexLen is how many hex characters of the digest ride the wire.
	// 32 hex = 128 bits: collision-free for any realistic fleet, and short
	// enough to read in a table.
	machineIDHexLen = 32

	// machineIDFileName is the persisted-fallback file (tier 3).
	machineIDFileName = "machine-id"

	// nativeMachineIDTimeout bounds the darwin ioreg subprocess. A hung
	// lookup must degrade to the persisted fallback, never stall startup.
	nativeMachineIDTimeout = 3 * time.Second
)

// MachineIDSource records which tier produced the identity. Reported in the
// resolution log line so an operator can tell a hardware-derived identity
// (stable, unforkable) from a persisted one (stable, but only as stable as
// the file).
type MachineIDSource string

const (
	// MachineIDSourceEnv — pinned by MachineIDEnvVar.
	MachineIDSourceEnv MachineIDSource = "env"
	// MachineIDSourceNative — hashed from the OS-native machine identifier.
	MachineIDSourceNative MachineIDSource = "native"
	// MachineIDSourcePersisted — read from a previously written file.
	MachineIDSourcePersisted MachineIDSource = "persisted"
	// MachineIDSourceGenerated — freshly generated and written to disk.
	MachineIDSourceGenerated MachineIDSource = "generated"
	// MachineIDSourceEphemeral — generated but NOT persistable (no writable
	// local directory). Survives only for the life of the process; logged
	// loudly because it re-forks identity on every restart.
	MachineIDSourceEphemeral MachineIDSource = "ephemeral"
)

// machineIDDeps are the environment lookups machineID resolution performs.
// Injected so the resolution order can be tested without a real machine.
type machineIDDeps struct {
	// Getenv reads the operator override. Defaults to os.Getenv.
	Getenv func(string) string
	// NativeID returns the OS-native machine identifier, or "" when the
	// platform exposes none. Defaults to nativeMachineID.
	NativeID func() string
	// LocalDir returns the machine-local (never-synced) directory the
	// persisted fallback lives in. Defaults to localMachineStateDir.
	LocalDir func() (string, error)
	// RandomHex returns n hex characters of cryptographic randomness.
	// Defaults to randomHex.
	RandomHex func(n int) (string, error)
}

func (d machineIDDeps) getenv(k string) string {
	if d.Getenv != nil {
		return d.Getenv(k)
	}
	return os.Getenv(k)
}

func (d machineIDDeps) nativeID() string {
	if d.NativeID != nil {
		return d.NativeID()
	}
	return nativeMachineID()
}

func (d machineIDDeps) localDir() (string, error) {
	if d.LocalDir != nil {
		return d.LocalDir()
	}
	return localMachineStateDir()
}

func (d machineIDDeps) randomHex(n int) (string, error) {
	if d.RandomHex != nil {
		return d.RandomHex(n)
	}
	return randomHex(n)
}

var (
	machineIDOnce   sync.Once
	machineIDCached string
)

// MachineID returns this machine's stable identity, resolving it on first use
// and memoizing the result for the life of the process.
//
// This is the single authoritative host-identity source. Registration reports
// it as `machineId`; the hostname is a human-readable LABEL only and must
// never be used as an identity key.
//
// Never returns an empty string: the last-resort path generates an ephemeral
// identity and logs the degradation rather than reporting no identity at all.
func MachineID() string {
	machineIDOnce.Do(func() {
		id, source, err := resolveMachineID(machineIDDeps{})
		if err != nil {
			slog.Warn("[machine-id] resolution degraded",
				"source", string(source),
				"err", err.Error(),
			)
		}
		slog.Info("[machine-id]",
			"event", "resolved",
			"machineId", id,
			"source", string(source),
		)
		machineIDCached = id
	})
	return machineIDCached
}

// resolveMachineID implements the documented resolution order. The returned
// error is advisory — a non-empty id is always returned alongside it.
func resolveMachineID(deps machineIDDeps) (string, MachineIDSource, error) {
	// 1. Operator override.
	if pinned := sanitizeMachineID(deps.getenv(MachineIDEnvVar)); pinned != "" {
		return pinned, MachineIDSourceEnv, nil
	}

	// 2. OS-native identifier, hashed. Preferred: not stored in any directory
	// an operator might sync, and reclaimed verbatim after a re-install.
	if native := strings.TrimSpace(deps.nativeID()); native != "" {
		return machineIDPrefix + hashMachineIdentifier(native), MachineIDSourceNative, nil
	}

	// 3. Persisted fallback in the machine-local directory.
	dir, dirErr := deps.localDir()
	if dirErr == nil {
		path := filepath.Join(dir, machineIDFileName)
		if existing := readPersistedMachineID(path); existing != "" {
			return existing, MachineIDSourcePersisted, nil
		}
		generated, genErr := generateMachineID(deps)
		if genErr != nil {
			return fallbackMachineID(), MachineIDSourceEphemeral, genErr
		}
		if writeErr := writePersistedMachineID(path, generated); writeErr != nil {
			return generated, MachineIDSourceEphemeral, fmt.Errorf(
				"persist machine id to %s: %w", path, writeErr)
		}
		return generated, MachineIDSourceGenerated, nil
	}

	generated, genErr := generateMachineID(deps)
	if genErr != nil {
		return fallbackMachineID(), MachineIDSourceEphemeral, genErr
	}
	return generated, MachineIDSourceEphemeral, fmt.Errorf(
		"no writable machine-local directory: %w", dirErr)
}

// hashMachineIdentifier domain-separates and hashes an OS-native identifier so
// the raw value never leaves the machine.
func hashMachineIdentifier(native string) string {
	sum := sha256.Sum256([]byte(machineIDHashDomain + strings.ToLower(strings.TrimSpace(native))))
	return hex.EncodeToString(sum[:])[:machineIDHexLen]
}

func generateMachineID(deps machineIDDeps) (string, error) {
	h, err := deps.randomHex(machineIDHexLen)
	if err != nil {
		return "", fmt.Errorf("generate machine id: %w", err)
	}
	return machineIDPrefix + h, nil
}

// fallbackMachineID is the identity of last resort, used only when even
// randomness is unavailable. Deliberately constant so a broken host collapses
// to ONE bogus identity rather than fanning out into many.
func fallbackMachineID() string {
	return machineIDPrefix + strings.Repeat("0", machineIDHexLen)
}

func randomHex(n int) (string, error) {
	buf := make([]byte, (n+1)/2)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("read randomness: %w", err)
	}
	return hex.EncodeToString(buf)[:n], nil
}

// persistedMachineID is the on-disk shape of the tier-3 fallback.
type persistedMachineID struct {
	MachineID string `json:"machineId"`
	CreatedAt string `json:"createdAt,omitempty"`
	// Note is a breadcrumb for a human who finds this file; it carries no
	// behaviour.
	Note string `json:"note,omitempty"`
}

func readPersistedMachineID(path string) string {
	data, err := os.ReadFile(path) //nolint:gosec // daemon-owned local state path
	if err != nil {
		return ""
	}
	var p persistedMachineID
	if err := json.Unmarshal(data, &p); err != nil {
		return ""
	}
	return sanitizeMachineID(p.MachineID)
}

func writePersistedMachineID(path, id string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("create machine id dir: %w", err)
	}
	data, err := json.MarshalIndent(persistedMachineID{
		MachineID: id,
		CreatedAt: time.Now().UTC().Format(time.RFC3339),
		Note: "Stable identity for THIS machine. Do not copy this file to " +
			"another machine and do not sync it — two machines sharing one id " +
			"are indistinguishable to any orchestrator.",
	}, "", "  ")
	if err != nil {
		return fmt.Errorf("encode machine id: %w", err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		return fmt.Errorf("write machine id: %w", err)
	}
	return nil
}

// machineIDInvalidRE matches everything the wire value may NOT contain. The
// resolved id must survive being embedded in an upstream host key verbatim.
var machineIDInvalidRE = regexp.MustCompile(`[^a-zA-Z0-9_-]`)

// sanitizeMachineID normalizes an externally supplied identity (the env
// override, or a previously persisted file) to the wire-safe alphabet.
// Returns "" for anything that sanitizes away to nothing.
func sanitizeMachineID(v string) string {
	v = strings.TrimSpace(v)
	if v == "" {
		return ""
	}
	v = machineIDInvalidRE.ReplaceAllString(v, "-")
	v = strings.Trim(v, "-")
	if len(v) > 128 {
		v = v[:128]
	}
	return v
}

// localMachineStateDir returns the per-machine, per-user directory that
// convention EXCLUDES from dotfile/config synchronisation:
//
//	darwin  ~/Library/Application Support/<brand>
//	windows %LOCALAPPDATA%\<brand>   (the explicitly non-roaming location)
//	other   $XDG_STATE_HOME/<brand> or ~/.local/state/<brand>
//
// Deliberately NOT the daemon state directory: that one holds config an
// operator may well sync between their machines, and a synced identity file
// would give two machines the same identity.
func localMachineStateDir() (string, error) {
	brand := localStateBrand()
	switch runtime.GOOS {
	case "darwin":
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("resolve home dir: %w", err)
		}
		return filepath.Join(home, "Library", "Application Support", brand), nil
	case "windows":
		if base := os.Getenv("LOCALAPPDATA"); base != "" {
			return filepath.Join(base, brand), nil
		}
		return "", fmt.Errorf("LOCALAPPDATA is unset")
	default:
		if base := os.Getenv("XDG_STATE_HOME"); base != "" {
			return filepath.Join(base, brand), nil
		}
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("resolve home dir: %w", err)
		}
		return filepath.Join(home, ".local", "state", brand), nil
	}
}

// localStateBrand derives the directory name from the configured state-home
// brand so an embedder running under its own brand does not write into
// another's directory.
func localStateBrand() string {
	if b := strings.TrimSpace(statehome.Brand()); b != "" {
		return b
	}
	return statehome.DefaultBrand
}

// nativeMachineID returns the OS-native machine identifier, or "" when the
// platform exposes none (or the lookup fails). Never fatal: the caller has a
// persisted fallback.
func nativeMachineID() string {
	switch runtime.GOOS {
	case "darwin":
		return darwinPlatformUUID()
	case "linux":
		return linuxMachineID()
	default:
		return ""
	}
}

// linuxMachineIDPaths are the systemd/D-Bus locations, in preference order.
var linuxMachineIDPaths = []string{
	"/etc/machine-id",
	"/var/lib/dbus/machine-id",
}

func linuxMachineID() string {
	for _, path := range linuxMachineIDPaths {
		data, err := os.ReadFile(path) //nolint:gosec // fixed OS-owned paths
		if err != nil {
			continue
		}
		if id := strings.TrimSpace(string(data)); id != "" {
			return id
		}
	}
	return ""
}

// darwinIOPlatformUUIDRE extracts the UUID from `ioreg` output of the form:
//
//	"IOPlatformUUID" = "XXXXXXXX-XXXX-XXXX-XXXX-XXXXXXXXXXXX"
var darwinIOPlatformUUIDRE = regexp.MustCompile(`"IOPlatformUUID"\s*=\s*"([^"]+)"`)

func darwinPlatformUUID() string {
	ctx, cancel := context.WithTimeout(context.Background(), nativeMachineIDTimeout)
	defer cancel()
	// Fixed command, fixed arguments — nothing here is caller-controlled.
	out, err := exec.CommandContext(ctx, "ioreg", "-rd1", "-c", "IOPlatformExpertDevice").Output()
	if err != nil {
		return ""
	}
	return parseDarwinPlatformUUID(string(out))
}

func parseDarwinPlatformUUID(out string) string {
	m := darwinIOPlatformUUIDRE.FindStringSubmatch(out)
	if len(m) < 2 {
		return ""
	}
	return strings.TrimSpace(m[1])
}
