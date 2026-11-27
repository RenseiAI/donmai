#!/usr/bin/env python3
"""Regenerate the vendored Codex protocol inventory and request schemas.

Run from the repository root: python3 scripts/generate-codex-notification-list.py
Experimental notifications and params are included so feature flags cannot
hide drift. Vendored: protocol discriminants, the request param names of the
session-shaping requests the harness builds, those requests' full param
schemas, and the notification schemas the event mapper's fixtures validate
against (testdata/schema/, with the generator version and Codex's Apache-2.0
LICENSE and NOTICE fetched at the matching release tag). Re-run after bumping
the Codex version; the conformance tests then validate every translated
request against the new schemas.
"""
import urllib.request
import json
from pathlib import Path
import subprocess
import tempfile

# Requests whose params the harness builds from a Spec; their property names
# are vendored so a key the app-server would silently drop fails a test.
REQUESTS = ("thread/start", "thread/resume", "turn/start")
# Their full param schemas, vendored so values (enums, nested shapes) are
# validated too, not only key names.
PARAM_SCHEMAS = ("ThreadStartParams", "ThreadResumeParams", "TurnStartParams")
# Notification schemas the mapper decodes; test fixtures validate against them
# so a fixture cannot drift from the wire shape (fileChange kind objects).
NOTIFICATION_SCHEMAS = ("ItemCompletedNotification",)
VENDORED_SCHEMAS = PARAM_SCHEMAS + NOTIFICATION_SCHEMAS
SCHEMA_DIR = Path("provider/harness/codex/testdata/schema")
LICENSE_URL = "https://raw.githubusercontent.com/openai/codex/rust-v{version}/{name}"

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
    SCHEMA_DIR.mkdir(parents=True, exist_ok=True)
    for name in VENDORED_SCHEMAS:
        param_schema = json.loads((Path(directory) / "v2" / f"{name}.json").read_text())
        (SCHEMA_DIR / f"{name}.json").write_text(
            json.dumps(param_schema, sort_keys=True, separators=(",", ":")) + "\n")
    release = version.split()[-1]
    for name in ("LICENSE", "NOTICE"):
        with urllib.request.urlopen(LICENSE_URL.format(version=release, name=name)) as response:
            (SCHEMA_DIR / name).write_bytes(response.read())
    (SCHEMA_DIR / "PROVENANCE.json").write_text(json.dumps({
        "generator": version,
        "command": "codex app-server generate-json-schema --experimental",
        "source": "https://github.com/openai/codex",
        "license": "Apache-2.0",
        "schemas": [f"v2/{name}.json" for name in VENDORED_SCHEMAS],
    }, indent=2) + "\n")
