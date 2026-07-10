#!/usr/bin/env bash
# Kit post-acquire hook: Ruby
# Runs once after the workarea is acquired and ready.
# Installs gem dependencies (NOT the base toolchain).
#
# PATH note: rbenv shims live in ~/.rbenv/shims. Until demand.env threads that
# onto PATH for every kit command, this hook initialises rbenv itself.

set -euo pipefail

WORKAREA_ROOT="${1:-$(pwd)}"
cd "$WORKAREA_ROOT"

# Make ruby/bundle visible even if the composed env hasn't augmented PATH yet.
if [ -d "$HOME/.rbenv/bin" ]; then
  export PATH="$HOME/.rbenv/bin:$PATH"
fi
if command -v rbenv &>/dev/null; then
  eval "$(rbenv init - bash)"
fi

echo "[ruby kit] post_acquire: installing gems..."

if [ -f "Gemfile" ] && command -v bundle &>/dev/null; then
  bundle install
else
  echo "[ruby kit] No Gemfile or bundler not found. Skipping install."
fi

echo "[ruby kit] post_acquire: done."
