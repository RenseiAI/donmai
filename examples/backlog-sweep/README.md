# Backlog sweep: GitHub issues → parallel agents → PRs

Use Donmai v0.72.74 or newer, Git, GitHub CLI (`gh`), asciinema (optional), and
an installed Claude Code or Codex CLI with a working subscription login. You
need a GitHub repository you own, permission to push and create PRs, and two
small open issues with explicit tasks and acceptance checks. Use a disposable
repo and a host without another Donmai daemon running on port 7734.

On macOS:

```bash
brew install RenseiAI/homebrew-tap/donmai gh asciinema
brew install --cask claude-code
claude auth login
claude --version # Sonnet 5 profiles require 2.1.197 or newer
# Or install Codex and use its file-backed subscription login:
# brew install codex
# codex -c 'cli_auth_credentials_store="file"' login
gh auth login --hostname github.com --git-protocol https
gh auth setup-git --hostname github.com
export GITHUB_TOKEN="$(gh auth token --hostname github.com)"
```

On Linux, install Donmai from its GitHub release archives and install `gh` and
your chosen harness using their official installers. Setup only offers installed,
authenticated profiles; discovery does not prove model entitlement or a successful
turn. GitHub intake uses the active `gh` login when neither `GITHUB_TOKEN` nor
`GH_TOKEN` is set. Setup and `donmai github` commands require `GITHUB_TOKEN` in their shell.
Keep credentials out of YAML and recordings.

## Configure

Clone Donmai to get the scripts and clone your demo repo. Replace `OWNER/REPO`,
`42`, and `43` below with your repository and two issue numbers. Define an
absolute path to this example checkout in every terminal:

```bash
git clone https://github.com/RenseiAI/donmai.git donmai-examples
EXAMPLES="$(pwd)/donmai-examples/examples"
gh repo clone OWNER/REPO demo
cd demo
gh label create donmai --repo OWNER/REPO --color 0E8A16 --description 'Agent work' --force
donmai host setup
```

In setup choose **2. Local file queue**, set **Max concurrent sessions** to **2**,
select your Claude Code or Codex native profile, choose `OWNER/REPO`, label
`donmai`, and the repo's existing base branch (usually `main`). Read and confirm
the displayed execution security policy: the initial host-user policy permits
ambient host credentials and broad host access. The wizard verifies repository
identity and base branch, then writes `~/.donmai/daemon.yaml` and local queue
settings. Let the wizard generate machine identity, queue path and model pairing;
do not paste a host config from another machine.

[.donmai/config.yaml](.donmai/config.yaml) is optional repository metadata. Copy
it to the demo repo's `.donmai/` only if you also use the standalone orchestrator.
The GitHub host workflow does not read it, and it cannot select labels or harnesses.

## Run and inspect

In the demo clone, keep the foreground host open:

```bash
donmai host run
```

In another terminal, set `EXAMPLES` to the same absolute examples path and make
the GitHub credential available before labeling (do this before recording):

```bash
export GITHUB_TOKEN="$(gh auth token --hostname github.com)"
bash "$EXAMPLES/backlog-sweep/label.sh" OWNER/REPO 42 43
donmai host watch --all
```

The label starts intake. With both issues eligible and capacity available, expect
two sessions with separate workareas and branches. A label is not a guarantee of
a successful agent turn. Review each completed PR, its diff and Donmai session
receipt comment before merging. Inspect workareas and host logs in another terminal:

```bash
donmai host workarea list
donmai host logs
gh pr list --repo OWNER/REPO
gh pr view 44 --repo OWNER/REPO --comments # Replace 44 with the actual PR number.
```

Session receipts carry outcome and publication evidence; host logs help diagnose
failures. Each issue is admitted once per queue, so relabeling does not retry a
failed run. Retain its receipt; use a new small issue for another demonstration.
Stop the foreground host with Ctrl-C after work completes. No service installation
or automatic merge is needed.

## Recorded run

[rehearsal.cast](rehearsal.cast) records the real label script against offline
command fixtures; it is not a vendor or TUI recording. For a live run, start
recording in the second terminal before labeling:

```bash
asciinema rec backlog-sweep.cast
bash "$EXAMPLES/backlog-sweep/label.sh" OWNER/REPO 42 43
donmai host watch --all
# Quit the watch, then show the actual completed PRs and receipts:
gh pr list --repo OWNER/REPO
gh pr view 44 --repo OWNER/REPO --comments
exit
```

Keep the cast alongside the two issue URLs, two PR URLs, selected harness/model,
Donmai version and observed outcomes. A failed session should remain visible as
such; do not present a rehearsal as overnight backlog completion.
