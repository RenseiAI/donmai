#!/usr/bin/env python3
"""Record the Claude Code stream-json conformance corpus with the installed CLI.

Run from the repository root: python3 scripts/record-claude-stream-json.py
Needs a logged-in `claude` CLI; each scenario is a short headless session
against a cheap model in an empty temporary directory. Claude Code publishes
no stream-json schema, so the recorded transcripts are the contract: the
conformance tests map every recorded line and assert the normalized event
invariants. Re-run after bumping the Claude Code version.

Recordings are sanitized before they are written: hook events (the
recording machine's own configuration) are dropped, the init frame keeps only
an allowlist of fields with the built-in tool list, and paths, UUIDs and
message, tool-use and request ids are replaced with stable placeholders.
"""
import json
import os
import re
import shutil
import subprocess
import tempfile
from pathlib import Path

TARGET = Path("provider/harness/clijsonl/testdata/recorded")
MODEL = "haiku"
SCENARIOS = {
    "tool_call": (["--allowedTools", "Bash"],
                  "Use the Bash tool to run: echo hello. Then reply with the single word done."),
    "task_list": (["--allowedTools", "TaskCreate,TaskUpdate,ToolSearch"],
                  "Create one task named 'read' with your task tools, mark it completed, then reply done."),
    "streamed_long_tool": (["--allowedTools", "Bash", "--include-partial-messages"],
                           "Use the Bash tool to run: sleep 6 && echo ok. Then reply with the single word done."),
}
INIT_KEYS = ("type", "subtype", "cwd", "session_id", "model", "tools", "permissionMode",
             "claude_code_version", "output_style")
UUID = re.compile(r"[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}")
IDS = re.compile(r"\b(msg|toolu|req|srvtoolu)_[A-Za-z0-9]+")


class Sanitizer:
    def __init__(self, workdir: str):
        self.workdir, self.home = workdir, os.path.expanduser("~")
        self.seen: dict[str, str] = {}

    def _stable(self, match: re.Match) -> str:
        value = match.group(0)
        if value not in self.seen:
            n = len(self.seen) + 1
            prefix = match.group(1) if match.re is IDS else None
            self.seen[value] = f"{prefix}_{n:04d}" if prefix else f"00000000-0000-4000-8000-{n:012d}"
        return self.seen[value]

    def line(self, raw: str):
        event = json.loads(raw)
        if event.get("type") == "system" and str(event.get("subtype", "")).startswith("hook_"):
            return None
        if event.get("type") == "system" and event.get("subtype") == "init":
            event = {k: event[k] for k in INIT_KEYS if k in event}
            event["tools"] = [t for t in event.get("tools", []) if "__" not in t]
            for key in ("mcp_servers", "slash_commands", "agents", "skills", "plugins"):
                event[key] = []
        text = json.dumps(event, separators=(",", ":"))
        text = text.replace(os.path.realpath(self.workdir), "/work").replace(self.workdir, "/work")
        text = text.replace(self.home, "/home/user")
        return IDS.sub(self._stable, UUID.sub(self._stable, text))


def main() -> None:
    claude = shutil.which("claude")
    if not claude:
        raise SystemExit("claude CLI not on PATH")
    version = subprocess.check_output([claude, "--version"], text=True).split()[0]
    TARGET.mkdir(parents=True, exist_ok=True)
    for name, (flags, prompt) in SCENARIOS.items():
        with tempfile.TemporaryDirectory(prefix="claude-corpus-") as workdir:
            out = subprocess.run(
                [claude, "-p", prompt, "--output-format", "stream-json", "--verbose",
                 "--model", MODEL, "--max-turns", "8", *flags],
                cwd=workdir, stdin=subprocess.DEVNULL, capture_output=True, text=True,
                check=False, timeout=300)
            sanitizer = Sanitizer(workdir)
            lines = [s for s in (sanitizer.line(l) for l in out.stdout.splitlines() if l.strip()) if s]
        if not lines or json.loads(lines[-1]).get("type") != "result":
            raise SystemExit(f"{name}: no terminal result line (exit {out.returncode}): {out.stderr[-400:]}")
        (TARGET / f"{name}.jsonl").write_text("\n".join(lines) + "\n")
    (TARGET / "RECORDING.json").write_text(json.dumps({
        "cliVersion": version, "model": MODEL, "scenarios": sorted(SCENARIOS),
        "command": "claude -p <prompt> --output-format stream-json --verbose",
    }, indent=2) + "\n")


if __name__ == "__main__":
    main()
