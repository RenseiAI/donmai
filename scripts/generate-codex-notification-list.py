#!/usr/bin/env python3
"""Regenerate the protocol inventory with the installed Codex CLI.

Run from the repository root: python3 scripts/generate-codex-notification-list.py
Experimental notifications and params are included so feature flags cannot
hide drift. Only protocol discriminants and the request param names of the
session-shaping requests the harness builds are vendored; no local config or
full schema text.
"""
import json
from pathlib import Path
import subprocess
import tempfile

# Requests whose params the harness builds from a Spec; their property names
# are vendored so a key the app-server would silently drop fails a test.
REQUESTS = ("thread/start", "thread/resume", "turn/start")

with tempfile.TemporaryDirectory(prefix="codex-schema-") as directory:
    subprocess.run([
        "codex", "app-server", "generate-json-schema", "--experimental",
        "--out", directory,
    ], check=True)
    schema = json.loads((Path(directory) / "ServerNotification.json").read_text())
    methods = sorted(v["properties"]["method"]["enum"][0] for v in schema["oneOf"])
    items = sorted(v["properties"]["type"]["enum"][0]
                   for v in schema["definitions"]["ThreadItem"]["oneOf"])
    client = json.loads((Path(directory) / "ClientRequest.json").read_text())
    requests = {}
    for variant in client["oneOf"]:
        method = variant["properties"]["method"]["enum"][0]
        if method in REQUESTS:
            name = variant["properties"]["params"]["$ref"].rsplit("/", 1)[-1]
            requests[method] = sorted(client["definitions"][name]["properties"])
    if not methods or not items or sorted(requests) != sorted(REQUESTS):
        raise ValueError("empty Codex protocol inventory")
    version = subprocess.check_output(["codex", "--version"], text=True).strip()
    target = Path("provider/harness/codex/testdata/protocol-inventory.json")
    target.parent.mkdir(parents=True, exist_ok=True)
    target.write_text(json.dumps({"generator": version, "notifications": methods,
                                 "items": items, "requestParams": requests},
                                indent=2) + "\n")
