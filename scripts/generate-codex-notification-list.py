#!/usr/bin/env python3
"""Regenerate the protocol inventory with the installed Codex CLI.

Run from the repository root: python3 scripts/generate-codex-notification-list.py
Experimental notifications are included so feature flags cannot hide drift.
Only protocol discriminants are vendored; no local config or full schema text.
"""
import json
from pathlib import Path
import subprocess
import tempfile

with tempfile.TemporaryDirectory(prefix="codex-schema-") as directory:
    subprocess.run([
        "codex", "app-server", "generate-json-schema", "--experimental",
        "--out", directory,
    ], check=True)
    schema = json.loads((Path(directory) / "ServerNotification.json").read_text())
    methods = sorted(v["properties"]["method"]["enum"][0] for v in schema["oneOf"])
    items = sorted(v["properties"]["type"]["enum"][0]
                   for v in schema["definitions"]["ThreadItem"]["oneOf"])
    if not methods or not items:
        raise ValueError("empty Codex protocol inventory")
    version = subprocess.check_output(["codex", "--version"], text=True).strip()
    target = Path("provider/harness/codex/testdata/protocol-inventory.json")
    target.parent.mkdir(parents=True, exist_ok=True)
    target.write_text(json.dumps({"generator": version, "notifications": methods,
                                 "items": items}, indent=2) + "\n")
