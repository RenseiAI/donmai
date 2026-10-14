# Showcase workflows

Start with [backlog sweep](backlog-sweep/README.md): label small GitHub issues,
let agents work in isolated workareas, and review their PRs and session receipts.
Then try [second opinion](second-opinion/README.md) to compare Claude Code and
Codex on the same issue, or [drift check](drift-check-pr/README.md) to post a native
architectural assessment on a PR.

Each README includes its own prerequisites and commands. Use a disposable
repository you own; agent execution spends your vendor quota. Nothing merges PRs.
The repository YAML files are valid optional `RepositoryConfig` examples. Host
intake, concurrency and harness selection belong to the configuration generated
by `donmai host setup`, not to that YAML.

## Recordings

Each directory contains an asciicast v2 `rehearsal.cast`, captured by executing
its script with controlled command fixtures. These are **offline script runs**:
they demonstrate argument handling, comparison output and comment publication
control flow. They do not show live vendor turns, the TUI, or successful GitHub
publication. No live vendor run is claimed. The per-example recording instructions
produce the full live evidence when an operator supplies their own demo repository
and vendor logins. Review recordings for credentials, personal data and private
repository content before sharing them.

The drift example also includes a [live release run](drift-check-pr/live.cast)
using v0.72.74, with its [published PR comment](https://github.com/RenseiAI/donmai/pull/943#issuecomment-6169929675)
read back from GitHub. Backlog and second opinion still need operator-supplied live
vendor recordings.

Replay with `asciinema play examples/backlog-sweep/rehearsal.cast` (and similarly
for the other two). Regenerate the fixture recordings and run the script tests:

```bash
GOWORK=off go test -race ./examples
DONMAI_RECORD_EXAMPLES=1 GOWORK=off go test -count=1 ./examples -run TestRecordedRehearsals
```
