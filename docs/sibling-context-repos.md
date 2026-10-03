# Sibling context repos (`DONMAI_SIBLING_REPOS`)

The runner can materialize read-only context repositories, typically the
governing architecture corpus a repo's `AGENTS.md` expects at `../<name>`,
before the agent spawns (ADR-2026-07-07-sibling-context-repos, as amended by
ADR-2026-08-22-session-owned-multi-repository-workarea, in
`donmai-architecture`).

- **Env var**: `DONMAI_SIBLING_REPOS`. Carried on the work item's `env` map;
  the process env is the fallback for standalone runs.
- **Format**: comma-separated entries, each `<git-url>` or `<git-url>#<ref>`.
  Example: `https://github.com/Example/docs-corpus.git#main`. `<name>` is the
  URL path basename with a trailing `.git` stripped.
- **Placement** depends on the executor. Either way the agent finds each
  sibling at `../<name>` from its working directory.
  - An executor that attests the `session-root-v1` workarea protocol and
    `isolated-read-only-v1` enforcement gets each entry as a read-only
    `context` leaf under the session root, beside the selected repository.
    The leaf belongs to the session: it is torn down, archived and adopted
    with it, and the executor holds it read-only. An entry whose name repeats
    the primary repository's or an earlier entry's is left out.
  - Any other executor keeps the original placement: a shallow clone
    (`git clone --depth 1`, plus `--branch <ref>` when given) into
    `<worktree-parent>/<name>`, beside the session worktree. An existing
    sibling with a `.git` gets a best-effort `git pull --ff-only --quiet`;
    on failure the stale copy is kept, and a directory without a `.git` is
    left untouched. Each entry is bounded by a timeout and serialized across
    processes by a file lock beside the target
    (`<worktree-parent>/.<name>.sibling-lock`).
- **Precedence**: a work item that declares its own repositories
  (`repositoryDeclaration`) is authoritative. The variable is ignored for it,
  with a warning on the result.
- **Non-fatal**: any sibling failure logs a warning and the session proceeds.
  A context leaf that fails to clone is skipped with a warning on the result
  and recorded in the workarea's declaration; agents fall back to cloning the
  repo themselves. Unsafe names (empty, `.`, `..`, path separators, or a
  collision with the worktree itself) are skipped.
