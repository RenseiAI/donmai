#!/usr/bin/env bash
# Kit post-acquire hook: Rust
# Runs once after the workarea is acquired and ready.
# Fetches crate dependencies (NOT the base toolchain).
#
# PATH note: rustup installs cargo to ~/.cargo/bin. Until demand.env threads
# that onto PATH for every kit command, this hook sources ~/.cargo/env itself.

set -euo pipefail

WORKAREA_ROOT="${1:-$(pwd)}"
cd "$WORKAREA_ROOT"

# Make cargo visible even if the composed env hasn't augmented PATH yet.
if [ -f "$HOME/.cargo/env" ]; then
  # shellcheck disable=SC1091
  . "$HOME/.cargo/env"
fi

echo "[rust kit] post_acquire: fetching crate dependencies..."

if command -v cargo &>/dev/null && [ -f Cargo.toml ]; then
  cargo fetch
else
  echo "[rust kit] WARNING: cargo or Cargo.toml not found. Skipping fetch."
fi

echo "[rust kit] post_acquire: done."
