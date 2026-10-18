# Second opinion: one issue, two vendors

This example uses **two independent hosts**, one configured for Claude Code and
one for Codex, watching the same GitHub issue. Donmai currently selects one
native harness per local host. Each host has its own machine identity and queue,
so each admits the issue once and publishes a distinct session branch and PR.
Do not switch the harness on one existing queue and expect a second admission.

## Prerequisites and configuration

On each host install Donmai v0.72.74 or newer, Git, `gh`, and optionally asciinema.
On macOS use `brew install RenseiAI/homebrew-tap/donmai gh asciinema`.
On Linux use the Donmai GitHub release archive and the official GitHub CLI
installer. Clone your disposable `OWNER/REPO` on both hosts. Both hosts need
push and PR permissions on that same repo.

Host A: install Claude Code (`brew install --cask claude-code` on macOS), run
`claude auth login`, and ensure `claude --version` is at least 2.1.197 for Sonnet 5.
Host B: install Codex (`brew install codex` on macOS), then run:

```bash
codex -c 'cli_auth_credentials_store="file"' login
codex -c 'cli_auth_credentials_store="file"' login status
```

The Codex status should say `Logged in using ChatGPT`. On **both** hosts:

```bash
gh auth login --hostname github.com --git-protocol https
gh auth setup-git --hostname github.com
export GITHUB_TOKEN="$(gh auth token --hostname github.com)"
gh repo clone OWNER/REPO demo
cd demo
donmai host setup
```

Choose **2. Local file queue**, capacity **1**, the Claude Code profile on A and
the Codex profile on B, the same `OWNER/REPO`, label **second-opinion**, and the
same existing base branch. Confirm the displayed execution security policy on
each host. Setup verifies GitHub access and writes each host's own
`~/.donmai/daemon.yaml`; installed/login checks do not prove model entitlement.
Setup and `donmai github` require `GITHUB_TOKEN` in their shell. Running host
intake uses the active `gh` login when token environment variables are absent.
Neither host should already have a daemon using port 7734.

[.donmai/config.yaml](.donmai/config.yaml) is valid optional repository metadata;
this GitHub host workflow does not consume it. Labels, harness, model pairing and
queue identity are configured by setup, never by adding ignored YAML keys.

## Dispatch the same issue

Create one small issue with a precise task and tests. Do not label it until both
hosts are configured. Its body should ask for a PR, forbid merging or closing the
issue, and explicitly say two independent implementations are being compared.
Keep the issue open and labeled until both hosts have admitted it. On each host:

```bash
donmai host run
```

From another terminal on either host, replace `42` with that issue's number:

```bash
export GITHUB_TOKEN="$(gh auth token --hostname github.com)"
gh label create second-opinion --repo OWNER/REPO --color 5319E7 --description 'Two independent implementations' --force
donmai github add-labels --repo OWNER/REPO --number 42 --labels second-opinion
```

On each host use another terminal for `donmai host watch --all`, and inspect
`donmai host logs` if work fails. Wait for each host's terminal result and session
receipt. Authentication or quota errors are failures; no vendor fallback is
implied. Stop each foreground host with Ctrl-C after completion.

## Compare the two PRs

Clone `https://github.com/RenseiAI/donmai.git` to obtain this example script.
Replace `44` and `45` with the actual PRs from A and B:

```bash
bash donmai/examples/second-opinion/compare.sh OWNER/REPO 44 45
```

This reads each PR's URL, description, branch, comments (including session
receipts) and diff without merging. Open the two URLs side by side and run each
PR's tests separately. Compare correctness, scope, tests and explanations; an
agent's completion claim does not prove its implementation works. Preserve the
same issue URL and both PR URLs, and identify which host/harness produced each.
If either run failed or produced no PR, record that outcome instead of claiming
a completed cross-vendor comparison.

## Recorded run

[rehearsal.cast](rehearsal.cast) records the comparison script with offline PR
fixtures. It contains no live vendor evidence. To record the real comparison:

```bash
asciinema rec second-opinion.cast
bash donmai/examples/second-opinion/compare.sh OWNER/REPO 44 45
exit
```

Also record `donmai host watch --all` on both hosts during dispatch, and keep the
casts with the issue/PR URLs, Donmai version and each harness/model selection.
