package runner

import (
	"context"
	"errors"
	"os"
	"os/exec"

	"github.com/RenseiAI/donmai/internal/kit"
)

// shellExecer is the kit.Execer used by the donmai runner: it runs each
// kit-provision command via `sh -c` with cmd.Dir set to the worktree and a
// composed env.
//
// The runner ALWAYS executes inside the box it provisions work for: on `local`
// it runs on the daemon host; on every cloud sandbox provider (e2b/modal/
// daytona/docker/kubernetes) the box's entrypoint is `donmai agent run`, so the
// runner — and therefore this shellExecer — runs in-box, against the cloned
// worktree on the box's own filesystem. clone -> kit install -> agent spawn all
// share one local filesystem, so shellExecer is correct everywhere the runner
// runs. There is NO per-provider remote kit.Execer backed by sbx.exec().
// (vercel is build-time/serverless and cannot host the runner, so kits are N/A
// there; the platform gates vercel out of runner-bearing pools.)
// See runs/2026-05-28-kits-delivery/01-cloud-execer-research.md.
type shellExecer struct {
	// baseEnv is the session env merged into every command's environment
	// (worktree-relative install scripts read PATH / HOME / proxy vars).
	// The per-command env passed to Exec (demand.Env) is overlaid on top
	// so a kit can pin e.g. PATH=$HOME/.cargo/bin:$PATH.
	baseEnv map[string]string
}

// Exec runs command in dir via `sh -c`, returning the process exit code.
// A clean run returns (0, nil); a command that ran and failed returns its
// non-zero exit code with nil error; a failure to start the process (or a
// cancelled ctx) returns a non-nil error.
func (e shellExecer) Exec(ctx context.Context, dir, command string, env map[string]string) (int, error) {
	cmd := exec.CommandContext(ctx, "sh", "-c", command) //nolint:gosec // operator-installed kit install scripts; trust-gated upstream
	cmd.Dir = dir
	cmd.Env = mergeExecEnv(e.baseEnv, env)
	if err := cmd.Run(); err != nil {
		// exec.ExitError carries the non-zero exit code of a command that
		// ran but failed — surface the code with a nil error so the
		// provisioner classifies it as a "command failed" abort (not an
		// "exec error" abort). Any other error (binary missing, ctx
		// cancelled) is a genuine exec failure.
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			return exitErr.ExitCode(), nil
		}
		return -1, err
	}
	return 0, nil
}

// mergeExecEnv flattens the daemon host env plus the supplied overlays into
// the os.Environ() KEY=VALUE form. Precedence (low → high): process env,
// then baseEnv, then per-command env. Returns nil when nothing to set so
// the child inherits the daemon env unchanged.
func mergeExecEnv(parts ...map[string]string) []string {
	merged := map[string]string{}
	for _, kv := range os.Environ() {
		for i := 0; i < len(kv); i++ {
			if kv[i] == '=' {
				merged[kv[:i]] = kv[i+1:]
				break
			}
		}
	}
	overlaid := false
	for _, p := range parts {
		for k, v := range p {
			merged[k] = v
			overlaid = true
		}
	}
	if !overlaid && len(merged) == 0 {
		return nil
	}
	out := make([]string, 0, len(merged))
	for k, v := range merged {
		out = append(out, k+"="+v)
	}
	return out
}

// compile-time assertion that shellExecer satisfies kit.Execer.
var _ kit.Execer = shellExecer{}
