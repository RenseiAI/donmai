#!/usr/bin/env python3
"""No checkout, no PR-derived executable input, no third-party Python packages."""
import hashlib
import io
import json
import math
import os
from pathlib import Path
import platform
import re
import shutil
import subprocess
import sys
import tarfile
import tempfile
import urllib.error
import urllib.request

VERSION = "0.72.47"
ARCHIVES = {
    ("Linux", "x86_64"): ("linux_amd64", "0c4318c0ebfe6f1f27103c2bbb2191cf5eba9f7db1806c4683598a7b8a562f7b"),
    ("Linux", "aarch64"): ("linux_arm64", "f460d3d7e45ff447973ab407b116faff69dc9bfe8f745dddd1e47d8a40a73b8d"),
    ("Darwin", "x86_64"): ("darwin_amd64", "da96669454559f830867cad1ee9c94afd9bfb9a1d5a70a8178e1fbdb4c5e3c70"),
    ("Darwin", "arm64"): ("darwin_arm64", "c9026a30f61dfc1151546d9007061ae381f9c64835c87f96856ee7170650b049"),
}
MARKER = "<!-- donmai-native-drift-check -->"
LIMIT = 256 * 1024 * 1024


class ActionError(Exception):
    """Only fixed, non-sensitive diagnostic strings may be surfaced."""


class NoRedirect(urllib.request.HTTPRedirectHandler):
    def redirect_request(self, req, fp, code, msg, headers, newurl):
        raise ActionError("GitHub API redirect refused; use the canonical repository name.")


def api(token, path, method="GET", body=None):
    # All paths are built locally from validated repository identity or integers.
    request = urllib.request.Request(
        "https://api.github.com" + path,
        data=None if body is None else json.dumps(body).encode(),
        method=method,
        headers={"Authorization": "Bearer " + token, "Accept": "application/vnd.github+json",
                 "Content-Type": "application/json", "X-GitHub-Api-Version": "2022-11-28"},
    )
    try:
        with urllib.request.build_opener(NoRedirect).open(request, timeout=30) as response:
            data = response.read(8 * 1024 * 1024 + 1)
        if len(data) > 8 * 1024 * 1024:
            raise ActionError("GitHub API response exceeded the safety limit.")
        return json.loads(data)
    except urllib.error.HTTPError:
        raise
    except (OSError, ValueError) as exc:
        raise ActionError("GitHub API request failed.") from exc


def context(env):
    if env.get("GITHUB_EVENT_NAME") != "pull_request":
        raise ActionError("Only pull_request events are supported; do not use pull_request_target.")
    if env.get("GITHUB_SERVER_URL", "https://github.com") != "https://github.com":
        raise ActionError("This Action currently supports github.com only.")
    repo = env.get("GITHUB_REPOSITORY", "")
    if not re.fullmatch(r"[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+", repo):
        raise ActionError("Invalid repository identity.")
    event = json.loads(Path(env["GITHUB_EVENT_PATH"]).read_text())
    pr = event["pull_request"]
    number = event["number"]
    if type(number) is not int or number <= 0 or pr["base"]["repo"]["full_name"] != repo:
        raise ActionError("Pull request repository identity mismatch.")
    head, base = pr["head"]["sha"], pr["base"]["sha"]
    if not all(isinstance(sha, str) and re.fullmatch(r"[0-9a-f]{40}", sha) for sha in (head, base)):
        raise ActionError("Invalid pull request commit identity.")
    policy = env.get("ACTION_GATE_POLICY", "no-severity-high")
    if not re.fullmatch(r"(?:none|no-severity-high|zero-deviations|max:[0-9]{1,6})", policy):
        raise ActionError("Invalid gate policy.")
    comment = env.get("ACTION_COMMENT", "auto")
    if comment not in ("auto", "required", "never"):
        raise ActionError("Invalid comment policy.")
    return repo, number, head, base, policy, comment


def verify_snapshot(token, repo, number, head, base):
    pr = api(token, f"/repos/{repo}/pulls/{number}")
    if pr["head"]["sha"] != head or pr["base"]["sha"] != base:
        raise ActionError("Pull request changed during this run; rerun the check.")


def install(directory):
    selected = ARCHIVES.get((platform.system(), platform.machine()))
    if not selected:
        raise ActionError("Use a Linux or macOS x64/ARM64 runner with Python 3 and gh.")
    suffix, expected = selected
    url = f"https://github.com/RenseiAI/donmai/releases/download/v{VERSION}/donmai_{VERSION}_{suffix}.tar.gz"
    # No credentials accompany the public release download. The embedded digest,
    # not a checksum downloaded alongside an archive, is the trust anchor.
    with urllib.request.urlopen(url, timeout=60) as response:
        archive = response.read(LIMIT + 1)
    if len(archive) > LIMIT or hashlib.sha256(archive).hexdigest() != expected:
        raise ActionError("Pinned release archive checksum verification failed.")
    return unpack(archive, directory)


def unpack(archive, directory):
    with tarfile.open(fileobj=io.BytesIO(archive), mode="r:gz") as bundle:
        matches = [member for member in bundle.getmembers() if member.name == "donmai"]
        if len(matches) != 1 or not matches[0].isfile() or matches[0].size > LIMIT:
            raise ActionError("Pinned archive does not contain one regular Donmai executable.")
        # Never extract paths, links, or other archive members.
        binary = directory / "donmai"
        with bundle.extractfile(matches[0]) as source:
            binary.write_bytes(source.read(LIMIT + 1))
    binary.chmod(0o700)
    return binary


def assess(binary, gh, directory, token, repo, number, policy):
    home = directory / "home"
    home.mkdir()
    (directory / "gh").symlink_to(gh)
    env = {"HOME": str(home), "TMPDIR": str(directory), "PATH": f"{directory}:/usr/bin:/bin",
           "GH_TOKEN": token, "GH_HOST": "github.com", "GH_CONFIG_DIR": str(home / ".config/gh"),
           "GH_PROMPT_DISABLED": "1", "DONMAI_STATE_HOME": str(home)}
    command = [str(binary), "arch", "assess", f"https://github.com/{repo}/pull/{number}",
               "--require-diff", "--gate-policy", policy]
    result = subprocess.run(command, cwd=home, env=env, capture_output=True, timeout=180, check=False)
    if result.returncode not in (0, 1):
        raise ActionError("Complete PR diff assessment failed; no clean result is available.")
    report = json.loads(result.stdout)
    if report.get("mode") != "native-diff-only" or type(report.get("gated")) is not bool:
        raise ActionError("Unexpected native assessment result.")
    if report["gated"] != (result.returncode == 1):
        raise ActionError("Native assessment exit status and gate result disagree.")
    observations = report.get("observations")
    if observations is None:
        observations = []
    if not isinstance(observations, list):
        raise ActionError("Invalid observation list.")
    counts = {"pattern": 0, "convention": 0, "decision": 0}
    for observation in observations:
        kind, confidence = observation.get("kind"), observation.get("confidence")
        if kind not in counts or type(confidence) not in (int, float) or not math.isfinite(confidence) or not 0 <= confidence <= 1:
            raise ActionError("Invalid native observation.")
        counts[kind] += 1
    return report["gated"], counts


def summary(head, policy, gated, counts, error=None):
    # PR titles, bodies, filenames, diff text and native summaries are never
    # interpolated. Only validated SHA/policy and locally counted integers enter.
    lines = [MARKER, "## Donmai native drift check", "", f"Commit: `{head}`", f"Policy: `{policy}`", ""]
    if error:
        lines += ["**Assessment failed. No clean result is available.**", error]
    else:
        lines += ["**Gate triggered.**" if gated else "**Policy passed.**",
                  f"Native observations: **{sum(counts.values())}** "
                  f"(patterns: {counts['pattern']}, conventions: {counts['convention']}, decisions: {counts['decision']})."]
    lines += ["", "This is regex-based diff analysis, not a learned architectural baseline or a semantic review.",
              f"Analyzer: Donmai `{VERSION}`. A passed policy does not mean there are no architectural problems."]
    return "\n".join(lines) + "\n"


def publish(token, repo, number, body, mode):
    if mode == "never":
        return "disabled"
    try:
        # Bound pagination; never edit a contributor's marker-bearing comment.
        for page in range(1, 11):
            comments = api(token, f"/repos/{repo}/issues/{number}/comments?per_page=100&page={page}")
            for comment in comments:
                if comment.get("user", {}).get("login") == "github-actions[bot]" and comment.get("body", "").startswith(MARKER):
                    comment_id = comment.get("id")
                    if type(comment_id) is not int or comment_id <= 0:
                        raise ActionError("Invalid comment identity.")
                    api(token, f"/repos/{repo}/issues/comments/{comment_id}", "PATCH", {"body": body})
                    return "updated"
            if len(comments) < 100:
                api(token, f"/repos/{repo}/issues/{number}/comments", "POST", {"body": body})
                return "posted"
        raise ActionError("Comment search exceeded its limit.")
    except urllib.error.HTTPError as exc:
        if exc.code in (403, 404) and mode == "auto":
            return "unavailable"
        raise ActionError("PR comment could not be published; check token permissions.") from exc


def output(name, value):
    if path := os.environ.get("GITHUB_OUTPUT"):
        with open(path, "a", encoding="utf-8") as stream:
            stream.write(f"{name}={value}\n")


def annotation(kind, message):
    # Workflow command data escaping is mandatory even for future diagnostics.
    safe = message.replace("%", "%25").replace("\r", "%0D").replace("\n", "%0A")
    print(f"::{kind}::{safe}")


def run():
    repo, number, head, base, policy, comment = context(os.environ)
    token = os.environ.get("ACTION_TOKEN", "")
    if not token:
        raise ActionError("A GitHub token is required to fetch the complete PR diff.")
    gh = shutil.which("gh")
    if not gh:
        raise ActionError("GitHub CLI gh is required on the runner.")
    verify_snapshot(token, repo, number, head, base)
    gated, counts, error = False, None, None
    try:
        with tempfile.TemporaryDirectory(prefix="donmai-drift-") as temporary:
            directory = Path(temporary)
            binary = install(directory)
            gated, counts = assess(binary, gh, directory, token, repo, number, policy)
        verify_snapshot(token, repo, number, head, base)
    except ActionError as exc:
        error = str(exc)
    body = summary(head, policy, gated, counts, error)
    if path := os.environ.get("GITHUB_STEP_SUMMARY"):
        with open(path, "a", encoding="utf-8") as stream:
            stream.write(body)
    result = "error" if error else ("gated" if gated else "clean")
    output("result", result)
    output("head-sha", head)
    if counts is not None and not error:
        output("observations", sum(counts.values()))
    status = publish(token, repo, number, body, comment)
    output("comment-status", status)
    if status == "unavailable":
        annotation("warning", "PR comment unavailable with this token; assessment remains in the job summary. Fork tokens are normally read-only.")
    annotation("error" if error or gated else "notice", error or f"Donmai native diff policy {result}; see job summary for observation counts.")
    return 2 if error else int(gated)


def main():
    try:
        return run()
    except ActionError as exc:
        annotation("error", str(exc))
    except Exception:
        # No raw network/subprocess/parser exception: it may contain attacker
        # content, token values, or workflow-command injection payloads.
        annotation("error", "Donmai Action failed before completing a verified assessment.")
    output("result", "error")
    return 2


if __name__ == "__main__":
    sys.exit(main())
