#!/usr/bin/env python3
"""Render `go test -json` as a readable stream plus a CI step summary.

Reads a `go test -json` event stream on stdin and writes:

  * to stdout, a per-package result line in roughly the shape `go test`
    prints natively, with the full output of anything that failed;
  * to $GITHUB_STEP_SUMMARY (when set), a Markdown summary whose headline
    numbers include the SKIP COUNT.

The skip count is the point. `go test ./...` reports a skipped test the same
way it reports nothing at all — the package still says `ok` — so a test that
starts skipping (a missing binary on the runner, an env var that stopped being
set, a `t.Skip` added to dodge a flake) silently stops protecting anything and
CI stays green. Surfacing the count, and naming every skipped test with its
reason, makes that visible without anyone having to read a full log.

Exit status is 1 if any test or package failed, else 0. The workflow also runs
with `set -o pipefail`, so `go test`'s own status is authoritative; this is a
second line of defence, not the primary gate.
"""

import collections
import json
import os
import sys


def main() -> int:
    # Per-package tallies and buffered output for failure reporting.
    passed = failed = skipped = 0
    # pkg_output keeps every line in order, for dumping a failed package.
    # test_output keys the same lines by test, so a skip reason is read from
    # that test's own output rather than guessed from interleaved package
    # output — `go test -json` attributes each output event to its test, and
    # parallel tests interleave freely.
    pkg_output = collections.defaultdict(list)
    test_output = collections.defaultdict(list)
    pkg_result = {}
    pkg_elapsed = {}
    skipped_tests = []
    failed_tests = []

    for line in sys.stdin:
        line = line.strip()
        if not line:
            continue
        try:
            event = json.loads(line)
        except json.JSONDecodeError:
            # Not a JSON event (e.g. a toolchain message). Pass it through so
            # nothing is ever silently swallowed.
            print(line, flush=True)
            continue

        action = event.get("Action", "")
        pkg = event.get("Package", "")
        test = event.get("Test", "")

        if action == "output":
            out = event.get("Output", "")
            pkg_output[pkg].append(out)
            if test:
                test_output[(pkg, test)].append(out)
            continue

        if test:
            # Test-level event. Subtests count individually, which is what we
            # want: a skipped subtest is a skipped assertion.
            if action == "pass":
                passed += 1
            elif action == "fail":
                failed += 1
                failed_tests.append(f"{pkg}.{test}")
            elif action == "skip":
                skipped += 1
                reason = skip_reason(test_output.pop((pkg, test), []))
                skipped_tests.append((pkg, test, reason))
            continue

        # Package-level event.
        if action in ("pass", "fail", "skip"):
            pkg_result[pkg] = action
            pkg_elapsed[pkg] = event.get("Elapsed", 0.0)
            emit_package(pkg, action, pkg_elapsed[pkg], pkg_output.pop(pkg, []))

    # Anything left un-emitted (e.g. a build failure that never produced a
    # package result event) still gets printed rather than dropped.
    for pkg, out in pkg_output.items():
        if out:
            print(f"--- output for {pkg} ---", flush=True)
            sys.stdout.write("".join(out))

    write_summary(passed, failed, skipped, pkg_result, skipped_tests, failed_tests)
    print(
        f"\ntotals: {passed} passed, {failed} failed, {skipped} skipped "
        f"across {len(pkg_result)} packages",
        flush=True,
    )
    return 1 if failed else 0


def skip_reason(output_lines):
    """Extract the `t.Skip` message from a single test's output.

    `go test` prints the reason BEFORE the `--- SKIP:` marker, as an indented
    `foo_test.go:12: reason`. Take the last such line, which is the one
    t.Skip emitted. A bare t.SkipNow() produces no reason line at all.
    """
    reason = ""
    for line in output_lines:
        stripped = line.strip()
        if stripped.startswith("--- SKIP") or stripped.startswith("=== "):
            continue
        # `foo_test.go:12: some reason`
        head, sep, tail = stripped.partition(": ")
        if sep and ".go:" in head:
            reason = tail
    return reason


def emit_package(pkg, action, elapsed, output):
    if action == "fail":
        # Print the package's whole output so the failure is diagnosable
        # straight from the console log, exactly as `go test` would show it —
        # minus go's own trailing FAIL lines, which we reprint in one
        # canonical form (a build failure produces no such line at all).
        body = "".join(output).rstrip("\n").split("\n")
        while body and (body[-1] == "FAIL" or body[-1].startswith("FAIL\t")):
            body.pop()
        if body:
            print("\n".join(body), flush=True)
        print(f"FAIL\t{pkg}\t{elapsed:.3f}s", flush=True)
        return
    if action == "skip":
        print(f"ok  \t{pkg}\t(no test files)", flush=True)
        return
    # Preserve the coverage line `go test -cover` appends, since the workflow
    # relies on it for the coverage summary.
    coverage = ""
    for line in output:
        if "coverage:" in line:
            coverage = "\tcoverage: " + line.split("coverage:", 1)[1].strip()
            break
    print(f"ok  \t{pkg}\t{elapsed:.3f}s{coverage}", flush=True)


def write_summary(passed, failed, skipped, pkg_result, skipped_tests, failed_tests):
    path = os.environ.get("GITHUB_STEP_SUMMARY")
    if not path:
        return
    lines = [
        "## Test results",
        "",
        "| Passed | Failed | Skipped | Packages |",
        "| ---: | ---: | ---: | ---: |",
        f"| {passed} | {failed} | **{skipped}** | {len(pkg_result)} |",
        "",
    ]

    if failed_tests:
        lines += ["### Failed", ""]
        lines += [f"- `{name}`" for name in sorted(failed_tests)]
        lines.append("")

    if skipped_tests:
        lines += [
            f"### Skipped ({skipped})",
            "",
            "A skipped test is a test that is not protecting anything. Check that",
            "each of these is skipping for a reason that still applies.",
            "",
            "| Test | Reason |",
            "| --- | --- |",
        ]
        for pkg, test, reason in sorted(skipped_tests):
            short_pkg = pkg.rsplit("/", 1)[-1] if pkg else ""
            reason = (reason or "_no reason given_").replace("|", "\\|")
            lines.append(f"| `{short_pkg}.{test}` | {reason} |")
        lines.append("")
    else:
        lines += ["No tests were skipped.", ""]

    with open(path, "a", encoding="utf-8") as handle:
        handle.write("\n".join(lines) + "\n")


if __name__ == "__main__":
    sys.exit(main())
