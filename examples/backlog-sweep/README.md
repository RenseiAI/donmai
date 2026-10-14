# backlog-sweep — labeled issues worked overnight in parallel

One job: issues carrying the watched label are picked up, worked in
parallel in isolated workareas, and returned as pull requests with
session logs. Watch it all from the terminal dashboard.

## What you need

- The shipping release (`brew install`, `go install`, or a release
  archive — see the top-level README install section).
- A GitHub repository you can push branches and open pull requests in.
- One installed, signed-in harness CLI (Claude Code or Codex).
- `gh` authenticated (`gh auth login`), with `GITHUB_TOKEN` exported in
  the shells running setup and the host.

## Setup (once)

From a clone of your repository, run the interactive wizard and pick
**2. Local file queue**:

```bash
cd /path/to/your/repository
donmai host setup
```

The wizard asks for a native profile (an installed, authenticated
harness with a built-in host-login model), the `OWNER/REPO` to watch,
the issue label that starts intake (this example uses `donmai`), and
the base branch for new pull requests. It verifies the repository and
branch, then shows the local execution security policy before saving.

Keep the host running in the foreground for this example:

```bash
donmai host run
```

## Run it

Label a small open issue with a clear task and acceptance check, then
watch the local sessions from another terminal:

```bash
export GITHUB_TOKEN="$(gh auth token --hostname github.com)"
donmai github list-issues --repo OWNER/REPO --state open --limit 5
donmai github add-labels --repo OWNER/REPO --number 42 --labels donmai
donmai host watch --all
```

Replace `OWNER/REPO` and `42` with your repository and issue number.
If the label does not exist yet, create it first:

```bash
gh label create donmai --repo OWNER/REPO
```

Label two or three issues and they are worked in parallel — each in
its own isolated workarea (see `donmai host workarea list`), each
returning its own pull request. A run that finishes opens a pull
request and posts a session receipt comment on the issue; review both
before merging.

`host watch` is a live session view and a pure reader: quitting it
never stops the daemon or any running agent. If no work starts or no
pull request appears, check `donmai host logs` in another terminal.
A label starts intake; it does not guarantee a successful run.
Authentication failures are failures — the host does not switch
models or providers on its own. Each issue is admitted once per local
queue: relabeling a failed issue does not retry it.

## Config reference

The wizard writes the daemon config (`~/.donmai/daemon.yaml`). The
parts that matter for this workflow:

```yaml
apiVersion: donmai.dev/v2
kind: LocalDaemon
orchestrator:
  url: file:///home/you/.donmai/queue   # local file queue, no network
localRuntime:
  harness: claude-code                  # or: codex
  model: claude-opus-4-7
  modelAuthor: anthropic
  repositories:
    - repositoryId: 12345678            # immutable GitHub numeric ID
      ownerRepo: OWNER/REPO
      label: donmai                     # this label starts intake
      ref: main                         # base branch for new PRs
```

`repositoryId` is read back from GitHub by the wizard — never invent
it. One entry watches one label on one repository's default ref.

## Recorded run

[`run.cast`](./run.cast) is an asciicast v2 recording of this workflow
against the shipping release: list open issues, add the watched label,
start the host, watch two sessions work in parallel workareas, and
tail the intake log. Play it with any asciicast v2 player.
