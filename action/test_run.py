"""Run with DONMAI_ACTION_TEST_BINARY pointing to verified Donmai v0.72.47."""
import contextlib
import io
import json
import os
import shlex
import subprocess
import sys
from pathlib import Path
import tarfile
import tempfile
import unittest
from unittest.mock import patch
import urllib.error

import run as action

HEAD, BASE = "a" * 40, "b" * 40


class ActionTests(unittest.TestCase):
    def test_metadata_launcher_ignores_untrusted_python_import_paths(self):
        action_directory = Path(__file__).resolve().parent.parent
        metadata = (action_directory / "action.yml").read_text()
        command, = [line.strip()[5:] for line in metadata.splitlines() if line.strip().startswith("run: ")]
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            marker = root / "untrusted-import"
            (root / "json.py").write_text(
                f"from pathlib import Path\nPath({str(marker)!r}).write_text('executed')\n")
            tools = root / "bin"
            tools.mkdir()
            invocation = root / "python-invocation.json"
            recorder = "import json,pathlib,sys; pathlib.Path(sys.argv[1]).write_text(json.dumps(sys.argv[2:]))"
            launcher = tools / "python3"
            launcher.write_text(
                "#!/bin/sh\n"
                f"{shlex.quote(sys.executable)} -I -c {shlex.quote(recorder)} {shlex.quote(str(invocation))} \"$@\"\n"
                f"exec {shlex.quote(sys.executable)} \"$@\"\n")
            launcher.chmod(0o700)
            env = {"PATH": str(tools), "HOME": str(root), "PYTHONPATH": str(root),
                   "ACTION_DIRECTORY": str(action_directory), "GITHUB_EVENT_NAME": "unsupported",
                   "ACTION_TOKEN": "synthetic-fixture-token"}
            result = subprocess.run(["/bin/bash", "--noprofile", "--norc", "-c", command],
                                    cwd=root, env=env, text=True, capture_output=True, timeout=10, check=False)
            self.assertEqual(result.returncode, 2, result.stdout + result.stderr)
            self.assertIn("Only pull_request events are supported", result.stdout)
            self.assertTrue(invocation.is_file(), "Action metadata did not invoke the Python launcher")
            self.assertEqual(json.loads(invocation.read_text()),
                             ["-I", str(action_directory / "action" / "run.py")])
            self.assertFalse(marker.exists(), "Action startup executed a module from untrusted PYTHONPATH")

    def test_event_identity_and_unsafe_events(self):
        with tempfile.TemporaryDirectory() as temporary:
            event = Path(temporary) / "event.json"
            event.write_text(json.dumps({"number": 7, "pull_request": {
                "head": {"sha": HEAD}, "base": {"sha": BASE, "repo": {"full_name": "example/repo"}}}}))
            env = {"GITHUB_EVENT_NAME": "pull_request", "GITHUB_REPOSITORY": "example/repo", "GITHUB_EVENT_PATH": str(event)}
            self.assertEqual(action.context(env), ("example/repo", 7, HEAD, BASE, "no-severity-high", "auto"))
            for change in ({"GITHUB_EVENT_NAME": "pull_request_target"}, {"GITHUB_REPOSITORY": "example/other"},
                           {"ACTION_GATE_POLICY": "none\n::error::forged"}, {"ACTION_COMMENT": "AUTO"},
                           {"GITHUB_SERVER_URL": "https://attacker.invalid"}):
                with self.subTest(change=change), self.assertRaises(action.ActionError):
                    action.context(env | change)

    def test_archive_checksum_is_embedded_not_downloaded(self):
        archive = io.BytesIO()
        with tarfile.open(fileobj=archive, mode="w:gz") as bundle:
            member = tarfile.TarInfo("donmai")
            member.size = 9
            bundle.addfile(member, io.BytesIO(b"untrusted"))
        with tempfile.TemporaryDirectory() as temporary:
            with patch.object(action.platform, "system", return_value="Linux"), patch.object(action.platform, "machine", return_value="x86_64"):
                with patch.object(action.urllib.request, "urlopen", return_value=io.BytesIO(archive.getvalue())):
                    with self.assertRaisesRegex(action.ActionError, "checksum"):
                        action.install(Path(temporary))
            self.assertFalse((Path(temporary) / "donmai").exists())

    def test_archive_only_extracts_regular_binary(self):
        for symlink in (False, True):
            with tempfile.TemporaryDirectory() as temporary:
                archive = io.BytesIO()
                with tarfile.open(fileobj=archive, mode="w:gz") as bundle:
                    escape = tarfile.TarInfo("../escape")
                    escape.size = 4
                    bundle.addfile(escape, io.BytesIO(b"oops"))
                    member = tarfile.TarInfo("donmai")
                    if symlink:
                        member.type, member.linkname = tarfile.SYMTYPE, "../escape"
                        bundle.addfile(member)
                    else:
                        member.size = 6
                        bundle.addfile(member, io.BytesIO(b"binary"))
                directory = Path(temporary) / "bin"
                directory.mkdir()
                if symlink:
                    with self.assertRaises(action.ActionError):
                        action.unpack(archive.getvalue(), directory)
                else:
                    self.assertEqual(action.unpack(archive.getvalue(), directory).read_bytes(), b"binary")
                self.assertFalse((Path(temporary) / "escape").exists())

    def test_annotations_escape_workflow_commands(self):
        stream = io.StringIO()
        with contextlib.redirect_stdout(stream):
            action.annotation("error", "bad%value\r\n::notice::forged")
        self.assertEqual(stream.getvalue(), "::error::bad%25value%0D%0A::notice::forged\n")

    def test_comment_updates_only_own_bot_marker(self):
        calls = []
        def request(token, path, method="GET", body=None):
            calls.append((path, method, body))
            if method == "GET":
                return [{"id": 9, "body": action.MARKER, "user": {"login": "contributor"}},
                        {"id": 10, "body": action.MARKER, "user": {"login": "github-actions[bot]"}}]
            return {}
        with patch.object(action, "api", side_effect=request):
            self.assertEqual(action.publish("test", "example/repo", 7, "safe", "auto"), "updated")
        self.assertEqual(calls[-1], ("/repos/example/repo/issues/comments/10", "PATCH", {"body": "safe"}))

    def test_comment_permissions_and_errors_are_not_silent(self):
        for mode in ("auto", "required"):
            with patch.object(action, "api", side_effect=urllib.error.HTTPError("url", 403, "forbidden", {}, None)):
                if mode == "auto":
                    self.assertEqual(action.publish("test", "example/repo", 7, "safe", mode), "unavailable")
                else:
                    with self.assertRaises(action.ActionError):
                        action.publish("test", "example/repo", 7, "safe", mode)
        with patch.object(action, "api", side_effect=AssertionError("must not call API")):
            self.assertEqual(action.publish("test", "example/repo", 7, "safe", "never"), "disabled")

    def test_snapshot_drift_is_refused(self):
        with patch.object(action, "api", return_value={"head": {"sha": "c" * 40}, "base": {"sha": BASE}}):
            with self.assertRaisesRegex(action.ActionError, "changed"):
                action.verify_snapshot("test", "example/repo", 7, HEAD, BASE)

    def test_failed_assessment_never_posts_a_clean_comment(self):
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            event = root / "event.json"
            event.write_text(json.dumps({"number": 7, "pull_request": {
                "head": {"sha": HEAD}, "base": {"sha": BASE, "repo": {"full_name": "example/repo"}}}}))
            env = {"GITHUB_EVENT_NAME": "pull_request", "GITHUB_REPOSITORY": "example/repo",
                   "GITHUB_EVENT_PATH": str(event), "ACTION_TOKEN": "secret-fixture",
                   "GITHUB_OUTPUT": str(root / "output"), "GITHUB_STEP_SUMMARY": str(root / "summary")}
            with patch.dict(os.environ, env, clear=True), patch.object(action.shutil, "which", return_value="/usr/bin/gh"), \
                    patch.object(action, "verify_snapshot") as snapshot, \
                    patch.object(action, "install", return_value=root / "binary"), \
                    patch.object(action, "assess", side_effect=action.ActionError("Complete PR diff assessment failed; no clean result is available.")), \
                    patch.object(action, "publish", return_value="posted") as publish, \
                    contextlib.redirect_stdout(io.StringIO()):
                self.assertEqual(action.main(), 2)
            snapshot.assert_called_once_with("secret-fixture", "example/repo", 7, HEAD, BASE)
            body = publish.call_args.args[3]
            self.assertIn("Assessment failed", body)
            self.assertNotIn("Policy passed", body)
            self.assertNotIn("secret-fixture", body)
            self.assertIn("result=error", (root / "output").read_text())
            self.assertNotIn("observations=", (root / "output").read_text())

    def test_raw_exception_never_becomes_a_workflow_command(self):
        stream = io.StringIO()
        with patch.object(action, "run", side_effect=ValueError("token-secret\n::notice::forged")), \
                patch.object(action, "output"), contextlib.redirect_stdout(stream):
            self.assertEqual(action.main(), 2)
        self.assertNotIn("token-secret", stream.getvalue())
        self.assertNotIn("::notice::forged", stream.getvalue())

    def test_api_does_not_follow_authenticated_redirect(self):
        with self.assertRaises(action.ActionError):
            action.NoRedirect().redirect_request(None, None, 302, "redirect", {}, "https://attacker.invalid")


class ReleasedCLI(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        binary = os.environ.get("DONMAI_ACTION_TEST_BINARY")
        if not binary or not Path(binary).is_file():
            raise RuntimeError("Set DONMAI_ACTION_TEST_BINARY to verified Donmai v0.72.47; real CLI controls are mandatory")
        cls.binary = Path(binary).resolve()

    def assess(self, policy="none", fail_diff=False, missing_patch=False):
        with tempfile.TemporaryDirectory() as temporary:
            directory = Path(temporary)
            # Metadata and patch content are hostile data, not shell fragments.
            metadata = {"title": "`@everyone`\n::error::forged", "body": "<script>bad</script>",
                        "files": [{"path": "src/auth/entry.go", "additions": 1, "deletions": 0}]}
            diff = "diff --git a/src/auth/entry.go b/src/auth/entry.go\n--- a/src/auth/entry.go\n+++ b/src/auth/entry.go\n@@ -0,0 +1 @@\n+package auth\n"
            fixture = directory / "fixture-gh"
            # Python executable fixture handles precisely gh's expected verbs.
            fixture.write_text("#!/usr/bin/env python3\nimport sys\n"
                               "if sys.argv[1:3] == ['pr','view']:\n print(" + repr(json.dumps(metadata)) + ")\n"
                               "elif sys.argv[1:3] == ['pr','diff']:\n"
                               + (" sys.exit(1)\n" if fail_diff else " print(" + repr("" if missing_patch else diff) + ")\n")
                               + "else:\n sys.exit(99)\n")
            fixture.chmod(0o700)
            return action.assess(self.binary, str(fixture), directory, "synthetic-token", "example/repo", 7, policy)

    def test_real_cli_clean_and_gated(self):
        gated, counts = self.assess()
        self.assertFalse(gated)
        self.assertGreater(sum(counts.values()), 0)
        self.assertTrue(self.assess("zero-deviations")[0])

    def test_real_cli_requires_complete_diff(self):
        for options in ({"fail_diff": True}, {"missing_patch": True}):
            with self.subTest(options=options), self.assertRaisesRegex(action.ActionError, "Complete PR diff"):
                self.assess(**options)

    def test_real_cli_untrusted_content_not_in_summary(self):
        gated, counts = self.assess()
        body = action.summary(HEAD, "none", gated, counts)
        for forbidden in ("@everyone", "::error::", "<script>", "entry.go", "synthetic-token"):
            self.assertNotIn(forbidden, body)
        self.assertIn("Native observations:", body)
        self.assertIn(HEAD, body)


if __name__ == "__main__":
    unittest.main()
