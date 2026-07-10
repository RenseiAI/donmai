#!/usr/bin/env bash
# Kit post-acquire hook: Go
# Runs once after the workarea is acquired and ready.
# Fetches module dependencies (NOT the base toolchain).

set -euo pipefail

WORKAREA_ROOT="${1:-$(pwd)}"
cd "$WORKAREA_ROOT"

echo "[go kit] post_acquire: downloading module dependencies..."

if command -v go &>/dev/null && [ -f go.mod ]; then
  go mod download
else
  echo "[go kit] WARNING: go toolchain or go.mod not found. Skipping download."
fi

echo "[go kit] post_acquire: done."
