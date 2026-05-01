# prompt

Renders work-type-specific (system, user) prompt pairs for the
agentfactory-tui Go agent runner.

This is the v0.5.0 port of the legacy TS template subsystem
(`../agentfactory/packages/core/src/templates/{registry,renderer}.ts`)
boiled down to the minimum surface F.2 needs: take a `QueuedWork`
(the Redis session JSON shape the daemon's poll loop already decodes),
return two strings.

## Usage

```go
import "github.com/RenseiAI/agentfactory-tui/prompt"

var b prompt.Builder
system, user, err := b.Build(prompt.QueuedWork{
    SessionID:       "0b5e88d9-32d0-4aca-9f8c-caf82f2b399c",
    IssueIdentifier: "REN-123",
    ProjectName:     "smoke-alpha",
    OrganizationID:  "org_xxx",
    Repository:      "github.com/RenseiAI/rensei-smokes-alpha",
    Ref:             "main",
    WorkType:        string(prompt.WorkTypeDevelopment),
    PromptContext:   "<issue identifier=\"REN-123\">…</issue>",
})
```

The builder is safe for concurrent use; templates parse lazily on first
`Build` and the parsed set is read-only afterwards.

## Work types

| WorkType                | Template                | Notes                                           |
|-------------------------|-------------------------|-------------------------------------------------|
| `WorkTypeDevelopment`   | `user_development.tmpl` | Implements + opens PR. Default for unknown.     |
| `WorkTypeQA`            | `user_qa.tmpl`          | Validates against acceptance criteria.          |
| `WorkTypeResearch`      | `user_research.tmpl`    | Refines issue description; never opens PR.      |

The system prompt is the same across work types — the runner's identity
+ operating-rules block, optionally augmented by
`Builder.SystemAppend` (mirrors the legacy `RepositoryConfig.systemPrompt.append`).

## Determinism

`Builder.Build` is deterministic: given the same inputs and the same
builder state it produces byte-identical output. The golden-file tests
under `testdata/*.golden` assert this; regenerate with:

```
go test ./prompt -update
```

## Boundaries

- Pure: no shell-outs, no HTTP, no filesystem reads.
- Knows nothing about MCP servers, providers, worktrees, or the
  platform API.
- Does NOT load `.agentfactory/templates/` overrides — that is deferred
  to F.5 once a worktree-aware loader is wired.

## References

- F.1.1 §1 (prompt/ row) — locked package contract
- F.0.1 §5 — legacy TS prompt + result shapes
- Live Redis session payload (F.2.7 verification) confirms the
  `QueuedWork` shape: `issueIdentifier`, `promptContext`, `projectName`,
  `organizationId`, `workType`, `linearSessionId`, `providerSessionId`.
