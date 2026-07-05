# code-intel eval benchmark (v1)

Canonical, git-diffable ground-truth dataset for the code-intelligence A/B eval
harness (`eval/codeintel`). Each `*.jsonl` file is one task family; each line is
an `EvalDatasetCase` (brief 06 §4.2):

```jsonc
{
  "id": "codeintel-find-symbol-donmai-001",
  "input": { "taskType": "find-symbol", "repo": "RenseiAI/donmai", "ref": "<pinned-sha>", "prompt": "…" },
  "expectedOutput": { "file": "afcli/agent_run.go", "lineRange": [70, 100] },
  "rubric": null,
  "tags": ["codeintel-eval", "find-symbol", "v1"]
}
```

## Families & ground-truth shape

| File | Family | `expectedOutput` | Graded by |
|---|---|---|---|
| `find-symbol.jsonl` | `find-symbol` | `{file, lineRange}` — the symbol's definition | structural (objective) |
| `locate-usage.jsonl` | `locate-usage` | `{files[], minRecall}` — the usage-site file set | structural (recall, objective) |
| `refactor-across-files.jsonl` | `refactor-across-files` | `null` (uses `rubric`) | LLM-judge (rubric) |
| `dedup.jsonl` | `dedup` | `{isDuplicate, file?}` — the dup verdict | structural (objective) |

## Pinned refs

- `RenseiAI/donmai` → `13a3fdd404331a8b7348bbf6474e85fe68c073f1`
- `RenseiAI/platform` → `f9bb5f16b5826bdb006ea050882ac292bb494c24`

The ground truth was derived at these SHAs; the driver provisions each workarea
at exactly the case's `ref` so a fixture stays valid even as the repos evolve.

## How the ground truth was derived (and spot-verified)

The ground truth is **objectively true regardless of the code-intel engine** —
it was derived with `git grep` / `git grep -n` against the pinned SHAs, and a
sample was verified by hand:

- **find-symbol** — `git grep -n '^func <name>'` / `'^type <name>'` /
  `'^export (function|class|interface) <name>'` gives the authoritative
  definition line. `lineRange` is a tolerance window around it (the engine
  reports the definition line off-by-one in some cases, e.g. `newAgentRunCmd`
  is at `afcli/agent_run.go:80` but `donmai code search-symbols` reports 79 —
  which is precisely why the ground truth is the hand-verified `git grep` line
  with a window, not the engine's output). The grader passes only when the
  answer names the right file **and** a line inside the window.
- **locate-usage** — `git grep -ln '<symbol>' -- '<glob>' | grep -v test`
  enumerates the usage-site file set. `minRecall` scales with set size
  (small exhaustive sets → 1.0; larger sets → 0.5–0.75 so partial-but-substantial
  recall passes). The grader computes recall over `files`.
- **dedup** — positives are lightly-edited near-copies of a real, cohesive
  function/file (e.g. `codeintel-dedup-donmai-001` is a rename-only copy of
  `FindGitRoot` in `afclient/codeintel/gitroot.go`); the `{isDuplicate:true, file}`
  verdict is true **by construction**. Negatives (`fibonacciIterative`,
  `celsiusToFahrenheit`, `triangleAreaHeron`) were confirmed absent from both
  repos with `git grep` before encoding `{isDuplicate:false}`.
- **refactor-across-files** — rubrics are grounded in the *same* real usage sets
  (a rename must touch every reference site the `git grep` enumerated). Graded
  by an LLM judge against the rubric.

`eval/codeintel/dataset_test.go` pins several of these facts (definition lines,
usage sets, dup verdicts) so a fixture edit that corrupts them fails loudly.

## Counts (v1 seed)

find-symbol 12 · locate-usage 10 · refactor-across-files 8 · dedup 8 — each
family split across both dogfood repos. This is the **v1 seed**: it meets the
floor of ≥8 tasks/family × 2 repos for a plumbing/dev run. The authoritative GA
run (brief threshold: 8–12 tasks/family × 2 repos × ≥3 trials/arm) may extend
these files; edits stay reviewable because the format is canonical JSONL and the
ground truth is `git grep`-reproducible.

## Regenerating

`find-symbol`, `locate-usage`, `refactor-across-files` are hand-authored. The
`dedup` snippets are emitted by a generator that guarantees valid JSON escaping;
see the harness lane's commit history for the generator. Validate any edit with:

```
GOWORK=off go test ./eval/codeintel/ -run TestBenchmark
```
