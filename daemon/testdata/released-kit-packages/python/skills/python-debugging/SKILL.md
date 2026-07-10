---
name: python-debugging
description: Diagnose Python import, environment, type-check, lint, collection, and test failures. Use when pytest, mypy, ruff, or module loading fails or hangs.
license: MIT
metadata:
  author: donmai
  version: "1.0.0"
---

# Python Debugging

## Overview

Debugging Python import errors, type-checker failures, lint failures, and test
failures. Systematic diagnosis for the most common Python project failure modes.

## Triggers

Use this skill when:
- `pytest` fails, errors on collection, or hangs
- `mypy .` reports type errors that block the validate gate
- `ruff check .` reports lint violations
- `ModuleNotFoundError` / `ImportError` at runtime or import time

## Debugging Workflow

### 1. Import / Environment Errors

```
python -c "import sys; print(sys.executable)"
pip list 2>&1 | head -20
```

**Common root causes:**
- Wrong interpreter — confirm you're inside the project's `.venv`
- Dependency not installed — `uv sync` / `pip install -r requirements.txt`
- `ModuleNotFoundError` for a local package — check `pyproject.toml` packaging
  config / `src/` layout vs flat layout
- Editable install missing — `pip install -e .`

### 2. Type Errors (validate gate)

```
mypy . 2>&1 | head -40
```

**Common root causes:**
- Missing type hints on public signatures
- `Optional[...]` not narrowed before use (guard with `if x is not None:`)
- Missing third-party stubs — install `types-*` packages

### 3. Lint (validate gate)

```
ruff check . 2>&1 | head -40
```

Most ruff findings are auto-fixable: `ruff check --fix .`.

### 4. Test Failures

```
pytest -q 2>&1 | grep -E "FAILED|ERROR|assert" | head -30
```

**Common root causes:**
- Collection errors — an import at module top-level fails (fix imports first)
- Fixture not found — check `conftest.py` scope/location
- Flaky tests depending on ordering or shared mutable global state

## Notes

- `uv` lives in `~/.local/bin`. If `uv: not found`, prepend `~/.local/bin` to
  PATH (the installer wrote it there).
- Always operate inside a virtualenv — never install into the system Python.
