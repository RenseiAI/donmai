# Templates Port Audit — H (2026-05-13)

Scope: survey the TS template system in
`../agentfactory/packages/core/src/templates/` and the existing Go prompt
machinery in `prompt/`, then recommend a Go porting plan.

---

## 1. TS Template System Inventory

### 1.1 WorkflowTemplate YAML files (`defaults/`)

22 YAML files, each with `apiVersion: v1 / kind: WorkflowTemplate`:

| File | workType | Notes |
|------|----------|-------|
| acceptance.yaml | acceptance | |
| backlog-creation.yaml | backlog-creation | |
| backlog-groomer.yaml | backlog-groomer | uses {{staleDays}} |
| development.yaml | development | coordinator + solo paths |
| development-retry.yaml | development | strategy compound key |
| documentation-steward.yaml | documentation-steward | uses {{allowDirectCommits}} |
| ga-readiness.yaml | ga-readiness | |
| improvement-loop.yaml | improvement-loop | |
| inflight.yaml | inflight | |
| operational-scanner-audit.yaml | operational-scanner-audit | |
| operational-scanner-ci.yaml | operational-scanner-ci | |
| operational-scanner-vercel.yaml | operational-scanner-vercel | |
| outcome-auditor.yaml | outcome-auditor | |
| qa.yaml | qa | |
| qa-native.yaml | qa | strategy compound key |
| qa-retry.yaml | qa | strategy compound key |
| refinement.yaml | refinement | |
| refinement-context-enriched.yaml | refinement | strategy compound key |
| refinement-coordination.yaml | refinement-coordination | |
| refinement-decompose.yaml | refinement | strategy compound key |
| research.yaml | research | |
| security.yaml | security | |

Total: **22 WorkflowTemplate YAMLs** (17 distinct workTypes + 5 strategy-variant compound keys).

### 1.2 PartialTemplate YAML files (`defaults/partials/`)

20 top-level partials + 3 governor sub-partials = **23 PartialTemplate YAMLs**:

Top-level:
`agent-bug-backlog`, `architectural-context`, `cli-instructions`,
`code-intelligence-instructions`, `commit-push-pr`, `dependency-instructions`,
`human-blocker-instructions`, `ios-build-validation`, `large-file-instructions`,
`native-build-validation`, `path-scoping`, `pr-selection`, `quality-baseline`,
`repo-validation`, `scope-completion-audit`, `session-memory`,
`shared-worktree-safety`, `task-lifecycle`, `validation-hard-fail`,
`work-result-marker`.

Governor sub-directory (`partials/governor/`):
`decomposition-proposal`, `escalation-alert`, `review-request`.

### 1.3 TemplateContext variables

35 optional fields + 1 required (`identifier`), total **36 variables** in
the Zod schema. Key groups:

| Group | Variables |
|-------|-----------|
| Core | identifier, mentionContext, startStatus, completeStatus |
| Parent/sub-issue | parentContext, subIssueList |
| Strategy / escalation | cycleCount, strategy, failureSummary, attemptNumber, previousFailureReasons, totalCostUsd |
| Governor notifications | blockerIdentifier, team |
| Repo / path scoping | repository, projectPath, sharedPaths |
| Tool plugins | useToolPlugins, hasCodeIntelligence |
| Non-Node projects | linearCli, packageManager, buildCommand, testCommand, validateCommand |
| Model selection | model, subAgentModel, effort, subAgentEffort, subAgentProvider |
| Quality baseline | qualityBaseline (object: tests.{total,passed,failed}, typecheckErrors, lintErrors) |
| Context injection | sessionMemoryContext, architecturalContext |
| Misc | mergeQueueEnabled, conflictWarning, phaseOutputs, agentBugBacklog |
| Non-standard (in YAMLs) | staleDays, allowDirectCommits |

### 1.4 Handlebars features used

Across all 22 templates + 23 partials the following Handlebars features appear:

- **Variable interpolation**: `{{identifier}}`, `{{linearCli}}`, etc.
- **Conditionals**: `{{#if VAR}} … {{/if}}`, `{{#unless (eq packageManager "none")}}`
- **Iteration**: `{{#each sharedPaths}}`, `{{#each previousFailureReasons}}`
- **Partials**: `{{> partials/cli-instructions}}` (42 distinct partial invocations)
- **Custom helpers**: `eq`, `neq` (registered in TemplateRegistry constructor)
- **Built-ins**: `{{else}}`, `{{this}}`
- **Nested access**: `{{qualityBaseline.tests.total}}`, `{{qualityBaseline.lintErrors}}`

### 1.5 ToolPermissionAdapter exports (adapters.ts)

4 exported symbols:
1. `ClaudeToolPermissionAdapter` — `{ shell: "pnpm *" }` → `"Bash(pnpm:*)"`
2. `CodexToolPermissionAdapter` — shell patterns + `buildPermissionConfig()` (regex)
3. `SpringAiToolPermissionAdapter` — `"spring-tool:shell:pnpm *"`
4. `createToolPermissionAdapter(format)` — factory function

---

## 2. Existing Go `.tmpl` Files

Path: `prompt/templates/` — 4 files, **all `text/template` syntax** (Go stdlib):

| File | Purpose | Variables used |
|------|---------|----------------|
| `system_base.tmpl` | Runner identity + operating rules | `.SessionID`, `.OrganizationID`, `.ProjectName`, `.Repository`, `.Ref`, `.Append`, `.SkillAppend` |
| `user_development.tmpl` | Development work directive | `.IssueIdentifier`, `.Context`, `.ParentContext`, `.MentionContext`, `.Ref`, `.Repository` |
| `user_qa.tmpl` | QA/validation directive | `.IssueIdentifier`, `.Context`, `.ParentContext`, `.MentionContext` |
| `user_research.tmpl` | Research/story-refinement directive | `.IssueIdentifier`, `.Context`, `.ParentContext`, `.MentionContext` |

All 4 use `{{or .Field "fallback"}}` and `{{if .Field}} … {{end}}` — clean, no
custom helpers, no partials. The `prompt.Builder` renders them via
`text/template.ParseFS` (embedded FS).

The existing Go coverage is **3 of 17 work types** (development, qa,
research), with no partial support and no YAML-frontmatter tooling.

---

## 3. Gap Analysis

| Capability | TS | Go (today) |
|------------|-----|-----------|
| YAML frontmatter parsing | yes | no |
| WorkflowTemplate schema validation | yes (Zod) | no |
| Partial templates | 23 partials | 0 |
| Strategy compound keys | yes | no |
| TemplateContext (36 vars) | yes | 6 vars (builder-local structs) |
| ToolPermissionAdapter | 3 adapters | no |
| Layered template directories | yes | no (embedded only) |
| Frontend discriminator for partials | yes | no |

---

## 4. Handlebars Library Recommendation

### Options evaluated

| Library | Stars (approx) | Status | Fidelity | Notes |
|---------|---------------|--------|----------|-------|
| `text/template` (stdlib) | — | stable | low | No partials, no `#each`, no `(eq …)` subexpressions |
| `github.com/aymerick/raymond` | ~600 | maintained | high | Near-complete Handlebars 3 spec; partials, block helpers, subexpressions |
| `github.com/flosch/pongo2` | ~2.8k | maintained | low | Django/Jinja2 syntax — **incompatible** with Handlebars |

**Recommendation: `github.com/aymerick/raymond`.**

Rationale:
- The TS registry uses Handlebars `compile()` + custom helpers (`eq`, `neq`),
  `{{#if}}`, `{{#each}}`, `{{#unless (eq …)}}`, `{{> partials/…}}`. Raymond
  supports all of these natively.
- `text/template` does NOT support `{{> partial}}` or `(eq …)` subexpressions
  without significant custom plumbing that would diverge from the TS behaviour.
- `pongo2` is a completely different syntax — non-starter.
- Raymond is pure Go, no CGo, fits the module constraint.

One caveat: raymond uses `{{{triple-stash}}}` for HTML-unescaped output (same
as Handlebars). The TS registry uses `noEscape: true` globally. Replicate this
with raymond by registering string helpers that return `raymond.SafeString(v)`.

---

## 5. Suggested Phasing

### Phase H (this lane) — Scaffolding only
- [x] `templates/loader.go` — `Template` struct + `Load(path)` parsing YAML frontmatter
- [x] `templates/loader_test.go` — 5 tests on stub Load

### Phase H+1 — Raymond integration + partial loader
- Add `raymond` to go.mod
- `templates/registry.go` — `Registry` struct wrapping raymond, `registerPartial`, `render`
- Port `system_base` + `user_{development,qa,research}` from `.tmpl` to YAML (3 templates)
- Embed built-in YAMLs via `//go:embed`
- Wire raymond `eq`/`neq` helpers

### Phase H+2 — Full workType coverage
- Port remaining 14 work-type YAMLs + 22 partials to `templates/defaults/`
- Port TemplateContext Go struct (mirror of TS, 36 fields)
- Implement strategy compound-key resolution (`workType-strategy` lookup)
- Integrate with `prompt.Builder` as replacement for raw `.tmpl` path

### Phase H+3 — ToolPermissionAdapter
- `templates/adapters.go` — `ClaudeAdapter`, `CodexAdapter` (regex policy)
- Wire into `runner/` permission gating

### Phase H+4 — Layered override + frontend discriminator
- `Load` scans `.agentfactory/templates/` at repo root (override layer)
- `frontend` param filters partial loading (Linear vs generic)
- Schema validation for loaded YAMLs

---

## 6. Complexity Estimate

| Phase | Effort | Risk |
|-------|--------|------|
| H+1 (raymond + 3 templates) | ~2d | low — raymond is well-documented |
| H+2 (14 templates + 22 partials) | ~4d | medium — partial graph correctness, `(eq …)` subexpressions |
| H+3 (adapters) | ~1d | low |
| H+4 (layered override) | ~1d | low |

Total estimated port effort: **~8 engineer-days** with the scaffold from H as
the foundation.

---

*Generated by the H audit lane. Full port is deferred to H+1 … H+4 follow-up lanes.*
