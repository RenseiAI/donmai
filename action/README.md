# Donmai native drift check

Analyze a pull request's complete diff without checking out or running its code.
The Action downloads Donmai **v0.72.53**, verifies the archive against a SHA-256
embedded in the Action, and runs `donmai arch assess --require-diff`.
It produces a job summary, a check annotation, and (when permitted) a PR comment.

Save these three lines as
`.github/workflows/drift.yml`:

```yaml
on: pull_request
permissions: {contents: read, pull-requests: write}
jobs: {drift: {runs-on: ubuntu-latest, steps: [{uses: RenseiAI/donmai-drift-action@a52d98dcee713278d505c482f3975695bf233e8e}]}}
```

The example uses the reviewed **full commit SHA of v1.0.1** for a stable,
reviewable pin. The executable has its own independent version/checksum pin;
changing the Action ref does not select an arbitrary executable version.
No checkout step, model account, server, daemon, or additional credential is needed.

## What it checks

This is native, regex-based diff analysis. It reports patterns, conventions and
decision signals; it does not learn an architectural baseline or run an LLM.
The policy evaluates **observations**, not semantic architecture violations.
A successful job means the selected policy passed on an available complete diff.
It does not certify a design or prove that a change has no architectural problems.

| Input | Default | Meaning |
| --- | --- | --- |
| `token` | `github.token` | `pull-requests: read` for the diff; `pull-requests: write` for comments. |
| `gate-policy` | `no-severity-high` | `none`, `no-severity-high`, `zero-deviations`, or `max:N`. |
| `comment` | `auto` | `auto`, `required`, or `never`. |

`no-severity-high` is the native CLI policy name: it gates when any observation
has confidence at least **0.65**, not a semantic severity grade. `zero-deviations`
gates on any observation; `max:N` gates above N observations; `none` reports
without gating. Invalid policies fail rather than silently disabling the check.
Missing/truncated diffs, unavailable GitHub access, changed PR commits, download
or checksum failures all fail the job. They never become a clean assessment.

Outputs are `result` (`clean`, `gated`, `error`), `observations` (absent on error),
`head-sha` (the event's head commit), and `comment-status` (`posted`, `updated`,
`disabled`, `unavailable`). A successful assessment verifies both head and base
before and after fetching the diff. The comment includes the head SHA so a later
PR update cannot make an old assessment look current.

## Forks and permissions

Use the `pull_request` event. The Action refuses `pull_request_target` and other
events. It never checks out the PR head, runs repository scripts, loads project
configuration, or evaluates text from a diff. It executes in an empty temporary
directory with isolated HOME and only the required GitHub authentication.
Python runs in isolated mode so inherited `PYTHONPATH` and user-site modules
cannot execute during Action startup.

GitHub normally gives fork PRs and Dependabot read-only tokens. With `comment:
auto`, a denied comment write produces a warning and `comment-status:
unavailable`; the assessment and job summary still run. Do not give fork code a
write token to obtain a comment. Use `comment: required` when comment publication
is an explicit requirement: unavailable permission then fails the job.
Organization policy may also restrict comments on same-repository PRs.

Only a comment carrying this Action's marker **and** authored by
`github-actions[bot]` is updated. Otherwise a new comment is created. A custom
PAT is not needed; custom-token comments may accumulate because the Action does
not edit comments by human accounts. API redirects refuse rather than forward
a credential to a new destination. The Action currently supports github.com,
Linux/macOS x64 and ARM64, Python 3 with a working CA certificate store, and `gh`; GitHub-hosted Ubuntu runners provide
these tools. Windows and GitHub Enterprise hosts are not supported yet.

Comments and annotations contain only validated commit identity, policy and
locally counted observation totals. PR titles, paths, bodies, diff content and
raw analyzer/network errors are deliberately excluded to avoid mentions, markup,
workflow-command injection, and credential disclosure.

## Maintainer verification and publication

Run the local tests with an already verified released executable:

```sh
DONMAI_ACTION_TEST_BINARY=/path/to/donmai python3 -m unittest discover -s action -v
```

The suite drives the real executable against a local fake `gh` transport: complete
diff, gated diff, unavailable diff, missing patch, and hostile PR text. Separate
controls exercise embedded checksum refusal, archive links/traversal, event and
commit identity, comment ownership/permission errors, and annotation escaping.
The test cases do not contact GitHub or post comments. The required read-only
CI test job downloads the checksum-pinned analyzer and runs this suite. The fake transport is used only by
tests; production always calls the runner's actual `gh` executable.

The repository dogfood workflow uses the immutable published Action above.
It requires no repository checkout and runs the same package other repositories
can consume. The first introducing PR used an explicit bootstrap notice; normal
pull requests now run a complete assessment.

The Action is [listed on GitHub Marketplace](https://github.com/marketplace/actions/donmai-native-drift-check)
and released from the focused [Action repository](https://github.com/RenseiAI/donmai-drift-action).
Outside-organization use and visible “Used by” dependents are separate adoption
evidence; the listing alone does not establish them. Local tests do not post
public comments.

GitHub documents [fork workflow permissions](https://docs.github.com/en/actions/reference/workflows-and-actions/events-that-trigger-workflows#pull_request)
and [composite metadata](https://docs.github.com/en/actions/reference/workflows-and-actions/metadata-syntax#runs-for-composite-actions).
