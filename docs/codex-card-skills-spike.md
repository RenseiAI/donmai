# Card skills load natively in Codex sessions — spike result

Status: spike complete. Native delivery is implemented for headless
sessions (`provider/harness/codex/skills.go`); interactive sessions keep
the prompt fallback (see Decision).

## Question

Do Codex sessions discover skills from `$CODEX_HOME/skills` or from
`<project>/.agents/skills/` when the project is untrusted
(`trust_level = "untrusted"`, the posture platform sessions use so the
project layer cannot widen session authority)?

## Method — the check that proved it

- Binary: `codex-cli 0.164.0`.
- Isolated `CODEX_HOME` in a scratch temp dir with a minimal config
  (`mcp_servers = {}`); a scratch git project as the working directory.
- Probe skill A (user scope):
  `$CODEX_HOME/skills/spike-home-skill/SKILL.md`, carrying a
  `name`/`description` marker pair.
- Probe skill B (repo scope):
  `<project>/.agents/skills/spike-repo-skill/SKILL.md`, carrying a
  distinct marker pair.
- Control: `<project>/AGENTS.md` with its own marker. Project
  instructions are trust-gated, so the control proves the trust bit is
  honored by the check surface: if the control does not flip between
  modes, the run proves nothing.
- Check surface: `codex debug prompt-input`, the CLI's offline renderer
  of the model-visible prompt input list, which includes the
  `<skills_instructions>` block with one line per discovered skill.
  Each mode ran with cwd set to the project and a process-local trust
  override:
  `-c 'projects={"<project>"={trust_level="<trusted|untrusted>"}}'`.

Cwd discipline matters: repo-scope roots are scanned upward from the
working directory, so a run from any other directory drops the repo
probe in both modes. One early run did exactly that and was discarded;
the behavior is itself consistent with cwd-anchored discovery.

## Result

| probe | trusted | untrusted |
|---|---|---|
| `AGENTS.md` control marker | present | absent |
| `$CODEX_HOME/skills` skill | present | present |
| `<project>/.agents/skills` skill | present | present |

The control flips while both skill probes stay present: **both
candidate paths are discovered in an untrusted project.**

Source corroboration: the CLI's skill-root resolution builds its root
list from the config-layer stack (which contributes `$CODEX_HOME/skills`
at user scope) plus a working-directory-ancestry scan for
`.agents/skills` directories, with no trust check on either input. That
matches the empirical result exactly.

Also observed while spiking (not load-bearing for the decision):

- `$HOME/.agents/skills` is scanned at user scope even under an
  isolated `CODEX_HOME` — ambient operator skills leak into isolated
  sessions. It is shared operator state, so it cannot take per-session
  delivery.
- Bundled skills are served from `$CODEX_HOME/skills/.system`; session
  delivery must not collide with that subtree.
- The `SKILL.md` parser requires YAML frontmatter with a non-empty
  `description` (a skill without one is dropped with a parse error),
  defaults a missing `name` to the directory name, rejects names over
  64 characters, and ignores unknown frontmatter keys.
- `$CODEX_HOME/skills` is marked deprecated-but-still-scanned upstream.
  No removal is announced; the preferred user path is the shared
  `$HOME/.agents/skills`, which per-session delivery cannot use.

## Decision

- Deliver card skills to the session's **isolated** `$CODEX_HOME/skills`
  as `donmai-<card-id>/SKILL.md`. Both candidate paths work, but
  `.agents/skills/` lives inside the user worktree: writing there would
  pollute the user's worktree diff (and any cleanup would race the
  session's own reads). The isolated home is per-session, outside the
  worktree, and already lifecycle-managed by the existing config
  boundary, so delivery there writes nothing into the user diff.
- **Headless** sessions load card skills natively and skip the prompt
  fold for them — native discovery keeps skill bodies out of context
  until triggered, which is the point of the native path. Each written
  file is verified by a sha256 readback, and a wiring test goes red if
  delivery is removed.
- **Interactive** sessions keep the prompt fallback. The interactive
  lane has ambient-home sub-paths where isolated delivery is
  impossible, and the runner cannot know before spawn which sub-path a
  session will take — so the prompt stays the delivery surface there.
- Tool restriction stays runner-side for every lane: skill
  `disallowedTools` are unioned into the session spec (and from there
  into the approval bridge) exactly as before. The native files carry
  only `name`/`description` plus the body.
- No fallback size cap is introduced: a native path works, so the
  cap-only-applies-if-nothing-works branch does not trigger.

## Risk carried forward

The delivery readback proves the harness wrote the files, not that the
CLI discovered them. If a future CLI removes the deprecated
`$CODEX_HOME/skills` scan, card skills for headless sessions go silent.
Revisit this decision if the CLI announces that removal.
