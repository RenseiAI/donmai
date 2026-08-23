package codex

import (
	"strings"
	"testing"

	"github.com/RenseiAI/donmai/agent"
)

func repositoryAuthoritySpec() agent.Spec {
	return agent.Spec{
		Cwd: "/work/root/selected", SandboxEnabled: true, SandboxLevel: agent.SandboxWorkspaceWrite,
		RepositoryAuthority: &agent.RepositoryAuthorityPolicy{
			Protocol: "session-root-v1", WorkareaRoot: "/work/root", SelectedPath: "/work/root/selected",
			MutablePaths:  []string{"/work/root/selected", "/work/root/secondary"},
			ReadOnlyPaths: []string{"/work/root/context"}, Enforcement: "isolated-read-only-v1",
		},
	}
}

func TestRepositoryAuthorityNarrowsHeadlessWritableRoots(t *testing.T) {
	policy := resolveSandboxPolicy(repositoryAuthoritySpec())
	roots, ok := policy["writableRoots"].([]string)
	if !ok || len(roots) != 2 || roots[0] != "/work/root/selected" || roots[1] != "/work/root/secondary" {
		t.Fatalf("writableRoots = %#v, want only declared mutable repositories", policy["writableRoots"])
	}
	for _, root := range roots {
		if root == "/work/root/context" {
			t.Fatal("read-only context entered writableRoots")
		}
	}
}

func TestRepositoryAuthorityNarrowsInteractiveSandboxAndAddsMutableSibling(t *testing.T) {
	spec := repositoryAuthoritySpec()
	launch, err := buildInteractiveLaunchEnv(spec, func(key string) string {
		if key == codexHooksEnv {
			return codexHooksOff
		}
		return ""
	})
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(launch.argv, "\x00")
	for _, required := range []string{`sandbox_mode="workspace-write"`, "--add-dir\x00/work/root/secondary"} {
		if !strings.Contains(joined, required) {
			t.Fatalf("interactive argv missing %q: %#v", required, launch.argv)
		}
	}
	if strings.Contains(joined, "/work/root/context") {
		t.Fatalf("read-only path was granted by interactive argv: %#v", launch.argv)
	}
}

func TestRepositoryAuthorityRefusesInteractiveHooksOutsideSandbox(t *testing.T) {
	_, err := buildInteractiveLaunchEnv(repositoryAuthoritySpec(), func(key string) string {
		if key == codexHooksEnv {
			return codexHooksInherit
		}
		return ""
	})
	if err == nil || !strings.Contains(err.Error(), "hooks") {
		t.Fatalf("buildInteractiveLaunchEnv error = %v, want hooks refusal", err)
	}
}
