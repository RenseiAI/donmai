package stubagent

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func env(pairs map[string]string) func(string) string {
	return func(key string) string { return pairs[key] }
}

func TestParse(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		input   string
		wantErr string
		check   func(*testing.T, Scenario)
	}{
		{
			name:  "minimal scenario",
			input: `{"version":1}`,
			check: func(t *testing.T, s Scenario) {
				if len(s.Steps) != 0 || s.ExitCode != 0 {
					t.Errorf("got %+v, want an empty exit-0 scenario", s)
				}
				if s.StopModeOrDefault() != StopRespond {
					t.Errorf("StopModeOrDefault() = %q, want %q", s.StopModeOrDefault(), StopRespond)
				}
			},
		},
		{
			name:  "every step kind",
			input: `{"version":1,"name":"n","seed":7,"steps":[{"print":"a"},{"idle":"5ms"},{"a2a":{"text":"hi"}},{"awaitInput":{"timeout":"1s","echo":true}},{"hang":true},{"exit":3}],"stop":{"mode":"slow","delay":"2s","exitCode":9},"exitCode":4,"hangFor":"1s","outputRate":100}`,
			check: func(t *testing.T, s Scenario) {
				if len(s.Steps) != 6 {
					t.Fatalf("len(Steps) = %d, want 6", len(s.Steps))
				}
				if s.Steps[1].Idle.Duration() != 5*time.Millisecond {
					t.Errorf("idle = %s, want 5ms", s.Steps[1].Idle.Duration())
				}
				if s.Stop.Mode != StopSlow || s.Stop.ExitCode != 9 {
					t.Errorf("stop = %+v, want slow/9", s.Stop)
				}
				if s.OutputRate != 100 || s.ExitCode != 4 {
					t.Errorf("outputRate/exitCode = %d/%d, want 100/4", s.OutputRate, s.ExitCode)
				}
			},
		},
		{
			name:    "wrong version",
			input:   `{"version":2}`,
			wantErr: "not the supported version",
		},
		{
			name:    "unknown field",
			input:   `{"version":1,"stpes":[]}`,
			wantErr: "unknown field",
		},
		{
			name:    "duration as a number",
			input:   `{"version":1,"steps":[{"idle":250}]}`,
			wantErr: "duration must be a string",
		},
		{
			name:    "step with no action",
			input:   `{"version":1,"steps":[{}]}`,
			wantErr: "no action set",
		},
		{
			name:    "step with two actions",
			input:   `{"version":1,"steps":[{"print":"a","exit":0}]}`,
			wantErr: "exactly one is allowed",
		},
		{
			name:    "a2a without text",
			input:   `{"version":1,"steps":[{"a2a":{"text":"  "}}]}`,
			wantErr: "a2a text is required",
		},
		{
			name:    "a2a with an unknown role",
			input:   `{"version":1,"steps":[{"a2a":{"text":"x","role":"ROLE_ROBOT"}}]}`,
			wantErr: "unknown a2a role",
		},
		{
			name:    "unknown stop mode",
			input:   `{"version":1,"stop":{"mode":"maybe"}}`,
			wantErr: "unknown stop mode",
		},
		{
			name:    "unparseable hangFor",
			input:   `{"version":1,"hangFor":"a while"}`,
			wantErr: "parse hangFor",
		},
		{
			name:    "trailing content",
			input:   `{"version":1}{"version":1}`,
			wantErr: "trailing content",
		},
		{
			name:    "negative output rate",
			input:   `{"version":1,"outputRate":-1}`,
			wantErr: "is negative",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := Parse([]byte(tc.input))
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("Parse err = %v, want one containing %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("Parse: %v", err)
			}
			if tc.check != nil {
				tc.check(t, got)
			}
		})
	}
}

func TestDurationRoundTrip(t *testing.T) {
	t.Parallel()

	scenario, err := Parse([]byte(`{"version":1,"steps":[{"idle":"1m30s"}]}`))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	encoded, err := scenario.Steps[0].Idle.MarshalJSON()
	if err != nil {
		t.Fatalf("MarshalJSON: %v", err)
	}
	if string(encoded) != `"1m30s"` {
		t.Errorf("MarshalJSON = %s, want \"1m30s\"", encoded)
	}
}

func TestHangDuration(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		hangFor     string
		wantD       time.Duration
		wantForever bool
		wantErr     bool
	}{
		{name: "empty means exit now", hangFor: ""},
		{name: "forever", hangFor: HangForever, wantForever: true},
		{name: "duration", hangFor: "750ms", wantD: 750 * time.Millisecond},
		{name: "negative", hangFor: "-1s", wantErr: true},
		{name: "garbage", hangFor: "soon", wantErr: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			d, forever, err := Scenario{HangFor: tc.hangFor}.HangDuration()
			if tc.wantErr {
				if err == nil {
					t.Fatal("HangDuration err = nil, want an error")
				}
				return
			}
			if err != nil {
				t.Fatalf("HangDuration: %v", err)
			}
			if d != tc.wantD || forever != tc.wantForever {
				t.Errorf("HangDuration = (%s, %v), want (%s, %v)", d, forever, tc.wantD, tc.wantForever)
			}
		})
	}
}

func TestLoadSources(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "scenario.json")
	if err := os.WriteFile(path, []byte(`{"version":1,"name":"from-file"}`), 0o600); err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name     string
		vars     map[string]string
		wantName string
		wantErr  string
	}{
		{
			name:     "no configuration falls back to the default scenario",
			vars:     nil,
			wantName: "default",
		},
		{
			name:     "inline JSON",
			vars:     map[string]string{EnvScenario: `{"version":1,"name":"inline"}`},
			wantName: "inline",
		},
		{
			name:     "file",
			vars:     map[string]string{EnvScenarioFile: path},
			wantName: "from-file",
		},
		{
			name: "inline wins over file",
			vars: map[string]string{
				EnvScenario:     `{"version":1,"name":"inline"}`,
				EnvScenarioFile: path,
			},
			wantName: "inline",
		},
		{
			name:    "missing file",
			vars:    map[string]string{EnvScenarioFile: filepath.Join(dir, "absent.json")},
			wantErr: "read " + EnvScenarioFile,
		},
		{
			name:    "malformed inline JSON",
			vars:    map[string]string{EnvScenario: `{`},
			wantErr: "decode",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := Load(env(tc.vars))
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("Load err = %v, want one containing %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			if got.Name != tc.wantName {
				t.Errorf("Name = %q, want %q", got.Name, tc.wantName)
			}
		})
	}
}

func TestLoadStrictReportsNoScenario(t *testing.T) {
	t.Parallel()

	if _, err := LoadStrict(env(nil)); !errors.Is(err, ErrNoScenario) {
		t.Fatalf("LoadStrict err = %v, want ErrNoScenario", err)
	}
}

func TestLoadOverrides(t *testing.T) {
	t.Parallel()

	base := `{"version":1,"name":"base","seed":1,"exitCode":0,"stop":{"mode":"respond"}}`

	tests := []struct {
		name    string
		vars    map[string]string
		check   func(*testing.T, Scenario)
		wantErr string
	}{
		{
			name: "exit code",
			vars: map[string]string{EnvScenario: base, EnvExitCode: "17"},
			check: func(t *testing.T, s Scenario) {
				if s.ExitCode != 17 {
					t.Errorf("ExitCode = %d, want 17", s.ExitCode)
				}
			},
		},
		{
			name: "hang for",
			vars: map[string]string{EnvScenario: base, EnvHangFor: HangForever},
			check: func(t *testing.T, s Scenario) {
				_, forever, err := s.HangDuration()
				if err != nil || !forever {
					t.Errorf("HangDuration = (_, %v, %v), want forever", forever, err)
				}
			},
		},
		{
			name: "stop mode",
			vars: map[string]string{EnvScenario: base, EnvStopMode: string(StopIgnore)},
			check: func(t *testing.T, s Scenario) {
				if s.StopModeOrDefault() != StopIgnore {
					t.Errorf("stop mode = %q, want %q", s.StopModeOrDefault(), StopIgnore)
				}
			},
		},
		{
			name: "output rate and seed",
			vars: map[string]string{EnvScenario: base, EnvOutputRate: "64", EnvSeed: "-9"},
			check: func(t *testing.T, s Scenario) {
				if s.OutputRate != 64 || s.Seed != -9 {
					t.Errorf("outputRate/seed = %d/%d, want 64/-9", s.OutputRate, s.Seed)
				}
			},
		},
		{
			name:    "unparseable exit code",
			vars:    map[string]string{EnvScenario: base, EnvExitCode: "one"},
			wantErr: EnvExitCode,
		},
		{
			name:    "unparseable seed",
			vars:    map[string]string{EnvScenario: base, EnvSeed: "x"},
			wantErr: EnvSeed,
		},
		{
			name:    "unparseable output rate",
			vars:    map[string]string{EnvScenario: base, EnvOutputRate: "fast"},
			wantErr: EnvOutputRate,
		},
		{
			// An override is not a second chance to be invalid: the result is
			// revalidated, so a bad stop mode is refused at load rather than
			// discovered as a silently-default behaviour at run time.
			name:    "override is revalidated",
			vars:    map[string]string{EnvScenario: base, EnvStopMode: "sideways"},
			wantErr: "unknown stop mode",
		},
		{
			name:    "override applies to the default scenario too",
			vars:    map[string]string{EnvHangFor: "-1s"},
			wantErr: "is negative",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := Load(env(tc.vars))
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("Load err = %v, want one containing %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			tc.check(t, got)
		})
	}
}

func TestDefaultScenarioIsValid(t *testing.T) {
	t.Parallel()

	if err := DefaultScenario().Validate(); err != nil {
		t.Fatalf("DefaultScenario().Validate(): %v", err)
	}
}
