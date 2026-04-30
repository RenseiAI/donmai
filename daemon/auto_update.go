package daemon

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"time"
)

// UpdateCDNBase is the base URL for the rensei CDN that hosts release
// manifests and binaries.
const UpdateCDNBase = "https://updates.rensei.dev"

// VersionManifest is the schema of <channel>/latest.json.
type VersionManifest struct {
	Version    string `json:"version"`
	SHA256     string `json:"sha256"`
	ReleasedAt string `json:"releasedAt"`
}

// UpdateResult describes the outcome of a runUpdate call.
type UpdateResult struct {
	Updated bool   `json:"updated"`
	Version string `json:"version"`
	Reason  string `json:"reason"`
}

// BinaryVerifier is a narrow signature-verification interface. The default
// production verifier rejects all signatures (until REN-1314 ships a Go
// sigstore adapter). Tests can inject a passing verifier.
type BinaryVerifier interface {
	Verify(ctx context.Context, contentHash, signatureValue string) (valid bool, reason string)
}

// alwaysFailVerifier is the default production verifier. Production
// auto-update is only enabled when a real verifier is wired in. Until then,
// the daemon refuses to swap binaries — and runUpdate returns "no verifier".
type alwaysFailVerifier struct{}

// Verify implements BinaryVerifier. It always returns false to prevent the
// daemon from swapping in a binary without proper signature verification.
func (alwaysFailVerifier) Verify(_ context.Context, _, _ string) (bool, string) {
	return false, "no verifier configured (REN-1314 sigstore adapter not yet wired); refusing swap"
}

// UpdaterOptions configure an Updater.
type UpdaterOptions struct {
	CurrentVersion    string
	CurrentBinaryPath string
	Config            AutoUpdateConfig

	// HTTPClient is the client used to fetch the manifest, binary, and
	// signature. Defaults to a 60s-timeout client.
	HTTPClient *http.Client
	// Verifier is the binary-signature verifier. Defaults to
	// alwaysFailVerifier (production-safe — no real swaps until configured).
	Verifier BinaryVerifier
	// SkipExit, when true, prevents the swap step from calling os.Exit. Used
	// by tests and by callers that want to handle the restart explicitly.
	SkipExit bool
	// ExitFn allows tests to inject a fake exit. Called only when SkipExit
	// is false. Defaults to os.Exit.
	ExitFn func(int)
	// CDNBase overrides UpdateCDNBase (test injection).
	CDNBase string
	// PlatformSuffix overrides the auto-detected suffix (test injection).
	PlatformSuffix string
}

// Updater runs the full update flow: check → fetch → verify → swap → restart.
type Updater struct {
	opts UpdaterOptions
}

// NewUpdater returns an Updater with sane defaults.
func NewUpdater(opts UpdaterOptions) *Updater {
	if opts.HTTPClient == nil {
		opts.HTTPClient = &http.Client{Timeout: 60 * time.Second}
	}
	if opts.Verifier == nil {
		opts.Verifier = alwaysFailVerifier{}
	}
	if opts.ExitFn == nil {
		opts.ExitFn = os.Exit
	}
	if opts.CDNBase == "" {
		opts.CDNBase = UpdateCDNBase
	}
	if opts.PlatformSuffix == "" {
		opts.PlatformSuffix = ResolvePlatformSuffix()
	}
	if opts.CurrentBinaryPath == "" {
		if exe, err := os.Executable(); err == nil {
			if resolved, err := filepath.EvalSymlinks(exe); err == nil {
				exe = resolved
			}
			opts.CurrentBinaryPath = exe
		}
	}
	return &Updater{opts: opts}
}

// ResolvePlatformSuffix returns "<arch>-<os>" suitable for the CDN binary
// filename, e.g. "arm64-darwin", "amd64-linux".
func ResolvePlatformSuffix() string {
	return fmt.Sprintf("%s-%s", goArchToNode(runtime.GOARCH), runtime.GOOS)
}

// goArchToNode converts Go's GOARCH naming to the Node-style suffix used by
// the CDN. For now we map amd64 → amd64 (matches CDN naming) but keep the
// hook for future divergences.
func goArchToNode(goarch string) string {
	return goarch
}

// BuildManifestURL returns the manifest URL for a channel.
func (u *Updater) BuildManifestURL(channel UpdateChannel) string {
	return fmt.Sprintf("%s/%s/latest.json", strings.TrimRight(u.opts.CDNBase, "/"), channel)
}

// BuildBinaryURL returns the binary URL for a channel/version.
func (u *Updater) BuildBinaryURL(channel UpdateChannel, version string) string {
	return fmt.Sprintf("%s/%s/%s/rensei-daemon-%s",
		strings.TrimRight(u.opts.CDNBase, "/"), channel, version, u.opts.PlatformSuffix)
}

// BuildSignatureURL returns the signature URL for a binary URL.
func (u *Updater) BuildSignatureURL(binURL string) string {
	return binURL + ".sig"
}

// CheckForUpdate fetches the version manifest and returns it iff a strictly
// newer version is available. Returns (nil, nil) when up-to-date.
func (u *Updater) CheckForUpdate(ctx context.Context) (*VersionManifest, error) {
	url := u.BuildManifestURL(u.opts.Config.Channel)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("User-Agent", "rensei-daemon/"+u.opts.CurrentVersion)
	res, err := u.opts.HTTPClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetch manifest: %w", err)
	}
	defer func() { _ = res.Body.Close() }()
	if res.StatusCode >= 400 {
		return nil, fmt.Errorf("manifest HTTP %d", res.StatusCode)
	}
	var m VersionManifest
	if err := json.NewDecoder(res.Body).Decode(&m); err != nil {
		return nil, fmt.Errorf("decode manifest: %w", err)
	}
	if m.Version == "" {
		return nil, errors.New("malformed manifest: missing version")
	}
	if !IsNewerVersion(m.Version, u.opts.CurrentVersion) {
		return nil, nil
	}
	return &m, nil
}

// RunUpdate executes the complete update flow. When successful and SkipExit
// is false, it calls ExitFn(ExitCodeRestart) and does not return.
//
//nolint:gocyclo // The flow is intentionally linear and each step is a guard.
func (u *Updater) RunUpdate(ctx context.Context) (*UpdateResult, error) {
	manifest, err := u.CheckForUpdate(ctx)
	if err != nil {
		return &UpdateResult{Updated: false, Version: u.opts.CurrentVersion, Reason: "version-check-failed: " + err.Error()}, err
	}
	if manifest == nil {
		return &UpdateResult{Updated: false, Version: u.opts.CurrentVersion, Reason: "already-up-to-date"}, nil
	}

	binURL := u.BuildBinaryURL(u.opts.Config.Channel, manifest.Version)
	sigURL := u.BuildSignatureURL(binURL)

	tmpBin, err := u.downloadToTemp(ctx, binURL, manifest.Version)
	if err != nil {
		return &UpdateResult{Updated: false, Version: u.opts.CurrentVersion, Reason: "download-failed: " + err.Error()}, err
	}
	defer func() { _ = os.Remove(tmpBin) }() // best-effort cleanup; rename below replaces it.

	signature, err := u.downloadString(ctx, sigURL)
	if err != nil {
		return &UpdateResult{Updated: false, Version: u.opts.CurrentVersion, Reason: "sig-download-failed: " + err.Error()}, err
	}

	hash, err := sha256OfFile(tmpBin)
	if err != nil {
		return &UpdateResult{Updated: false, Version: u.opts.CurrentVersion, Reason: "hash-failed: " + err.Error()}, err
	}

	valid, reason := u.opts.Verifier.Verify(ctx, hash, signature)
	if !valid {
		return &UpdateResult{Updated: false, Version: u.opts.CurrentVersion, Reason: "sig-rejected: " + reason}, nil
	}

	if u.opts.CurrentBinaryPath == "" {
		return &UpdateResult{Updated: false, Version: u.opts.CurrentVersion, Reason: "no current-binary-path"}, errors.New("currentBinaryPath unresolved")
	}

	if err := os.Chmod(tmpBin, 0o755); err != nil { //nolint:gosec
		return &UpdateResult{Updated: false, Version: u.opts.CurrentVersion, Reason: "chmod-failed: " + err.Error()}, err
	}
	if err := os.Rename(tmpBin, u.opts.CurrentBinaryPath); err != nil {
		return &UpdateResult{Updated: false, Version: u.opts.CurrentVersion, Reason: "swap-failed: " + err.Error()}, err
	}

	result := &UpdateResult{Updated: true, Version: manifest.Version, Reason: "update-applied"}
	if !u.opts.SkipExit {
		u.opts.ExitFn(ExitCodeRestart)
	}
	return result, nil
}

func (u *Updater) downloadToTemp(ctx context.Context, url, version string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("User-Agent", "rensei-daemon/"+u.opts.CurrentVersion)
	res, err := u.opts.HTTPClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("fetch binary: %w", err)
	}
	defer func() { _ = res.Body.Close() }()
	if res.StatusCode >= 400 {
		return "", fmt.Errorf("binary HTTP %d", res.StatusCode)
	}
	tmp := filepath.Join(os.TempDir(), fmt.Sprintf("rensei-daemon-%s-%s-%d.tmp", u.opts.PlatformSuffix, version, time.Now().UnixNano()))
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600) //nolint:gosec
	if err != nil {
		return "", fmt.Errorf("open temp: %w", err)
	}
	defer func() { _ = f.Close() }()
	if _, err := io.Copy(f, res.Body); err != nil {
		_ = os.Remove(tmp)
		return "", fmt.Errorf("write temp: %w", err)
	}
	return tmp, nil
}

func (u *Updater) downloadString(ctx context.Context, url string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("User-Agent", "rensei-daemon/"+u.opts.CurrentVersion)
	res, err := u.opts.HTTPClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("fetch: %w", err)
	}
	defer func() { _ = res.Body.Close() }()
	if res.StatusCode >= 400 {
		return "", fmt.Errorf("HTTP %d", res.StatusCode)
	}
	body, err := io.ReadAll(res.Body)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(body)), nil
}

func sha256OfFile(path string) (string, error) {
	f, err := os.Open(path) //nolint:gosec
	if err != nil {
		return "", err
	}
	defer func() { _ = f.Close() }()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// IsNewerVersion returns true if candidate is strictly newer than current
// according to semver-prefix comparison. Falls back to lexicographic compare
// if either string is not a parseable semver prefix.
func IsNewerVersion(candidate, current string) bool {
	if candidate == current {
		return false
	}
	cv, ok1 := parseSemver(candidate)
	cr, ok2 := parseSemver(current)
	if !ok1 || !ok2 {
		return candidate > current
	}
	for i := 0; i < 3; i++ {
		if cv[i] > cr[i] {
			return true
		}
		if cv[i] < cr[i] {
			return false
		}
	}
	return false
}

var semverRE = regexp.MustCompile(`^v?(\d+)\.(\d+)\.(\d+)`)

func parseSemver(v string) ([3]int, bool) {
	m := semverRE.FindStringSubmatch(v)
	if m == nil {
		return [3]int{}, false
	}
	out := [3]int{}
	for i := 0; i < 3; i++ {
		n, err := strconv.Atoi(m[i+1])
		if err != nil {
			return [3]int{}, false
		}
		out[i] = n
	}
	return out, true
}
