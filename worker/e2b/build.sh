#!/usr/bin/env bash
# Rebuild the donmai e2b sandbox template (donmai-worker).
#
# 1. Cross-compiles the donmai binary for linux/amd64 (e2b sandboxes are x86_64
#    linux) into this directory as the e2b build-context blob.
# 2. Runs `e2b template create` against e2b.Dockerfile / e2b.toml.
#
# Requires:
#   - go (matching donmai's go.mod toolchain)
#   - e2b CLI (>= 2.x)
#   - a running Docker daemon (v1 `e2b template build` builds the image locally
#     before pushing)
#   - E2B_ACCESS_TOKEN in the environment, or `e2b auth login` already done.
#     NOTE: this is the CLI/CD *management* token from
#     https://e2b.dev/dashboard?tab=personal — NOT the runtime E2B_API_KEY the
#     SDK uses to spawn sandboxes. The management API 401s on E2B_API_KEY.
set -euo pipefail

if ! docker info >/dev/null 2>&1; then
  echo "ERROR: Docker daemon is not running (required by 'e2b template build')." >&2
  echo "       Start Docker Desktop and retry." >&2
  exit 1
fi
if [ -z "${E2B_ACCESS_TOKEN:-}" ] && [ ! -f "$HOME/.e2b/config.json" ]; then
  echo "ERROR: not authenticated with the e2b CLI." >&2
  echo "       Run 'e2b auth login' or export E2B_ACCESS_TOKEN" >&2
  echo "       (from https://e2b.dev/dashboard?tab=personal)." >&2
  exit 1
fi

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$HERE/../.." && pwd)"
VERSION="${DONMAI_VERSION:-v0.10.0}"

echo ">> building donmai $VERSION (linux/amd64) -> $HERE/donmai"
( cd "$REPO_ROOT" && \
  GOOS=linux GOARCH=amd64 CGO_ENABLED=0 \
  go build -trimpath -ldflags "-s -w -X main.version=$VERSION" \
    -o "$HERE/donmai" ./cmd/donmai )
chmod +x "$HERE/donmai"

echo ">> building e2b template 'donmai-worker'"
# e2b CLI 2.10.2: `template create <template-name>` reads e2b.Dockerfile from
# the cwd and builds + pushes it. The name is a POSITIONAL argument (there is
# no -n flag). A start command (`-c`, keep-alive — the platform provider
# launches `donmai agent run` itself via launchRunnerInline) REQUIRES a matching
# ready command (`--ready-cmd`, no short flag); `true` (always ready) suffices
# for a keep-alive sandbox.
# Older CLIs used `template build --name <name>`; swap if your CLI differs.
( cd "$HERE" && e2b template create donmai-worker \
    -d e2b.Dockerfile \
    -c "sleep infinity" \
    --ready-cmd "true" "$@" )

echo ">> done. See e2b.toml for the resulting template_id."
