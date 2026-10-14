# second-opinion — the same issue on two harnesses, side by side

The cross-vendor demo no single harness can ship: dispatch one issue
to two harnesses (Claude Code and Codex), get two pull requests, and
compare them side by side.

## What you need

- The `backlog-sweep` example working first (host installed, one
  repository watched).
- **Both** harness CLIs installed and signed in (Claude Code and
  Codex), each passing its own login check.
- The shipping release (same binary — no extra install).

## Setup (once)

Confirm both native profiles are visible to the host:

```bash
donmai host provider list
```

You should see `claude-code` and `codex` both `ready` under
`AgentRuntime`. If one is missing, install and sign in that CLI,
then re-run `donmai host setup` and pick it as an additional
profile. The host dispatches to whichever profile a run selects;
setup never invents a profile that is not installed and
authenticated.

## Run it

Preview what one issue would dispatch to, then dispatch it twice —
once per harness:

```bash
donmai orchestrator --single ENG-42 --harness claude --dry-run
donmai orchestrator --single ENG-42 --harness claude
donmai orchestrator --single ENG-42 --harness codex
```

(Replace `ENG-42` with your issue identifier. `--dry-run` prints the
dispatch candidate without spawning anything.)

Watch both sessions work the same issue concurrently:

```bash
donmai host watch --all
```

Each session works in its own isolated workarea on its own branch
(`donmai/ENG-42-<harness>`), so the two runs never share files.
When they finish, compare the two pull requests side by side:

```bash
gh pr list --head 'donmai/ENG-42-*'
```

Review both before merging either: the point of the exercise is the
comparison. The two harnesses make different trade-offs on the same
brief — different decompositions, different test coverage, different
blind spots. Merge the stronger one, or merge one and cherry-pick
the best ideas from the other.

## Notes

- Two runs cost roughly twice one run (two sessions, two workareas).
  Keep the issue small — this workflow is at its best on a tight,
  well-specified task.
- If a harness CLI is not signed in, its dispatch fails loudly at
  admission; the host does not fall back to the other harness.
- `--harness auto` (the default) probes Claude Code first, then
  Codex. Pin `--harness claude` / `--harness codex` here so the two
  runs land on different harnesses deterministically.

## Recorded run

[`run.cast`](./run.cast) is an asciicast v2 recording of this workflow
against the shipping release: both profiles listed as ready, one issue
previewed and dispatched to each harness, both sessions watched live,
and the two resulting pull requests listed for comparison.
