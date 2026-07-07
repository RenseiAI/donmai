# Sibling context repos (`DONMAI_SIBLING_REPOS`)

The runner can materialize read-only context repositories — typically the
governing architecture corpus a repo's `AGENTS.md` expects at `../<name>` —
next to the session worktree before the agent spawns
(ADR-2026-07-07-sibling-context-repos in `donmai-architecture`).

- **Env var**: `DONMAI_SIBLING_REPOS`. Carried on the work item's `env` map
  (the daemon injects work-item env into the worker child's process env);
  plain process env works for standalone runs.
- **Format**: comma-separated entries, each `<git-url>` or `<git-url>#<ref>`.
  Example: `https://github.com/Example/docs-corpus.git#main`.
- **Placement**: each repo is shallow-cloned (`git clone --depth 1`, plus
  `--branch <ref>` when given) into `<worktree-parent>/<name>`, where `<name>`
  is the URL path basename with a trailing `.git` stripped — i.e. a sibling of
  the session worktree, reachable as `../<name>` from inside it.
- **Freshen**: an existing sibling with a `.git` gets a best-effort
  `git pull --ff-only --quiet`; on failure the stale copy is kept. A directory
  without a `.git` is left untouched (never deleted).
- **Non-fatal**: any sibling failure logs a warning and the session proceeds —
  agents fall back to cloning the repo themselves. Unsafe names (empty, `.`,
  `..`, path separators, or a collision with the worktree itself) are skipped.
