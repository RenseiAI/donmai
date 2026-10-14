package examples_test

import (
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/RenseiAI/donmai/afclient/repoconfig"
)

// Run the shipped scripts, replacing only their external command boundaries.
// No test starts a daemon, spends vendor quota, or writes to GitHub.
const commandFixture = `fixture() {
local name=$1
shift
printf '%s' "$name" >> "$CALLS"
printf ' <%s>' "$@" >> "$CALLS"
printf '\n' >> "$CALLS"
if [[ $name == donmai && $1 == github ]]; then
  if [[ ${FAIL_ISSUE:-} == "$6" ]]; then return 3; fi
  printf '{"issue":%s,"labels":["donmai"]}\n' "$6"
elif [[ $name == donmai && $1 == arch ]]; then
  if [[ ${EMPTY_SUMMARY:-0} != 1 ]]; then
    printf 'Native drift assessment\nPolicy: no-severity-high\nGated: %s\n' "${ASSESS_STATUS:-0}"
  fi
  return "${ASSESS_STATUS:-0}"
elif [[ $name == gh && $2 == view ]]; then
  if [[ ${FAIL_VIEW:-0} == 1 ]]; then return 4; fi
  printf '{"url":"https://github.com/example/demo/pull/%s","comments":[{"body":"Offline receipt fixture"}]}\n' "$3"
elif [[ $name == gh && $2 == diff ]]; then
  printf 'diff --git a/message.txt b/message.txt\n+Implementation from PR %s\n' "$3"
elif [[ $name == gh && $2 == comment ]]; then
  if [[ ${FAIL_COMMENT:-0} == 1 ]]; then return 5; fi
  printf 'Published fixture comment:\n'
  cat "$5"
else
  echo 'Unexpected fixture command' >&2
  return 9
fi
}
donmai() { fixture donmai "$@"; }
gh() { fixture gh "$@"; }
`

type scriptResult struct {
	output string
	calls  string
	status int
}

func runScript(t *testing.T, script string, args []string, settings ...string) scriptResult {
	t.Helper()
	dir := t.TempDir()
	fixture := filepath.Join(dir, "commands.bash")
	if err := os.WriteFile(fixture, []byte(commandFixture), 0o600); err != nil {
		t.Fatal(err)
	}
	calls := filepath.Join(dir, "calls")
	var cmd *exec.Cmd
	switch script {
	case "backlog-sweep/label.sh":
		cmd = exec.Command("bash", "backlog-sweep/label.sh")
	case "second-opinion/compare.sh":
		cmd = exec.Command("bash", "second-opinion/compare.sh")
	case "drift-check-pr/comment.sh":
		cmd = exec.Command("bash", "drift-check-pr/comment.sh")
	default:
		t.Fatalf("unknown example entry point: %s", script)
	}
	cmd.Args = append(cmd.Args, args...)
	cmd.Env = append(os.Environ(), "BASH_ENV="+fixture, "CALLS="+calls)
	cmd.Env = append(cmd.Env, settings...)
	out, err := cmd.CombinedOutput()
	status := 0
	if err != nil {
		var exit *exec.ExitError
		if !errors.As(err, &exit) {
			t.Fatal(err)
		}
		status = exit.ExitCode()
	}
	log, err := os.ReadFile(calls)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
	return scriptResult{output: string(out), calls: string(log), status: status}
}

func TestBacklogLabelBatch(t *testing.T) {
	for _, tc := range []struct {
		name     string
		args     []string
		settings []string
		status   int
		calls    string
	}{
		{
			"two issues",
			[]string{"example/demo", "42", "43"},
			nil, 0,
			"donmai <github> <add-labels> <--repo> <example/demo> <--number> <42> <--labels> <donmai>\n" +
				"donmai <github> <add-labels> <--repo> <example/demo> <--number> <43> <--labels> <donmai>\n",
		},
		{"validate whole batch", []string{"example/demo", "42", "invalid"}, nil, 2, ""},
		{"missing issues", []string{"example/demo"}, nil, 2, ""},
		{"bad repository", []string{"--repo", "42"}, nil, 2, ""},
		{"zero issue", []string{"example/demo", "0"}, nil, 2, ""},
		{
			"stop on failed label",
			[]string{"example/demo", "42", "43"},
			[]string{"FAIL_ISSUE=42"},
			3,
			"donmai <github> <add-labels> <--repo> <example/demo> <--number> <42> <--labels> <donmai>\n",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := runScript(t, "backlog-sweep/label.sh", tc.args, tc.settings...)
			if got.status != tc.status || got.calls != tc.calls {
				t.Fatalf("status=%d calls=%q; want status=%d calls=%q; output=%s", got.status, got.calls, tc.status, tc.calls, got.output)
			}
		})
	}
}

func TestSecondOpinionComparison(t *testing.T) {
	got := runScript(t, "second-opinion/compare.sh", []string{"example/demo", "44", "45"})
	want := "gh <pr> <view> <44> <--repo> <example/demo> <--json> <url,title,headRefName,body,comments>\n" +
		"gh <pr> <diff> <44> <--repo> <example/demo>\n" +
		"gh <pr> <view> <45> <--repo> <example/demo> <--json> <url,title,headRefName,body,comments>\n" +
		"gh <pr> <diff> <45> <--repo> <example/demo>\n"
	if got.status != 0 || got.calls != want || !strings.Contains(got.output, "Implementation from PR 45") {
		t.Fatalf("comparison: %+v", got)
	}
	for _, args := range [][]string{{"example/demo", "44", "44"}, {"example/demo", "44", "0"}, {"example/demo"}, {"--repo", "44", "45"}} {
		got := runScript(t, "second-opinion/compare.sh", args)
		if got.status != 2 || got.calls != "" {
			t.Fatalf("invalid comparison %v performed work: %+v", args, got)
		}
	}
	got = runScript(t, "second-opinion/compare.sh", []string{"example/demo", "44", "45"}, "FAIL_VIEW=1")
	if got.status != 4 || strings.Contains(got.calls, "<diff>") || strings.Contains(got.calls, "<45>") {
		t.Fatalf("continued after view failed: %+v", got)
	}
}

func TestDriftCommentPublication(t *testing.T) {
	url := "https://github.com/example/demo/pull/44"
	for _, tc := range []struct {
		name     string
		settings []string
		status   int
		publish  bool
	}{
		{"clean", nil, 0, true},
		{"gated still publishes", []string{"ASSESS_STATUS=1"}, 1, true},
		{"fetch error never publishes", []string{"ASSESS_STATUS=2"}, 2, false},
		{"unexpected failure never publishes", []string{"ASSESS_STATUS=7"}, 7, false},
		{"empty summary never publishes", []string{"EMPTY_SUMMARY=1"}, 2, false},
		{"comment failure retained", []string{"FAIL_COMMENT=1"}, 5, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := runScript(t, "drift-check-pr/comment.sh", []string{url}, tc.settings...)
			if got.status != tc.status || strings.Contains(got.calls, "<comment>") != tc.publish {
				t.Fatalf("status=%d publication=%t; want status=%d publication=%t; output=%s", got.status, strings.Contains(got.calls, "<comment>"), tc.status, tc.publish, got.output)
			}
			if !strings.Contains(got.calls, "<--require-diff> <--summary> <--gate-policy> <no-severity-high>") {
				t.Fatalf("assessment was not strict: %s", got.calls)
			}
			if tc.publish && len(tc.settings) == 0 && !strings.Contains(got.output, "Published fixture comment:\nNative drift assessment\nPolicy: no-severity-high\nGated: 0\n") {
				t.Fatalf("multiline summary changed: %s", got.output)
			}
		})
	}
	for _, args := range [][]string{nil, {"https://github.com/example/demo/pull/0"}, {url, url}, {"--help"}} {
		got := runScript(t, "drift-check-pr/comment.sh", args)
		if got.status != 2 || got.calls != "" {
			t.Fatalf("invalid input performed work: %+v", got)
		}
	}
}

func TestRepositoryConfigs(t *testing.T) {
	for _, name := range []string{"backlog-sweep", "second-opinion", "drift-check-pr"} {
		t.Run(name, func(t *testing.T) {
			cfg, err := repoconfig.Load(name)
			if err != nil {
				t.Fatal(err)
			}
			// These host/arch examples must not restrict the standalone
			// orchestrator to a fictitious tracker project or remote.
			if !cfg.IsProjectAllowed("Demo") || cfg.Repository != "" {
				t.Fatalf("unexpected repository restriction: %+v", cfg)
			}
		})
	}
}

func TestRecordedRehearsals(t *testing.T) {
	for _, tc := range []struct {
		name     string
		script   string
		args     []string
		settings []string
		status   int
	}{
		{"backlog-sweep", "label.sh", []string{"example/demo", "42", "43"}, nil, 0},
		{"second-opinion", "compare.sh", []string{"example/demo", "44", "45"}, nil, 0},
		{"drift-check-pr", "comment.sh", []string{"https://github.com/example/demo/pull/44"}, []string{"ASSESS_STATUS=1"}, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := runScript(t, tc.name+"/"+tc.script, tc.args, tc.settings...)
			if got.status != tc.status {
				t.Fatalf("recorded script failed: %+v", got)
			}
			transcript := "OFFLINE REHEARSAL: external commands use fixtures; no live vendor or GitHub run.\n" +
				"$ bash " + tc.script + " " + strings.Join(tc.args, " ") + "\n" + got.output
			if tc.status == 1 {
				transcript += "Exit status: 1 (completed gated assessment; fixture comment published)\n"
			}
			event := []any{0.1, "o", strings.ReplaceAll(transcript, "\n", "\r\n")}
			path := filepath.Join(tc.name, "rehearsal.cast")
			if os.Getenv("DONMAI_RECORD_EXAMPLES") == "1" {
				header, err := json.Marshal(map[string]any{"version": 2, "width": 120, "height": 24, "title": tc.name + " offline script rehearsal"})
				if err != nil {
					t.Fatal(err)
				}
				frame, err := json.Marshal(event)
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, append(append(append(header, '\n'), frame...), '\n'), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			lines := strings.Split(strings.TrimSpace(string(data)), "\n")
			if len(lines) != 2 {
				t.Fatalf("expected asciicast header and event: %d lines", len(lines))
			}
			var header struct {
				Version int `json:"version"`
			}
			if err := json.Unmarshal([]byte(lines[0]), &header); err != nil || header.Version != 2 {
				t.Fatalf("invalid asciicast header: %s", lines[0])
			}
			var recorded []any
			if err := json.Unmarshal([]byte(lines[1]), &recorded); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(recorded, event) {
				t.Fatalf("recording differs from executed script: %v", recorded)
			}
		})
	}
}
