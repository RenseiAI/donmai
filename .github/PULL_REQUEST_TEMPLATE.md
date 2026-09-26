## Change

Describe the problem, the resulting behavior, and any relevant public issue.

## Validation

List the commands run and their results. For behavior changes, include the
failing control without the fix and the passing result with it, plus smoke
evidence or the remaining smoke gap. State any skipped or unrun checks.

Required local gates: `make test`, `make test-tagged`, `make lint`, `make guard`,
and `make build`.

## Compatibility and documentation

Describe changes to commands, configuration, public Go APIs, or wire types and
their downstream impact. Link relevant documentation or changelog updates.

## Contributor credit

List outside contributors who should be credited in release notes, using their
preferred public name or GitHub handle. Do not disclose private identities.
