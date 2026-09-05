package stubagent

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"time"
)

// CommandName is the argv the fake agent answers on. It is declared here,
// beside the environment contract it reads, so the harness that spawns the
// child and the CLI that registers it cannot drift apart.
const CommandName = "stub-agent"

// ScenarioVersion is the only scenario schema version this build accepts. A
// scenario carrying any other version is refused rather than best-effort
// decoded: an integration environment that silently ran a scenario it did not
// understand would report a green it never proved.
const ScenarioVersion = 1

// Environment variables the child reads. They are the whole configuration
// surface — the child takes no flags, so a scenario survives every layer that
// forwards agent.Spec.Env without any of them learning a new field.
const (
	// EnvScenario carries an inline JSON scenario. It wins over EnvScenarioFile.
	EnvScenario = "DONMAI_STUB_SCENARIO"
	// EnvScenarioFile names a file holding a JSON scenario.
	EnvScenarioFile = "DONMAI_STUB_SCENARIO_FILE"
	// EnvExitCode overrides Scenario.ExitCode.
	EnvExitCode = "DONMAI_STUB_EXIT_CODE"
	// EnvHangFor overrides Scenario.HangFor.
	EnvHangFor = "DONMAI_STUB_HANG_FOR"
	// EnvStopMode overrides Scenario.Stop.Mode.
	EnvStopMode = "DONMAI_STUB_STOP_MODE"
	// EnvOutputRate overrides Scenario.OutputRate (bytes per second).
	EnvOutputRate = "DONMAI_STUB_OUTPUT_RATE"
	// EnvSeed overrides Scenario.Seed.
	EnvSeed = "DONMAI_STUB_SEED"
)

// HangForever is the EnvHangFor / Scenario.HangFor value meaning "idle until
// something kills me". It is spelled out rather than encoded as a sentinel
// duration so a scenario file reads as what it does.
const HangForever = "forever"

// StopMode says how the child answers a cooperative stop (SIGTERM). It is the
// knob that separates a harness that shuts down when asked from one that has
// to be killed — the distinction the daemon's stop path exists to report.
type StopMode string

// Stop modes.
const (
	// StopRespond acknowledges the signal and exits. The default.
	StopRespond StopMode = "respond"
	// StopIgnore observes the signal and keeps running, forcing the parent's
	// grace window to expire and escalate to SIGKILL. This is the RED control
	// for any assertion that a cooperative stop succeeded: with it, a stop
	// that reports success is reporting something it did not observe.
	StopIgnore StopMode = "ignore"
	// StopSlow acknowledges the signal, waits StopPolicy.Delay, then exits —
	// the boundary case, used to prove a grace window is the length it claims.
	StopSlow StopMode = "slow"
)

// Duration is a time.Duration that marshals as a Go duration STRING ("250ms",
// "3s"). time.Duration's own JSON form is an integer count of nanoseconds,
// which makes a hand-written scenario file both unreadable and easy to get
// wrong by three orders of magnitude.
type Duration time.Duration

// MarshalJSON encodes the duration as its Go string form.
func (d Duration) MarshalJSON() ([]byte, error) {
	return json.Marshal(time.Duration(d).String())
}

// UnmarshalJSON accepts the Go duration string form. A JSON number is
// refused: it would silently be read as nanoseconds.
func (d *Duration) UnmarshalJSON(data []byte) error {
	var text string
	if err := json.Unmarshal(data, &text); err != nil {
		return fmt.Errorf("stub scenario: duration must be a string such as \"250ms\": %w", err)
	}
	parsed, err := time.ParseDuration(text)
	if err != nil {
		return fmt.Errorf("stub scenario: parse duration %q: %w", text, err)
	}
	*d = Duration(parsed)
	return nil
}

// Duration returns the value as a time.Duration.
func (d Duration) Duration() time.Duration { return time.Duration(d) }

// A2ADirective is one agent-to-agent line the scenario emits on stdout. It is
// deliberately a small projection of the real a2a.Message rather than the
// whole type: a scenario author supplies content, and the encoder supplies
// the protocol shape and the deterministic identifier (see EncodeA2ALine).
type A2ADirective struct {
	// Role is "ROLE_AGENT" (default) or "ROLE_USER".
	Role string `json:"role,omitempty"`
	// Text is the message body. Required.
	Text string `json:"text"`
	// ContextID and TaskID are threaded onto the message unchanged.
	ContextID string `json:"contextId,omitempty"`
	TaskID    string `json:"taskId,omitempty"`
}

// AwaitInput blocks the script until a line arrives on stdin. It is how a
// scenario exercises the PTY's INPUT direction — a prompt seed, an injected
// notice, an operator's keystrokes — instead of only its output.
type AwaitInput struct {
	// Timeout bounds the wait. Zero means DefaultAwaitTimeout. A wait that
	// expires is a scenario failure, not a silent continue: the caller asked
	// to prove input arrived.
	Timeout Duration `json:"timeout,omitempty"`
	// Echo prints the received line back, prefixed with EchoPrefix.
	Echo bool `json:"echo,omitempty"`
}

// Step is exactly one scenario action. Exactly one field must be set;
// Validate refuses a step that sets none or several, because a step that
// silently picks one of two requested actions is a scenario that does not
// mean what it says.
type Step struct {
	// Print writes one line to stdout.
	Print *string `json:"print,omitempty"`
	// Idle sleeps.
	Idle *Duration `json:"idle,omitempty"`
	// A2A emits one agent-to-agent line.
	A2A *A2ADirective `json:"a2a,omitempty"`
	// AwaitInput blocks on stdin.
	AwaitInput *AwaitInput `json:"awaitInput,omitempty"`
	// Exit terminates the run immediately with this code, skipping every
	// later step and Scenario.HangFor.
	Exit *int `json:"exit,omitempty"`
	// Hang idles forever. Only a stop (or the parent's SIGKILL) ends it.
	// An explicit false is a deliberate no-op step, not an error: it lets a
	// scenario file be edited to disable one hang without renumbering the
	// rest of the script.
	Hang *bool `json:"hang,omitempty"`
}

// StopPolicy is how the child answers a cooperative stop.
type StopPolicy struct {
	Mode StopMode `json:"mode,omitempty"`
	// Delay is how long StopSlow waits before exiting. Ignored otherwise.
	Delay Duration `json:"delay,omitempty"`
	// ExitCode is the status StopRespond and StopSlow exit with.
	ExitCode int `json:"exitCode,omitempty"`
	// Print is the line written when the stop is OBSERVED — written by every
	// mode including StopIgnore, so a transcript distinguishes "the signal
	// never arrived" from "the signal arrived and was ignored". Empty uses
	// DefaultStopNotice.
	Print string `json:"print,omitempty"`
}

// Scenario is the whole deterministic script.
type Scenario struct {
	Version int    `json:"version"`
	Name    string `json:"name,omitempty"`
	// Seed makes emitted identifiers reproducible. It contributes no
	// randomness — see EncodeA2ALine.
	Seed  int64      `json:"seed,omitempty"`
	Steps []Step     `json:"steps,omitempty"`
	Stop  StopPolicy `json:"stop,omitempty"`
	// ExitCode is the status after the last step. Default 0.
	ExitCode int `json:"exitCode,omitempty"`
	// HangFor idles after the last step and before exiting: a Go duration
	// string, or HangForever. Empty means exit immediately.
	HangFor string `json:"hangFor,omitempty"`
	// OutputRate throttles stdout to this many bytes per second. Zero is
	// unthrottled. It exists to reproduce a slow writer without needing a
	// slow machine.
	OutputRate int `json:"outputRate,omitempty"`
}

// DefaultScenario is what the child runs when the environment configures
// nothing: announce itself, pause briefly so an attach has something to
// observe, exit clean. It is a real session, just an uneventful one.
func DefaultScenario() Scenario {
	banner := "stub agent ready"
	idle := Duration(250 * time.Millisecond)
	return Scenario{
		Version: ScenarioVersion,
		Name:    "default",
		Steps:   []Step{{Print: &banner}, {Idle: &idle}},
		Stop:    StopPolicy{Mode: StopRespond},
	}
}

// Parse decodes and validates a JSON scenario. Unknown fields are refused:
// a scenario naming a knob this build does not have would otherwise run as
// though the knob were off, which is the failure mode this whole harness
// exists to make impossible.
func Parse(data []byte) (Scenario, error) {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	var scenario Scenario
	if err := decoder.Decode(&scenario); err != nil {
		return Scenario{}, fmt.Errorf("stub scenario: decode: %w", err)
	}
	// Refuse trailing content for the same reason unknown fields are refused:
	// a file holding two scenarios would otherwise run the first and drop the
	// second without saying so.
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		return Scenario{}, errors.New("stub scenario: trailing content after the scenario object")
	}
	if err := scenario.Validate(); err != nil {
		return Scenario{}, err
	}
	return scenario, nil
}

// ErrNoScenario reports that the environment configured no scenario. Load
// never returns it — it falls back to DefaultScenario — but callers that want
// to distinguish "configured" from "defaulted" can compare against it via
// LoadStrict.
var ErrNoScenario = errors.New("stub scenario: none configured")

// Load resolves the scenario from the environment, then applies the env-only
// fault overrides. getenv is the lookup (os.Getenv in production, a map in
// tests). Precedence, highest first:
//
//	DONMAI_STUB_SCENARIO       inline JSON
//	DONMAI_STUB_SCENARIO_FILE  path to JSON
//	DefaultScenario()
//
// The override variables (exit code, hang, stop mode, output rate, seed) are
// applied on top of whichever of those supplied the base, so a single
// scenario file can be reused across fault cases without being rewritten.
func Load(getenv func(string) string) (Scenario, error) {
	scenario, err := LoadStrict(getenv)
	if errors.Is(err, ErrNoScenario) {
		scenario = DefaultScenario()
	} else if err != nil {
		return Scenario{}, err
	}
	return applyOverrides(scenario, getenv)
}

// LoadStrict is Load without the default: it returns ErrNoScenario when
// neither scenario variable is set, and applies no overrides.
func LoadStrict(getenv func(string) string) (Scenario, error) {
	if inline := strings.TrimSpace(getenv(EnvScenario)); inline != "" {
		return Parse([]byte(inline))
	}
	if path := strings.TrimSpace(getenv(EnvScenarioFile)); path != "" {
		data, err := os.ReadFile(path) //nolint:gosec // the path is the operator's own scenario file
		if err != nil {
			return Scenario{}, fmt.Errorf("stub scenario: read %s: %w", EnvScenarioFile, err)
		}
		return Parse(data)
	}
	return Scenario{}, ErrNoScenario
}

func applyOverrides(scenario Scenario, getenv func(string) string) (Scenario, error) {
	if raw := strings.TrimSpace(getenv(EnvExitCode)); raw != "" {
		code, err := strconv.Atoi(raw)
		if err != nil {
			return Scenario{}, fmt.Errorf("stub scenario: parse %s=%q: %w", EnvExitCode, raw, err)
		}
		scenario.ExitCode = code
	}
	if raw := strings.TrimSpace(getenv(EnvHangFor)); raw != "" {
		scenario.HangFor = raw
	}
	if raw := strings.TrimSpace(getenv(EnvStopMode)); raw != "" {
		scenario.Stop.Mode = StopMode(raw)
	}
	if raw := strings.TrimSpace(getenv(EnvOutputRate)); raw != "" {
		rate, err := strconv.Atoi(raw)
		if err != nil {
			return Scenario{}, fmt.Errorf("stub scenario: parse %s=%q: %w", EnvOutputRate, raw, err)
		}
		scenario.OutputRate = rate
	}
	if raw := strings.TrimSpace(getenv(EnvSeed)); raw != "" {
		seed, err := strconv.ParseInt(raw, 10, 64)
		if err != nil {
			return Scenario{}, fmt.Errorf("stub scenario: parse %s=%q: %w", EnvSeed, raw, err)
		}
		scenario.Seed = seed
	}
	if err := scenario.Validate(); err != nil {
		return Scenario{}, err
	}
	return scenario, nil
}

// Validate reports the first structural problem with a scenario.
func (s Scenario) Validate() error {
	if s.Version != ScenarioVersion {
		return fmt.Errorf("stub scenario: version %d is not the supported version %d", s.Version, ScenarioVersion)
	}
	if s.OutputRate < 0 {
		return fmt.Errorf("stub scenario: outputRate %d is negative", s.OutputRate)
	}
	if _, _, err := s.HangDuration(); err != nil {
		return err
	}
	switch s.Stop.Mode {
	case "", StopRespond, StopIgnore, StopSlow:
	default:
		return fmt.Errorf("stub scenario: unknown stop mode %q (want %q, %q or %q)",
			s.Stop.Mode, StopRespond, StopIgnore, StopSlow)
	}
	if s.Stop.Delay < 0 {
		return fmt.Errorf("stub scenario: stop delay %s is negative", s.Stop.Delay.Duration())
	}
	for i, step := range s.Steps {
		if err := step.validate(); err != nil {
			return fmt.Errorf("stub scenario: step %d: %w", i, err)
		}
	}
	return nil
}

// HangDuration resolves HangFor. The second result distinguishes the three
// cases the string encodes: (0, false, nil) exit now, (d, false, nil) idle d,
// (0, true, nil) idle forever.
func (s Scenario) HangDuration() (time.Duration, bool, error) {
	trimmed := strings.TrimSpace(s.HangFor)
	switch trimmed {
	case "":
		return 0, false, nil
	case HangForever:
		return 0, true, nil
	}
	parsed, err := time.ParseDuration(trimmed)
	if err != nil {
		return 0, false, fmt.Errorf("stub scenario: parse hangFor %q: %w", s.HangFor, err)
	}
	if parsed < 0 {
		return 0, false, fmt.Errorf("stub scenario: hangFor %q is negative", s.HangFor)
	}
	return parsed, false, nil
}

// StopModeOrDefault resolves the empty StopMode to StopRespond.
func (s Scenario) StopModeOrDefault() StopMode {
	if s.Stop.Mode == "" {
		return StopRespond
	}
	return s.Stop.Mode
}

func (st Step) validate() error {
	set := make([]string, 0, 6)
	if st.Print != nil {
		set = append(set, "print")
	}
	if st.Idle != nil {
		set = append(set, "idle")
		if st.Idle.Duration() < 0 {
			return fmt.Errorf("idle %s is negative", st.Idle.Duration())
		}
	}
	if st.A2A != nil {
		set = append(set, "a2a")
		if strings.TrimSpace(st.A2A.Text) == "" {
			return errors.New("a2a text is required")
		}
		if err := validateRole(st.A2A.Role); err != nil {
			return err
		}
	}
	if st.AwaitInput != nil {
		set = append(set, "awaitInput")
		if st.AwaitInput.Timeout.Duration() < 0 {
			return fmt.Errorf("awaitInput timeout %s is negative", st.AwaitInput.Timeout.Duration())
		}
	}
	if st.Exit != nil {
		set = append(set, "exit")
	}
	if st.Hang != nil {
		set = append(set, "hang")
	}
	switch len(set) {
	case 1:
		return nil
	case 0:
		return errors.New("no action set (want exactly one of print, idle, a2a, awaitInput, exit, hang)")
	default:
		return fmt.Errorf("sets %d actions (%s); exactly one is allowed", len(set), strings.Join(set, ", "))
	}
}
