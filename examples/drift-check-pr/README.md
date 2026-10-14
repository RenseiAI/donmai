# Drift check: assess a PR and post its summary

Install Donmai v0.72.74 or newer, GitHub CLI (`gh`), Bash and optionally asciinema.
On macOS: `brew install RenseiAI/homebrew-tap/donmai gh asciinema`. On Linux use
Donmai's GitHub release archive and the official GitHub CLI installer. You need
read access to the PR and permission to comment on it. No agent harness, daemon,
model login or issue label is required.

```bash
gh auth login --hostname github.com --git-protocol https
git clone https://github.com/RenseiAI/donmai.git
cd donmai
```

[.donmai/config.yaml](.donmai/config.yaml) is optional repository metadata;
`arch assess` does not read it. The example explicitly supplies its gate policy
and PR URL. Leave the deprecated `DONMAI_ARCH_BIN` override unset and remove any
legacy `af-arch` executable from PATH to select native analysis.

## Assess, then publish

Replace the sample URL with your actual PR. Preview without publishing:

```bash
donmai arch assess https://github.com/OWNER/REPO/pull/44 --require-diff --summary --gate-policy no-severity-high
```

Native analysis fetches the PR diff through `gh` and checks regex-based
architectural signals. It does not compare against a learned architecture
baseline or perform an LLM review. `--require-diff` prevents a metadata-only
fallback from being mistaken for a complete check.

Post the completed assessment as a PR comment:

```bash
bash examples/drift-check-pr/comment.sh https://github.com/OWNER/REPO/pull/44
```

The script captures the summary in a temporary file and passes it with
`gh pr comment --body-file`, preserving multiline output. Exit **0** means a
completed ungated assessment and published comment. Exit **1** means a completed
gated assessment: the comment is still posted and the gate status is retained.
Exit **2** means assessment/input failure; no comment is posted. A GitHub comment
failure is also a nonzero result. An empty summary is rejected. Repeated invocation
posts another comment; the script neither updates an existing comment nor merges
or blocks the PR by itself. Review the summary and PR diff manually.

## Recorded run

[rehearsal.cast](rehearsal.cast) records a gated summary being published through
the actual script with offline command fixtures, not a live GitHub comment.
Record the live run after previewing the target PR:

```bash
asciinema rec drift-check-pr.cast
bash examples/drift-check-pr/comment.sh https://github.com/OWNER/REPO/pull/44
# Exit 1 is expected when the assessment is gated; inspect the posted comment.
gh pr view https://github.com/OWNER/REPO/pull/44 --comments
exit
```

Keep the cast, PR/comment URL, Donmai version, policy and exit status together.
