// Command session-shim-acceptance-mutator drives authenticated, target-owned
// fault stimuli for the installed session-shim acceptance.
package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/RenseiAI/donmai/attachwire"
	"github.com/RenseiAI/donmai/sessionshim"
	"github.com/RenseiAI/donmai/shimwire"
)

const (
	envStateDir   = "DONMAI_SESSION_SHIM_ACCEPTANCE_STATE_DIR"
	envRegistry   = "DONMAI_SESSION_SHIM_ACCEPTANCE_REGISTRY_DIR"
	envDaemonURL  = "DONMAI_SESSION_SHIM_ACCEPTANCE_DAEMON_URL"
	envCandidate  = "DONMAI_SESSION_SHIM_ACCEPTANCE_CANDIDATE"
	controlPrefix = "/api/daemon/session-shim/acceptance/"
	stateSchema   = 1
	maxBody       = 2 << 20
	// helperHarnessLifetime outlives any acceptance run, so the harness is
	// always reaped by this helper rather than expiring on its own — a child
	// that exited by itself would prove nothing about the reap.
	helperHarnessLifetime = "600"
	// helperTerminationGrace is the SIGTERM→SIGKILL window, matching the shim's
	// own bounded teardown shape.
	helperTerminationGrace = 2 * time.Second
	// helperTombstoneMarker names this helper's terminal publication. It is the
	// provenance string a build can be grepped for.
	helperTombstoneMarker = "acceptance helper tombstone written"
)

type config struct {
	stateDir  string
	registry  string
	daemonURL string
	candidate string
	tokenFile string
}

type state struct {
	SchemaVersion int          `json:"schemaVersion"`
	SessionID     string       `json:"sessionId,omitempty"`
	OrgID         string       `json:"orgId,omitempty"`
	Helper        *helperState `json:"helper,omitempty"`
}

type helperState struct {
	OrgID            string `json:"orgId"`
	SessionID        string `json:"sessionId"`
	ShimID           string `json:"shimId"`
	ProcessEpoch     uint64 `json:"processEpoch"`
	PID              int    `json:"pid"`
	ProcessStartedAt int64  `json:"processStartedAt"`
	HarnessPID       int    `json:"harnessPid"`
	HarnessStartedAt int64  `json:"harnessStartedAt"`
	RecordPath       string `json:"recordPath"`
	SocketPath       string `json:"socketPath"`
	StopPath         string `json:"stopPath"`
}

type helperLaunch struct {
	OrgID        string `json:"orgId"`
	SessionID    string `json:"sessionId"`
	ShimID       string `json:"shimId"`
	ProcessEpoch uint64 `json:"processEpoch"`
	RegistryDir  string `json:"registryDir"`
	RecordPath   string `json:"recordPath"`
	SocketPath   string `json:"socketPath"`
	ReadyPath    string `json:"readyPath"`
	StopPath     string `json:"stopPath"`
}

type daemonStatus struct {
	SessionShim struct {
		Adopted []struct {
			OrgID        string `json:"orgId"`
			SessionID    string `json:"sessionId"`
			ShimID       string `json:"shimId"`
			ProcessEpoch uint64 `json:"processEpoch"`
		} `json:"adopted"`
	} `json:"sessionShim"`
}

type controlRequest struct {
	OrgID        string `json:"orgId"`
	SessionID    string `json:"sessionId"`
	ShimID       string `json:"shimId,omitempty"`
	ProcessEpoch uint64 `json:"processEpoch,omitempty"`
}

func main() {
	if err := run(os.Args[1:]); err != nil {
		_, _ = fmt.Fprintln(os.Stderr, "session-shim-acceptance-mutator:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) == 0 {
		return errors.New("command is required")
	}
	if args[0] == "__hold-incompatible" {
		if len(args) != 1 {
			return usage(args[0])
		}
		return holdIncompatible()
	}
	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	switch args[0] {
	case "check":
		if len(args) != 1 {
			return usage(args[0])
		}
		return cfg.check()
	case "prepare":
		if len(args) != 1 {
			return usage(args[0])
		}
		return cfg.prepare()
	case "force-gap":
		return cfg.withSession(args, "force-gap", nil)
	case "quarantine-arm":
		if len(args) != 2 {
			return usage(args[0])
		}
		return cfg.quarantineArm(args[1])
	case "quarantine-clear":
		if len(args) != 2 {
			return usage(args[0])
		}
		return cfg.quarantineClear(args[1])
	case "fence-refuse-arm":
		return cfg.withSession(args, "fence-refuse-arm", nil)
	case "fence-refuse-clear":
		return cfg.withSession(args, "fence-refuse-clear", nil)
	case "cleanup":
		if len(args) > 2 {
			return usage(args[0])
		}
		var sessionID string
		if len(args) == 2 {
			sessionID = args[1]
		}
		return cfg.cleanup(sessionID)
	default:
		return fmt.Errorf("unknown command %q", args[0])
	}
}

func loadConfig() (config, error) {
	cfg := config{
		stateDir:  strings.TrimSpace(os.Getenv(envStateDir)),
		registry:  strings.TrimSpace(os.Getenv(envRegistry)),
		daemonURL: strings.TrimRight(strings.TrimSpace(os.Getenv(envDaemonURL)), "/"),
		candidate: strings.TrimSpace(os.Getenv(envCandidate)),
		tokenFile: strings.TrimSpace(os.Getenv(acceptanceTokenPathEnvironment())),
	}
	if cfg.daemonURL == "" {
		cfg.daemonURL = "http://127.0.0.1:7734"
	}
	for name, value := range map[string]string{
		envStateDir: cfg.stateDir, envRegistry: cfg.registry,
		envCandidate: cfg.candidate, acceptanceTokenPathEnvironment(): cfg.tokenFile,
	} {
		if value == "" || !filepath.IsAbs(value) {
			return config{}, fmt.Errorf("%s must be an absolute path", name)
		}
	}
	if filepath.Clean(cfg.stateDir) == string(filepath.Separator) || filepath.Clean(cfg.registry) == string(filepath.Separator) {
		return config{}, errors.New("state and registry directories must not be the filesystem root")
	}
	if !strings.HasPrefix(cfg.daemonURL, "http://127.0.0.1:") && !strings.HasPrefix(cfg.daemonURL, "http://localhost:") {
		return config{}, errors.New("acceptance daemon URL must be loopback HTTP")
	}
	return cfg, nil
}

func (c config) check() error {
	return c.checkForOS(runtime.GOOS)
}

func (c config) checkForOS(goos string) error {
	if goos != "linux" {
		return fmt.Errorf("unsupported operating system %s", goos)
	}
	candidateRoot, err := os.OpenRoot(filepath.Dir(c.candidate))
	if err != nil {
		return err
	}
	defer func() { _ = candidateRoot.Close() }()
	candidateName := filepath.Base(c.candidate)
	info, err := candidateRoot.Stat(candidateName)
	if err != nil || info.IsDir() || info.Mode()&0o111 == 0 {
		return errors.New("candidate is not executable")
	}
	if info.Size() <= 0 || info.Size() > 512<<20 {
		return errors.New("candidate executable size is invalid")
	}
	binary, err := candidateRoot.ReadFile(candidateName)
	if err != nil {
		return err
	}
	for _, marker := range [][]byte{
		[]byte("/api/daemon/session-shim/acceptance/"),
		[]byte("restart_fence_refused"),
	} {
		if !bytes.Contains(binary, marker) {
			return errors.New("candidate lacks the dormant session-shim acceptance control")
		}
	}
	if filepath.Dir(c.tokenFile) != c.stateDir {
		return errors.New("token file must be directly inside the acceptance state directory")
	}
	return nil
}

func (c config) prepare() error {
	return c.prepareForOS(runtime.GOOS)
}

func (c config) prepareForOS(goos string) error {
	if err := c.checkForOS(goos); err != nil {
		return err
	}
	if err := requireEmptyOrAbsent(c.stateDir); err != nil {
		return err
	}
	if err := ensurePrivateDir(c.stateDir); err != nil {
		return err
	}
	var raw [32]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return err
	}
	token := base64.RawURLEncoding.EncodeToString(raw[:])
	if err := atomicWrite(c.tokenFile, []byte(token+"\n"), 0o600); err != nil {
		return err
	}
	if err := setServiceEnvironment(acceptanceTokenPathEnvironment(), c.tokenFile); err != nil {
		return err
	}
	return c.saveState(state{SchemaVersion: stateSchema})
}

func (c config) withSession(args []string, action string, extra *helperState) error {
	if len(args) != 2 {
		return usage(action)
	}
	correlation, err := c.resolveSession(args[1])
	if err != nil {
		return err
	}
	if extra != nil {
		correlation.ShimID = extra.ShimID
		correlation.ProcessEpoch = extra.ProcessEpoch
	}
	return c.control(action, correlation)
}

func (c config) quarantineArm(sessionID string) error {
	current, err := c.loadState()
	if err != nil {
		return err
	}
	if current.Helper != nil {
		if current.Helper.SessionID == sessionID {
			return nil
		}
		return errors.New("a different incompatible fixture is already armed")
	}
	correlation, err := c.resolveSession(sessionID)
	if err != nil {
		return err
	}
	shimID, err := randomHex(16)
	if err != nil {
		return err
	}
	processEpoch := uint64(1)
	digest := sha256.Sum256([]byte(correlation.OrgID + "\x00" + sessionID + "\x00" + shimID))
	name := "acceptance-" + hex.EncodeToString(digest[:16])
	recordPath := filepath.Join(c.registry, name+".json")
	socketPath := filepath.Join(c.registry, name+".sock")
	readyPath := filepath.Join(c.stateDir, name+".ready.json")
	stopPath := filepath.Join(c.stateDir, name+".stop")
	process, err := startDetachedSelf(helperLaunch{
		OrgID: correlation.OrgID, SessionID: sessionID, ShimID: shimID,
		ProcessEpoch: processEpoch, RegistryDir: c.registry,
		RecordPath: recordPath, SocketPath: socketPath, ReadyPath: readyPath, StopPath: stopPath,
	})
	if err != nil {
		return fmt.Errorf("start incompatible shim fixture: %w", err)
	}
	_ = process.Release()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	var helper helperState
	if err := waitFor(ctx, 50*time.Millisecond, func() error {
		return decodeStrictFile(readyPath, &helper)
	}); err != nil {
		return err
	}
	if helper.OrgID != correlation.OrgID || helper.SessionID != sessionID || helper.ShimID != shimID || helper.ProcessEpoch != processEpoch || helper.RecordPath != recordPath || helper.SocketPath != socketPath || helper.StopPath != stopPath {
		return errors.New("incompatible helper changed exact correlation")
	}
	if err := c.control("quarantine-arm", correlation); err != nil {
		_ = atomicWrite(stopPath, []byte("stop\n"), 0o600)
		return err
	}
	current.OrgID, current.SessionID, current.Helper = correlation.OrgID, sessionID, &helper
	return c.saveState(current)
}

func (c config) quarantineClear(sessionID string) error {
	current, err := c.loadState()
	if err != nil {
		return err
	}
	if current.Helper == nil {
		return nil
	}
	helper := *current.Helper
	if helper.SessionID != sessionID {
		return errors.New("quarantine clear changed session identity")
	}
	if err := atomicWrite(helper.StopPath, []byte("stop\n"), 0o600); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	// The withdrawn record is also the tombstone's receipt: PutTombstone
	// publishes the terminal proof and only THEN removes the exact discovery
	// record, so a helper whose record is gone has already left its evidence
	// behind for the daemon's clear to reconcile.
	if err := waitFor(ctx, 50*time.Millisecond, func() error {
		if processAlive(helper.PID) || pathExists(helper.RecordPath) || pathExists(helper.SocketPath) {
			return errors.New("incompatible helper remains live")
		}
		return nil
	}); err != nil {
		return err
	}
	correlation := controlRequest{OrgID: helper.OrgID, SessionID: helper.SessionID, ShimID: helper.ShimID, ProcessEpoch: helper.ProcessEpoch}
	if err := c.control("quarantine-clear", correlation); err != nil {
		return err
	}
	current.Helper = nil
	return c.saveState(current)
}

func (c config) cleanup(sessionID string) error {
	current, err := c.loadState()
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if current.Helper != nil {
		if err := c.quarantineClear(current.Helper.SessionID); err != nil {
			return err
		}
		current, _ = c.loadState()
	}
	if sessionID == "" {
		sessionID = current.SessionID
	}
	if sessionID != "" {
		correlation, resolveErr := c.resolveSession(sessionID)
		if resolveErr == nil {
			_ = c.control("cleanup", correlation)
		}
	}
	if err := unsetServiceEnvironment(acceptanceTokenPathEnvironment()); err != nil {
		return err
	}
	_ = os.Remove(c.tokenFile)
	return nil
}

func (c config) resolveSession(sessionID string) (controlRequest, error) {
	var status daemonStatus
	if err := c.request(http.MethodGet, "/api/daemon/status", "", nil, &status, http.StatusOK); err != nil {
		return controlRequest{}, err
	}
	var matched *controlRequest
	for _, adopted := range status.SessionShim.Adopted {
		if adopted.SessionID != sessionID {
			continue
		}
		if matched != nil {
			return controlRequest{}, errors.New("session id is ambiguous across organizations")
		}
		value := controlRequest{OrgID: adopted.OrgID, SessionID: adopted.SessionID}
		matched = &value
	}
	if matched == nil || matched.OrgID == "" {
		return controlRequest{}, errors.New("exact session is not adopted by this daemon")
	}
	return *matched, nil
}

func (c config) control(action string, body controlRequest) error {
	tokenRoot, err := os.OpenRoot(filepath.Dir(c.tokenFile))
	if err != nil {
		return err
	}
	defer func() { _ = tokenRoot.Close() }()
	token, err := tokenRoot.ReadFile(filepath.Base(c.tokenFile))
	if err != nil {
		return err
	}
	defer func() {
		for i := range token {
			token[i] = 0
		}
	}()
	return c.request(http.MethodPost, controlPrefix+action, strings.TrimSpace(string(token)), body, nil, http.StatusNoContent)
}

func (c config) request(method, path, bearer string, body, out any, allowed ...int) error {
	var reader io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return err
		}
		reader = bytes.NewReader(raw)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, method, c.daemonURL+path, reader)
	if err != nil {
		return err
	}
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return errors.New("daemon control transport failed")
	}
	defer func() { _ = resp.Body.Close() }()
	ok := false
	for _, code := range allowed {
		ok = ok || resp.StatusCode == code
	}
	if !ok {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxBody))
		return fmt.Errorf("daemon control returned HTTP %d", resp.StatusCode)
	}
	if out == nil {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxBody))
		return nil
	}
	dec := json.NewDecoder(io.LimitReader(resp.Body, maxBody))
	if err := dec.Decode(out); err != nil {
		return err
	}
	return nil
}

func (c config) statePath() string { return filepath.Join(c.stateDir, "state.json") }

func (c config) loadState() (state, error) {
	var out state
	if err := decodeStrictFile(c.statePath(), &out); err != nil {
		return state{}, err
	}
	if out.SchemaVersion != stateSchema {
		return state{}, errors.New("acceptance state schema changed")
	}
	return out, nil
}

func (c config) saveState(value state) error {
	value.SchemaVersion = stateSchema
	raw, err := json.Marshal(value)
	if err != nil {
		return err
	}
	return atomicWrite(c.statePath(), raw, 0o600)
}

func holdIncompatible() error {
	requestFile := os.NewFile(3, "acceptance-helper-request")
	if requestFile == nil {
		return errors.New("incompatible helper: inherited request is unavailable")
	}
	defer func() { _ = requestFile.Close() }()
	var launch helperLaunch
	dec := json.NewDecoder(io.LimitReader(requestFile, maxBody))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&launch); err != nil {
		return fmt.Errorf("incompatible helper: decode inherited request: %w", err)
	}
	var extra any
	if err := dec.Decode(&extra); !errors.Is(err, io.EOF) {
		return errors.New("incompatible helper: inherited request has trailing JSON")
	}
	orgID, sessionID, shimID := launch.OrgID, launch.SessionID, launch.ShimID
	processEpoch := launch.ProcessEpoch
	if processEpoch == 0 {
		return errors.New("incompatible helper: invalid process epoch")
	}
	registryDir := launch.RegistryDir
	recordPath := launch.RecordPath
	socketPath := launch.SocketPath
	readyPath := launch.ReadyPath
	stopPath := launch.StopPath
	for _, value := range []string{registryDir, recordPath, socketPath, readyPath, stopPath} {
		if !filepath.IsAbs(value) {
			return errors.New("incompatible helper: paths must be absolute")
		}
	}
	if filepath.Dir(recordPath) != filepath.Clean(registryDir) || filepath.Dir(socketPath) != filepath.Clean(registryDir) {
		return errors.New("incompatible helper: record/socket escaped registry")
	}
	if filepath.Dir(readyPath) != filepath.Dir(stopPath) {
		return errors.New("incompatible helper: ready/stop state roots differ")
	}
	registry, err := sessionshim.NewRegistry(registryDir)
	if err != nil {
		return err
	}
	registryRoot, err := os.OpenRoot(registry.Dir())
	if err != nil {
		return err
	}
	defer func() { _ = registryRoot.Close() }()
	listener, err := net.Listen("unix", socketPath)
	if err != nil {
		return err
	}
	socketName := filepath.Base(socketPath)
	defer func() { _ = listener.Close(); _ = registryRoot.Remove(socketName) }()
	if err := registryRoot.Chmod(socketName, 0o600); err != nil {
		return err
	}
	harness := exec.Command("sleep", helperHarnessLifetime)
	harness.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := harness.Start(); err != nil {
		return err
	}
	// Pin the harness NOW, while it is running. The tombstone this helper must
	// leave behind names WHICH process was reaped, and a start time is
	// unreadable once the process is gone — a tombstone written from a bare pid
	// could not tell "this group is gone" from "a new process reused the pid".
	harnessIdentity, identityErr := sessionshim.ProcessIdentityFor(harness.Process.Pid)
	if identityErr != nil {
		reapProcessGroup(harness)
		return fmt.Errorf("incompatible helper: pin harness process identity: %w", identityErr)
	}
	reaped := false
	defer func() {
		if !reaped {
			reapProcessGroup(harness)
		}
	}()
	self, err := sessionshim.Self()
	if err != nil {
		return err
	}
	socketInfo, err := registryRoot.Stat(socketName)
	if err != nil {
		return err
	}
	stat, ok := socketInfo.Sys().(*syscall.Stat_t)
	if !ok {
		return errors.New("incompatible helper: socket identity is unavailable")
	}
	device, err := strconv.ParseUint(fmt.Sprint(stat.Dev), 10, 64)
	if err != nil {
		return errors.New("incompatible helper: socket device is invalid")
	}
	inode, err := strconv.ParseUint(fmt.Sprint(stat.Ino), 10, 64)
	if err != nil {
		return errors.New("incompatible helper: socket inode is invalid")
	}
	stateRoot, err := os.OpenRoot(filepath.Dir(readyPath))
	if err != nil {
		return err
	}
	defer func() { _ = stateRoot.Close() }()
	record := sessionshim.Record{
		SchemaVersion: sessionshim.RecordSchemaVersion,
		OrgID:         orgID, SessionID: sessionID, ShimID: shimID, ProcessEpoch: processEpoch,
		PID: self.PID, ProcessStartedAt: self.StartedAt,
		SocketPath: socketPath, SocketDevice: device, SocketInode: inode,
		ProtocolMin: shimwire.ProtocolMax + 1, ProtocolMax: shimwire.ProtocolMax + 1,
		Phase: shimwire.PhaseRunning, CreatedAtUnixNano: time.Now().UnixNano(),
	}
	if err := record.Validate(); err != nil {
		return err
	}
	raw, err := json.Marshal(record)
	if err != nil {
		return err
	}
	if err := atomicWrite(recordPath, raw, sessionshim.RecordFileMode); err != nil {
		return err
	}
	// The record is withdrawn by PutTombstone — proof first, liveness claim
	// second — so there is no unconditional removal defer here. But every path
	// that fails BEFORE the terminal publication must still take it back:
	// leaving a record for a process that is about to exit would publish a
	// lineage that is merely unobservable, which is not evidence of anything
	// (§D10) and which the daemon's clear then refuses forever.
	recordPublished := true
	defer func() {
		if recordPublished {
			_ = registryRoot.Remove(filepath.Base(recordPath))
		}
	}()
	ready := helperState{
		OrgID: orgID, SessionID: sessionID, ShimID: shimID, ProcessEpoch: processEpoch,
		PID: self.PID, ProcessStartedAt: self.StartedAt,
		HarnessPID: harnessIdentity.PID, HarnessStartedAt: harnessIdentity.StartedAt,
		RecordPath: recordPath, SocketPath: socketPath, StopPath: stopPath,
	}
	readyRaw, _ := json.Marshal(ready)
	if err := atomicWrite(readyPath, readyRaw, 0o600); err != nil {
		return err
	}
	defer func() { _ = stateRoot.Remove(filepath.Base(readyPath)) }()

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGTERM, syscall.SIGINT)
	defer signal.Stop(sig)
	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()
	held := true
	for held {
		select {
		case <-sig:
			held = false
		case <-ticker.C:
			if _, err := stateRoot.Lstat(filepath.Base(stopPath)); err == nil {
				_ = stateRoot.Remove(filepath.Base(stopPath))
				held = false
			}
		}
	}
	reaped = true
	// PutTombstone owns the record from here: on success it removes it after
	// the proof is durable, and on failure it must STAY so the daemon's clear
	// refuses rather than accepting an unobservable lineage.
	recordPublished = false
	return publishHelperTombstone(registry, record, harness, harnessIdentity)
}

// publishHelperTombstone runs the terminal half of this lineage: reap the
// harness process GROUP, ask the OS whether the exact recorded incarnation is
// really gone, and durably publish what was observed.
//
// Every field is measured. GroupReaped in particular is the answer to a live
// liveness probe and never a constant: a tombstone that claims a reap it did
// not verify is worse than no tombstone at all, because §D10 lets a proven one
// release a claim.
func publishHelperTombstone(
	registry *sessionshim.Registry,
	record sessionshim.Record,
	harness *exec.Cmd,
	harnessIdentity sessionshim.ProcessIdentity,
) error {
	state := reapProcessGroup(harness)
	alive, aliveErr := harnessIdentity.Alive()
	// Two questions, both asked of the OS: is the recorded LEADER incarnation
	// gone, and is the process GROUP gone? A leader can be reaped while a
	// grandchild it forked keeps the group alive, and §D8 promises the group.
	groupGone := errors.Is(syscall.Kill(-harnessIdentity.PID, 0), syscall.ESRCH)
	exitCode, signalName := helperTerminalOutcome(state)
	tombstone := sessionshim.Tombstone{
		SchemaVersion:    sessionshim.RecordSchemaVersion,
		OrgID:            record.OrgID,
		SessionID:        record.SessionID,
		ShimID:           record.ShimID,
		ProcessEpoch:     record.ProcessEpoch,
		HarnessPID:       harnessIdentity.PID,
		HarnessStartedAt: harnessIdentity.StartedAt,
		ExitCode:         exitCode,
		Signal:           signalName,
		// This lineage owns no PTY and allocates no host output sequence, so
		// the final sequence it allocated is zero. That is the measured value,
		// not a placeholder for one.
		LastSeq:            0,
		GroupReaped:        aliveErr == nil && !alive && groupGone,
		ObservedAtUnixNano: time.Now().UnixNano(),
	}
	if err := registry.PutTombstone(tombstone); err != nil {
		return fmt.Errorf("incompatible helper: publish terminal tombstone: %w", err)
	}
	_, _ = fmt.Fprintf(os.Stderr, "%s: session=%s shim=%s harnessPid=%d groupReaped=%t\n",
		helperTombstoneMarker, record.SessionID, record.ShimID, tombstone.HarnessPID, tombstone.GroupReaped)
	return nil
}

// helperTerminalOutcome renders a collected child's wait status in the §12.2
// vocabulary the product's own Exit payload uses: signal death carries the
// signal name and exitCode = 128 + signum.
func helperTerminalOutcome(state *os.ProcessState) (uint64, string) {
	if state == nil {
		return 0, ""
	}
	if status, ok := state.Sys().(syscall.WaitStatus); ok && status.Signaled() {
		signum := int(status.Signal())
		return attachwire.ExitCodeForSignal(signum), attachwire.SignalName(signum)
	}
	code := state.ExitCode()
	if code < 0 {
		return 0, ""
	}
	return uint64(code), ""
}

// reapProcessGroup runs the bounded teardown a shim runs on its own harness:
// SIGTERM to the process GROUP, a grace window, then SIGKILL — and a wait that
// actually collects the child, so the caller's liveness probe is answering
// about a reaped process rather than a zombie.
func reapProcessGroup(cmd *exec.Cmd) *os.ProcessState {
	if cmd == nil || cmd.Process == nil {
		return nil
	}
	_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM)
	done := make(chan struct{})
	go func() { _ = cmd.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(helperTerminationGrace):
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		<-done
	}
	return cmd.ProcessState
}

func setServiceEnvironment(name, value string) error {
	switch runtime.GOOS {
	case "linux":
		return runProcess("systemctl", "--user", "set-environment", name+"="+value)
	case "darwin":
		return runProcess("launchctl", "setenv", name, value)
	default:
		return errors.New("unsupported service manager")
	}
}

func unsetServiceEnvironment(name string) error {
	switch runtime.GOOS {
	case "linux":
		return runProcess("systemctl", "--user", "unset-environment", name)
	case "darwin":
		return runProcess("launchctl", "unsetenv", name)
	default:
		return nil
	}
}

func runProcess(name string, args ...string) error {
	path, err := exec.LookPath(name)
	if err != nil {
		return err
	}
	devnull, err := os.OpenFile(os.DevNull, os.O_RDWR, 0)
	if err != nil {
		return err
	}
	defer func() { _ = devnull.Close() }()
	process, err := os.StartProcess(path, append([]string{name}, args...), &os.ProcAttr{
		Env: os.Environ(), Files: []*os.File{devnull, devnull, devnull},
	})
	if err != nil {
		return err
	}
	state, err := process.Wait()
	if err != nil {
		return err
	}
	if !state.Success() {
		return fmt.Errorf("%s exited unsuccessfully", name)
	}
	return nil
}

func startDetachedSelf(launch helperLaunch) (*os.Process, error) {
	requestReader, requestWriter, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	requestRaw, err := json.Marshal(launch)
	if err != nil {
		_ = requestReader.Close()
		_ = requestWriter.Close()
		return nil, err
	}
	if _, err := requestWriter.Write(requestRaw); err != nil {
		_ = requestReader.Close()
		_ = requestWriter.Close()
		return nil, err
	}
	if err := requestWriter.Close(); err != nil {
		_ = requestReader.Close()
		return nil, err
	}
	defer func() { _ = requestReader.Close() }()
	devnull, err := os.OpenFile(os.DevNull, os.O_RDWR, 0)
	if err != nil {
		return nil, err
	}
	defer func() { _ = devnull.Close() }()
	process, err := os.StartProcess("/proc/self/exe", []string{"/proc/self/exe", "__hold-incompatible"}, &os.ProcAttr{
		Files: []*os.File{devnull, devnull, devnull, requestReader},
	})
	if err != nil {
		return nil, err
	}
	return process, nil
}

func acceptanceTokenPathEnvironment() string {
	return strings.Join([]string{"DONMAI", "SESSION", "SHIM", "ACCEPTANCE", "TOKEN", "FILE"}, "_")
}

func requireEmptyOrAbsent(path string) error {
	entries, err := os.ReadDir(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if len(entries) != 0 {
		return errors.New("acceptance state directory is not empty")
	}
	return nil
}

func atomicWrite(path string, raw []byte, mode os.FileMode) error {
	dir := filepath.Dir(path)
	root, err := os.OpenRoot(dir)
	if err != nil {
		return err
	}
	defer func() { _ = root.Close() }()
	nonce, err := randomHex(8)
	if err != nil {
		return err
	}
	name := ".acceptance-" + nonce
	tmp, err := root.OpenFile(name, os.O_CREATE|os.O_EXCL|os.O_WRONLY, mode)
	if err != nil {
		return err
	}
	defer func() { _ = root.Remove(name) }()
	if err := tmp.Chmod(mode); err != nil {
		_ = tmp.Close()
		return err
	}
	if _, err := tmp.Write(raw); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := root.Rename(name, filepath.Base(path)); err != nil {
		return err
	}
	d, err := root.Open(".")
	if err == nil {
		_ = d.Sync()
		_ = d.Close()
	}
	return nil
}

func decodeStrictFile(path string, out any) error {
	root, err := os.OpenRoot(filepath.Dir(path))
	if err != nil {
		return err
	}
	defer func() { _ = root.Close() }()
	raw, err := root.ReadFile(filepath.Base(path))
	if err != nil {
		return err
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(out); err != nil {
		return err
	}
	var extra any
	if err := dec.Decode(&extra); !errors.Is(err, io.EOF) {
		return errors.New("acceptance state has trailing JSON")
	}
	return nil
}

func waitFor(ctx context.Context, interval time.Duration, fn func() error) error {
	var last error
	for {
		err := fn()
		if err == nil {
			return nil
		}
		last = err
		select {
		case <-ctx.Done():
			return fmt.Errorf("timed out: %w", last)
		case <-time.After(interval):
		}
	}
}

func randomHex(size int) (string, error) {
	raw := make([]byte, size)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	return hex.EncodeToString(raw), nil
}

func processAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	return syscall.Kill(pid, 0) == nil
}

func pathExists(path string) bool {
	root, err := os.OpenRoot(filepath.Dir(path))
	if err != nil {
		return false
	}
	defer func() { _ = root.Close() }()
	_, err = root.Lstat(filepath.Base(path))
	return err == nil
}

func ensurePrivateDir(path string) error {
	parent, err := os.OpenRoot(filepath.Dir(path))
	if err != nil {
		return err
	}
	defer func() { _ = parent.Close() }()
	name := filepath.Base(path)
	if err := parent.Mkdir(name, 0o700); err != nil && !errors.Is(err, os.ErrExist) {
		return err
	}
	return parent.Chmod(name, 0o700)
}

func usage(command string) error { return fmt.Errorf("%s: wrong argument count", command) }
