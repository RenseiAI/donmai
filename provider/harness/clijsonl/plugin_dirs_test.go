package clijsonl

import (
	"slices"
	"testing"
)

// TestResumeArgv_PluginDirsRideBetweenPinAndResume proves per-session plugin
// directories are re-passed on every --resume (the CLI does not restore
// session-only plugin dirs on its own). Empty keeps the byte-identical
// shape; a non-absolute or unclean entry is dropped rather than widening
// the plugin surface.
func TestResumeArgv_PluginDirsRideBetweenPinAndResume(t *testing.T) {
	t.Parallel()

	t.Run("empty keeps byte-identical shape", func(t *testing.T) {
		t.Parallel()
		argv, _ := resumeArgv("sess-1", "follow up", nil, nil)
		want := []string{"-p", "--output-format", "stream-json", "--verbose", "--dangerously-skip-permissions", "--resume", "sess-1"}
		if !slices.Equal(argv, want) {
			t.Fatalf("resumeArgv = %q, want %q", argv, want)
		}
		if slices.Contains(argv, "--plugin-dir") {
			t.Fatalf("skill-less resume argv carries --plugin-dir: %q", argv)
		}
	})

	t.Run("plugin dir rides before resume", func(t *testing.T) {
		t.Parallel()
		argv, _ := resumeArgv("sess-1", "follow up", nil, []string{"/tmp/plug-1"})
		idx := slices.Index(argv, "--plugin-dir")
		if idx < 0 || idx+1 >= len(argv) || argv[idx+1] != "/tmp/plug-1" {
			t.Fatalf("resume argv missing --plugin-dir /tmp/plug-1: %q", argv)
		}
		if got := argv[len(argv)-2:]; !slices.Equal(got, []string{"--resume", "sess-1"}) {
			t.Fatalf("resume tail = %q, want --resume last", got)
		}
	})

	t.Run("pin and plugin dir ride together", func(t *testing.T) {
		t.Parallel()
		pin := `{"env":{"ANTHROPIC_BASE_URL":"http://127.0.0.1:1/v1"}}`
		argv, _ := resumeArgv("sess-1", "follow up", []string{"--settings", pin}, []string{"/tmp/plug-2"})
		if _, ok := adjacentValue(argv, "--settings"); !ok {
			t.Fatalf("resume argv drops the pin: %q", argv)
		}
		idx := slices.Index(argv, "--plugin-dir")
		if idx < 0 || argv[idx+1] != "/tmp/plug-2" {
			t.Fatalf("resume argv drops the plugin dir: %q", argv)
		}
		// Order: pin, then plugin dir, then --resume.
		pinIdx := slices.Index(argv, "--settings")
		resumeIdx := slices.Index(argv, "--resume")
		if !(pinIdx < idx && idx < resumeIdx) {
			t.Fatalf("fragment order wrong (want pin < plugin-dir < resume): %q", argv)
		}
	})

	t.Run("unsafe plugin dirs are dropped", func(t *testing.T) {
		t.Parallel()
		for _, dir := range []string{"", "relative/path", "a/../b", "/tmp/./x"} {
			argv, _ := resumeArgv("sess-1", "follow up", nil, []string{dir})
			if slices.Contains(argv, "--plugin-dir") {
				t.Fatalf("resume argv admits unsafe plugin dir %q: %q", dir, argv)
			}
		}
	})
}
