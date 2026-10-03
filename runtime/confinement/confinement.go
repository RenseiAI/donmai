package confinement

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"sync"

	"github.com/RenseiAI/donmai/agent"
)

// BackendName is the ConfinementBackend of the ADR's D1.
type BackendName string

// The backend names. Only macos-seatbelt has an implementation in this
// package today; the others are named for wire completeness.
const (
	BackendMacOSSeatbelt       BackendName = "macos-seatbelt"
	BackendLinuxMountNamespace BackendName = "linux-mount-namespace"
	BackendUserSeparation      BackendName = "user-separation"
)

// WritableClass is one class of the closed writable set (D2).
type WritableClass string

// The four writable classes.
const (
	ClassMutableLeaf  WritableClass = "mutable_leaf"
	ClassHarnessState WritableClass = "harness_state"
	ClassSessionTmp   WritableClass = "session_tmp"
	ClassSessionCache WritableClass = "session_cache"
)

var classOrder = map[WritableClass]int{
	ClassMutableLeaf: 0, ClassHarnessState: 1, ClassSessionTmp: 2, ClassSessionCache: 3,
}

// ProbeSetVersion names the probe set the self-test runs (D1.5). It changes
// whenever a probe is added, removed or its expectation changes, which makes
// every earlier self-test record stale.
const ProbeSetVersion = "executor-confinement-probes-v1"

// Spec is one session's confinement declaration: what the harness process and
// every descendant may write. Everything not named here is read-only to the
// harness (D2).
type Spec struct {
	SessionID   string
	HarnessID   string
	SessionMode agent.PromptSessionMode

	// WorkareaRoot is the session root. The root itself and its reserved
	// metadata are never writable.
	WorkareaRoot string
	// MutableLeaves are the repository leaves declared mutable, each with its
	// own .git directory. Each must sit strictly inside WorkareaRoot.
	MutableLeaves []string
	// HarnessState are the harness's documented per-session state
	// directories. Today they may sit inside a mutable leaf.
	HarnessState []string
	// SessionTmp is the executor-owned per-session temporary directory,
	// outside every repository leaf. TMPDIR, TMP and TEMP are bound to it.
	SessionTmp string
	// Caches are the per-session toolchain caches (D2.2), each bound to its
	// environment variable.
	Caches []Cache
	// ReadOnlyLeaves are the repository leaves declared read-only. They win
	// over any broader rule, even when nested inside a writable root.
	ReadOnlyLeaves []string
	// Protected are paths kept outside the writable set even where they sit
	// inside it, such as runner-injected artifacts beside harness state.
	Protected []string
	// Sockets are the local sockets the adapter declares for the session
	// (its control channel, an agent socket the credentials level grants, the
	// OS resolver). Every other socket outside the writable set is closed.
	Sockets []string
}

// Cache is one per-session toolchain cache bound to an environment variable.
type Cache struct {
	Env string
	Dir string
}

// RuleKind is the closed, backend-neutral composer rule vocabulary (D4.3).
type RuleKind string

// The composer rule kinds. Every kind denies; there is no allow.
const (
	RuleDenyRead          RuleKind = "deny_read"
	RuleDenyWrite         RuleKind = "deny_write"
	RuleDenyServiceLookup RuleKind = "deny_service_lookup"
)

// RuleScope says whether a path rule matches one path or a subtree.
type RuleScope string

// The rule scopes.
const (
	ScopeLiteral RuleScope = "literal"
	ScopeSubtree RuleScope = "subtree"
)

// Rule is one deny-only composer rule.
type Rule struct {
	Kind    RuleKind  `json:"kind"`
	Path    string    `json:"path,omitempty"`
	Scope   RuleScope `json:"scope,omitempty"`
	Service string    `json:"service,omitempty"`
}

// RuleContext is what the composer callback is told about a confined spawn.
// The self-test calls the callback with a probe context.
type RuleContext struct {
	SessionID    string
	HarnessID    string
	SessionMode  agent.PromptSessionMode
	Backend      BackendName
	WorkareaRoot string
}

// ExtraRules is the deny-only composer callback a composing binary sets. It
// is called once per confined spawn and once per self-test mode; its rules
// are appended after the executor's own, so they always win, and a rule the
// backend cannot render refuses the spawn with rule_unrenderable.
type ExtraRules func(RuleContext) []Rule

// Record is the per-session ConfinementRecord (D1.6). It carries no
// credential and no raw path: read-only leaves are named by leaf name only.
type Record struct {
	RecordID           string                  `json:"recordId"`
	Backend            BackendName             `json:"backend"`
	SelfTestDigest     string                  `json:"selfTestDigest"`
	SessionMode        agent.PromptSessionMode `json:"sessionMode"`
	WritableClasses    []WritableClass         `json:"writableClasses"`
	ReadOnlyLeaves     []string                `json:"readOnlyLeaves"`
	ComposerRuleDigest string                  `json:"composerRuleDigest,omitempty"`
	RenderedSpecDigest string                  `json:"renderedSpecDigest"`
}

// Digest is the record's own digest: what an applied receipt carries as the
// evidenceDigest of the executor_os_sandbox layer.
func (r Record) Digest() string {
	raw, err := json.Marshal(r)
	if err != nil {
		return ""
	}
	return digestBytes(raw)
}

// Attestation is the per-harness ConfinementAttestation a host publishes for
// a passing, current self-test (D1.3).
type Attestation struct {
	Backend         BackendName               `json:"backend"`
	BackendVersion  string                    `json:"backendVersion"`
	ProbeSetVersion string                    `json:"probeSetVersion"`
	SelfTestDigest  string                    `json:"selfTestDigest"`
	SessionModes    []agent.PromptSessionMode `json:"sessionModes"`
	TestedAt        string                    `json:"testedAt"`
}

// Options configure a Confiner.
type Options struct {
	// Backend applies the boundary. Nil means no backend exists for this OS:
	// every Prepare refuses with backend_absent. DefaultBackend returns the
	// backend for the running OS.
	Backend Backend
	// ProfileDir is where rendered profiles are written: under the host state
	// home, outside every session root. Required.
	ProfileDir string
	// ExtraRules is the optional deny-only composer callback.
	ExtraRules ExtraRules
	// Home is the operator's home and StateHome the host state home. No
	// writable root may contain either; the self-test probes both.
	Home      string
	StateHome string
	// ExecutableDigest identifies the running executable. A self-test record
	// taken under a different executable is stale.
	ExecutableDigest string
}

// Confiner is the production spawn binding: it renders a session's
// confinement, gates it on a passing self-test and wraps the harness argv.
// Safe for concurrent use.
type Confiner struct {
	opts Options

	mu       sync.Mutex
	selfTest *SelfTestRecord
}

// New validates opts and returns a Confiner. It does not run the self-test.
func New(opts Options) (*Confiner, error) {
	if opts.ProfileDir == "" || !filepath.IsAbs(opts.ProfileDir) {
		return nil, fmt.Errorf("confinement: profile directory must be an absolute path, got %q", opts.ProfileDir)
	}
	if err := os.MkdirAll(opts.ProfileDir, 0o700); err != nil {
		return nil, fmt.Errorf("confinement: create profile directory: %w", err)
	}
	return &Confiner{opts: opts}, nil
}

// BackendName names the configured backend, or "" when none exists.
func (c *Confiner) BackendName() BackendName {
	if c.opts.Backend == nil {
		return ""
	}
	return c.opts.Backend.Name()
}

// SelfTestRecord returns the last self-test record, passing or not.
func (c *Confiner) SelfTestRecord() (SelfTestRecord, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.selfTest == nil {
		return SelfTestRecord{}, false
	}
	return *c.selfTest, true
}

// Attestation returns the per-harness confinement attestation when the last
// self-test passed and is still current, or false.
func (c *Confiner) Attestation() (Attestation, bool) {
	record, ok := c.SelfTestRecord()
	if !ok || !record.Passed {
		return Attestation{}, false
	}
	if err := c.checkCurrent(record); err != nil {
		return Attestation{}, false
	}
	return record.Attestation(), true
}

// Plan is one prepared confinement: a rendered, written profile plus the
// environment bindings and the per-session record.
type Plan struct {
	record  Record
	env     []string
	applied Applied
}

// Command returns argv wrapped in the confinement. A relative argv[0] is
// resolved on the caller's PATH first, so the confined process runs exactly
// the binary the caller would have run.
func (p *Plan) Command(argv []string) ([]string, error) {
	if len(argv) == 0 || argv[0] == "" {
		return nil, errors.New("confinement: empty command")
	}
	binary := argv[0]
	if !filepath.IsAbs(binary) {
		resolved, err := exec.LookPath(binary)
		if err != nil {
			return nil, fmt.Errorf("confinement: resolve %q: %w", binary, err)
		}
		binary = resolved
	}
	inner := append([]string{binary}, argv[1:]...)
	return p.applied.Wrap(inner), nil
}

// Environment returns the KEY=VALUE bindings the confined process needs:
// TMPDIR, TMP and TEMP bound to session_tmp and each cache variable bound to
// its per-session directory. Both spawn paths apply the same bindings.
func (p *Plan) Environment() []string {
	return append([]string(nil), p.env...)
}

// Record returns the per-session confinement record.
func (p *Plan) Record() Record { return p.record }

// Release removes the rendered profile. A process already started under it
// stays confined; the profile is only read at exec.
func (p *Plan) Release() error {
	if p.applied.Release == nil {
		return nil
	}
	return p.applied.Release()
}

// Prepare renders and writes the confinement for spec. It refuses with a
// typed *Error when the confinement cannot be applied: no backend, a nested
// profile, no passing or a stale self-test, an unrepresentable writable set,
// or an unrenderable composer rule. It never returns a weaker plan.
func (c *Confiner) Prepare(spec Spec) (*Plan, error) {
	if err := validMode(spec.SessionMode); err != nil {
		return nil, err
	}
	if c.opts.Backend == nil {
		return nil, refuse(ReasonBackendAbsent, "no confinement backend for this operating system")
	}
	record, ok := c.SelfTestRecord()
	if !ok {
		return nil, refuse(ReasonSelfTestFailed, "no self-test has run on this host")
	}
	if !record.Passed || !containsMode(record.SessionModes, spec.SessionMode) {
		return nil, refuse(ReasonSelfTestFailed, "the self-test did not pass for session mode %s", spec.SessionMode)
	}
	if err := c.checkCurrent(record); err != nil {
		return nil, err
	}
	return c.prepare(spec, record.Digest)
}

// prepare is Prepare without the self-test gate; the self-test drives its
// probe through it, so the probe runs under exactly the production rendering.
func (c *Confiner) prepare(spec Spec, selfTestDigest string) (*Plan, error) {
	if err := validMode(spec.SessionMode); err != nil {
		return nil, err
	}
	backend := c.opts.Backend
	if backend == nil {
		return nil, refuse(ReasonBackendAbsent, "no confinement backend for this operating system")
	}
	if err := backend.Check(); err != nil {
		return nil, err
	}
	resolved, err := resolveSpec(spec, guards{
		home:       c.opts.Home,
		stateHome:  c.opts.StateHome,
		profileDir: c.opts.ProfileDir,
	}, backend.Canonical)
	if err != nil {
		return nil, err
	}
	var rules []Rule
	if c.opts.ExtraRules != nil {
		rules = c.opts.ExtraRules(RuleContext{
			SessionID:    spec.SessionID,
			HarnessID:    spec.HarnessID,
			SessionMode:  spec.SessionMode,
			Backend:      backend.Name(),
			WorkareaRoot: resolved.WorkareaRoot,
		})
	}
	applied, err := backend.Apply(ApplyRequest{Resolved: resolved, Rules: rules, ProfileDir: c.opts.ProfileDir})
	if err != nil {
		return nil, err
	}
	record := Record{
		Backend:            backend.Name(),
		SelfTestDigest:     selfTestDigest,
		SessionMode:        spec.SessionMode,
		WritableClasses:    resolved.classes(),
		ReadOnlyLeaves:     resolved.ReadOnlyLeafNames,
		RenderedSpecDigest: digestBytes(applied.Rendered),
	}
	if len(rules) > 0 {
		raw, err := json.Marshal(rules)
		if err != nil {
			return nil, fmt.Errorf("confinement: encode composer rules: %w", err)
		}
		record.ComposerRuleDigest = digestBytes(raw)
	}
	record.RecordID = recordID(spec.SessionID, record)
	return &Plan{record: record, env: resolved.environment(), applied: applied}, nil
}

func (c *Confiner) checkCurrent(record SelfTestRecord) error {
	current, err := c.fingerprint()
	if err != nil {
		return err
	}
	if record.fingerprint() != current {
		return refuse(ReasonSelfTestStale, "the executable, OS build or backend changed since the last self-test")
	}
	return nil
}

func (c *Confiner) fingerprint() (fingerprint, error) {
	if c.opts.Backend == nil {
		return fingerprint{}, refuse(ReasonBackendAbsent, "no confinement backend for this operating system")
	}
	version, err := c.opts.Backend.Version()
	if err != nil {
		return fingerprint{}, refuse(ReasonBackendAbsent, "backend version: %v", err)
	}
	return fingerprint{
		backend:          c.opts.Backend.Name(),
		backendVersion:   version,
		probeSetVersion:  ProbeSetVersion,
		executableDigest: c.opts.ExecutableDigest,
	}, nil
}

type fingerprint struct {
	backend          BackendName
	backendVersion   string
	probeSetVersion  string
	executableDigest string
}

func validMode(mode agent.PromptSessionMode) error {
	switch mode {
	case agent.PromptModeAutonomous, agent.PromptModeHumanControlled:
		return nil
	default:
		return refuse(ReasonModeUnsupported, "session mode %q", mode)
	}
}

func containsMode(modes []agent.PromptSessionMode, mode agent.PromptSessionMode) bool {
	for _, candidate := range modes {
		if candidate == mode {
			return true
		}
	}
	return false
}

func digestBytes(raw []byte) string {
	sum := sha256.Sum256(raw)
	return "sha256:" + hex.EncodeToString(sum[:])
}

func recordID(sessionID string, record Record) string {
	raw, _ := json.Marshal(struct {
		SessionID string `json:"sessionId"`
		Record    Record `json:"record"`
	}{sessionID, record})
	sum := sha256.Sum256(raw)
	return "cfr_" + hex.EncodeToString(sum[:12])
}

func randomSuffix() string {
	var b [6]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "0"
	}
	return hex.EncodeToString(b[:])
}

func (r *Resolved) classes() []WritableClass {
	seen := map[WritableClass]bool{}
	var classes []WritableClass
	for _, root := range r.Writable {
		if !seen[root.Class] {
			seen[root.Class] = true
			classes = append(classes, root.Class)
		}
	}
	sort.Slice(classes, func(i, j int) bool { return classOrder[classes[i]] < classOrder[classes[j]] })
	return classes
}

func (r *Resolved) environment() []string {
	env := []string{"TMPDIR=" + r.SessionTmp, "TMP=" + r.SessionTmp, "TEMP=" + r.SessionTmp}
	for _, cache := range r.Caches {
		env = append(env, cache.Env+"="+cache.Dir)
	}
	return env
}
