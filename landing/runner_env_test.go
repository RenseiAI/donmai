package landing

import "testing"

func TestGitNonInteractiveEnv(t *testing.T) {
	has := func(env []string, want string) bool {
		for _, e := range env {
			if e == want {
				return true
			}
		}
		return false
	}
	t.Run("git appends non-interactive auth vars", func(t *testing.T) {
		got := gitNonInteractiveEnv("git", nil)
		for _, want := range []string{"GIT_TERMINAL_PROMPT=0", "GCM_INTERACTIVE=never"} {
			if !has(got, want) {
				t.Errorf("gitNonInteractiveEnv(git): missing %q in %v", want, got)
			}
		}
	})
	t.Run("git preserves caller extraEnv", func(t *testing.T) {
		if got := gitNonInteractiveEnv("git", []string{"FOO=bar"}); !has(got, "FOO=bar") {
			t.Errorf("gitNonInteractiveEnv dropped caller env: %v", got)
		}
	})
	t.Run("non-git returns extraEnv unchanged", func(t *testing.T) {
		got := gitNonInteractiveEnv("pnpm", []string{"FOO=bar"})
		if len(got) != 1 || got[0] != "FOO=bar" {
			t.Errorf("gitNonInteractiveEnv(pnpm) = %v, want [FOO=bar]", got)
		}
	})
}
