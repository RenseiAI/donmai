#!/usr/bin/env bash
# Kit post-acquire hook: TypeScript (foundation)
# Runs once after the workarea is acquired and ready.
# Installs framework deps (NOT the base toolchain — that is the
# toolchain_install scripts' job).
#
# TODO(demand.env — cross-repo): PATH-mutating base installers are wired via
# the composed demand's `env` block end-to-end (Go Compose + platform
# composeToolchainDemand → execer). Node installs to a stable PATH so this kit
# does not need it, but later PATH-mutating kits (Rust/Python/Ruby) do.

set -euo pipefail

WORKAREA_ROOT="${1:-$(pwd)}"
cd "$WORKAREA_ROOT"

echo "[typescript kit] post_acquire: checking workarea..."

if [ ! -d "node_modules" ]; then
  echo "[typescript kit] Installing dependencies..."
  if command -v pnpm &>/dev/null && { [ -f pnpm-lock.yaml ] || [ -f pnpm-workspace.yaml ]; }; then
    pnpm install --prefer-offline
  elif command -v yarn &>/dev/null && [ -f yarn.lock ]; then
    yarn install
  elif command -v npm &>/dev/null; then
    npm install
  else
    echo "[typescript kit] WARNING: No package manager found. Skipping install."
  fi
fi

echo "[typescript kit] post_acquire: done."
