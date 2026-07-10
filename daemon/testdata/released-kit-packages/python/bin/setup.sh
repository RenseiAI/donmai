#!/usr/bin/env bash
# Kit post-acquire hook: Python
# Runs once after the workarea is acquired and ready.
# Installs project dependencies (NOT the base toolchain).
#
# PATH note: the uv installer writes to ~/.local/bin. Until demand.env threads
# that onto PATH for every kit command, this hook prepends it itself.

set -euo pipefail

WORKAREA_ROOT="${1:-$(pwd)}"
cd "$WORKAREA_ROOT"

# Make uv visible even if the composed env hasn't augmented PATH yet.
export PATH="$HOME/.local/bin:$PATH"

echo "[python kit] post_acquire: installing dependencies..."

if [ -f "pyproject.toml" ] && command -v uv &>/dev/null; then
  uv sync
elif [ -f "requirements.txt" ]; then
  python3 -m venv .venv
  # shellcheck disable=SC1091
  . .venv/bin/activate
  pip install -r requirements.txt
elif [ -f "pyproject.toml" ]; then
  python3 -m venv .venv
  # shellcheck disable=SC1091
  . .venv/bin/activate
  pip install -e .
else
  echo "[python kit] No recognized dependency file. Skipping install."
fi

echo "[python kit] post_acquire: done."
