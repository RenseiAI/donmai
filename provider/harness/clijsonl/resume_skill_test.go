package clijsonl

import (
	"errors"
	"slices"
	"sync/atomic"
	"testing"

	"github.com/RenseiAI/donmai/agent"
)

// The skill plugin directory rides every --resume turn beside the routing
// pin. These tests pin the admission: the harness's own plugin directory
// (absolute, clean, reserved name) passes, alone or after the pin; anything
// else vetoes the whole fragment, exactly like a malformed pin.

func TestResumeArgv_AdmitsSkillPluginDir(t *testing.T) {
	t.Parallel()
	pin := `{"env":{"ANTHROPIC_BASE_URL":"http://127.0.0.1:1/v1"}}`
	dir := "/tmp/donmai-skills-1234"
	cases := []struct {
		name  string
		extra []string
		want  []string
	}{
		{
			"plugin dir alone",
			[]string{"--plugin-dir", dir},
			[]string{"-p", "--output-format", "stream-json", "--verbose", "--dangerously-skip-permissions", "--plugin-dir", dir, "--resume", "sess-1"},
		},
		{
			"pin then plugin dir",
			[]string{"--settings", pin, "--plugin-dir", dir},
			[]string{"-p", "--output-format", "stream-json", "--verbose", "--dangerously-skip-permissions", "--settings", pin, "--plugin-dir", dir, "--resume", "sess-1"},
		},
		{
			"plugin dir then pin is reordered pin first",
			[]string{"--plugin-dir", dir, "--settings", pin},
			[]string{"-p", "--output-format", "stream-json", "--verbose", "--dangerously-skip-permissions", "--settings", pin, "--plugin-dir", dir, "--resume", "sess-1"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			argv, _ := resumeArgv("sess-1", "follow up", tc.extra)
			if !slices.Equal(argv, tc.want) {
				t.Fatalf("resume argv = %q, want %q", argv, tc.want)
			}
		})
	}
}

func TestResumeArgv_VetoesForeignPluginDirs(t *testing.T) {
	t.Parallel()
	pin := `{"env":{"ANTHROPIC_BASE_URL":"http://127.0.0.1:1/v1"}}`
	cases := map[string][]string{
		"relative dir":       {"--plugin-dir", "donmai-skills-1"},
		"unreserved name":    {"--plugin-dir", "/tmp/evil-plugin"},
		"unclean path":       {"--plugin-dir", "/tmp/x/../donmai-skills-1"},
		"two plugin dirs":    {"--plugin-dir", "/tmp/donmai-skills-1", "--plugin-dir", "/tmp/donmai-skills-2"},
		"dangling flag":      {"--plugin-dir"},
		"foreign with pin":   {"--settings", pin, "--plugin-dir", "/repo/.claude/plugins/x"},
		"plugin dir + model": {"--plugin-dir", "/tmp/donmai-skills-1", "--model", "opus"},
	}
	for name, extra := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			argv, _ := resumeArgv("sess-1", "follow up", extra)
			if slices.Contains(argv, "--plugin-dir") || slices.Contains(argv, "--settings") {
				t.Fatalf("resume argv admits %q: %q", extra, argv)
			}
		})
	}
}

// TestHandle_AddCleanupRunsOnceAtStop proves a registered cleanup runs when
// the handle stops, exactly once, and that a registration after the stop
// runs at once.
func TestHandle_AddCleanupRunsOnceAtStop(t *testing.T) {
	t.Parallel()
	h := &Handle{shutdown: make(chan struct{}), done: make(chan struct{}), events: make(chan agent.Event, 1)}
	close(h.done) // the parent subprocess already exited
	var before, after atomic.Int32
	h.AddCleanup(func() error { before.Add(1); return nil })
	h.AddCleanup(func() error { return errors.New("reported, not fatal") })
	if err := h.Stop(t.Context()); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	_ = h.Stop(t.Context())
	if got := before.Load(); got != 1 {
		t.Fatalf("cleanup ran %d times, want 1", got)
	}
	h.AddCleanup(func() error { after.Add(1); return nil })
	if got := after.Load(); got != 1 {
		t.Fatalf("late cleanup ran %d times, want 1 (immediately)", got)
	}
}
