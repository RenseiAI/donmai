# drift-check-pr — architecture review on a pull request, as a comment

Run `donmai arch` against a pull request and post the drift summary
as a PR comment. Manual invocation is fine here — no workflow file
needed. (The scheduled GitHub Action form of this check is a later
milestone; this example is the manual half.)

## What you need

- The shipping release (no daemon, no harness, no credential beyond
  PR read access).
- `gh` authenticated for the repository the PR lives in.
- Write access to the repository if you want the comment posted by
  you; read access is enough for the assessment itself.

## Run it

Assess the PR diff natively — pure regex analysis over the real diff,
no model, no datastore:

```bash
donmai arch assess https://github.com/OWNER/REPO/pull/101 --summary
```

`--summary` prints the human-readable verdict. For the machine
record (and for gates), drop the flag to get JSON. For automated
checks, add `--require-diff`: it exits 2 when the complete diff is
unavailable instead of degrading to a metadata-only assessment, so
an empty result can never look like a clean bill of health:

```bash
donmai arch assess --repository github.com/OWNER/REPO --pr 101 \
  --require-diff --gate-policy no-severity-high
echo $?
```

Exit codes: `0` clean, `1` gated (the policy tripped), `2` error
(unavailable diff, bad args, network failure). Gate policies:
`none` (report only), `no-severity-high` (block on observations at
confidence 0.65+, the default), `zero-deviations` (block on any
observation), `max:N` (block above N observations).

Save the summary to a file and post it as a PR comment:

```bash
donmai arch assess https://github.com/OWNER/REPO/pull/101 --summary > drift-comment.md
donmai github create-comment --repo OWNER/REPO --number 101 --body-file drift-comment.md
```

Replace `OWNER/REPO` and `101` with your repository and PR number.
The comment carries the assessment; the verdict stays yours — a
clean assessment means the selected policy passed on a complete
diff, not that the design is certified.

## Automating it later

The same assessment runs unattended as a GitHub Action (checksum-
pinned analyzer, job summary, check annotation, optional PR
comment). Save this as `.github/workflows/drift.yml`:

```yaml
on: pull_request
permissions: {contents: read, pull-requests: write}
jobs: {drift: {runs-on: ubuntu-latest,
  steps: [{uses: donmai-drift-action@<pinned-sha>}]}}
```

Pin the Action to a reviewed full commit SHA (see `action/README.md`
for the current pin and the `token` / `gate-policy` / `comment`
inputs). That scheduled form is tracked separately; this example
covers the manual invocation only.

## Recorded run

[`run.cast`](./run.cast) is an asciicast v2 recording of this workflow
against the shipping release: assess a PR with `--summary`, re-run
strict (`--require-diff`) and show the exit code, post the summary as
a PR comment, and show the workflow file that automates the same
check.
