# Security policy

## Report a vulnerability privately

Email [security@rensei.ai](mailto:security@rensei.ai) with suspected security
issues in Donmai. Do not open a public issue or discussion containing exploit
details, credentials, or private repository contents.

Include the affected Donmai version, operating system, reproduction steps,
expected and observed behavior, and the impact you believe is possible. Use a
minimal reproduction with synthetic data; never send live tokens or secrets.

## What to expect

Maintainers will review the report, request any information needed to reproduce
it, and coordinate remediation and public disclosure with the reporter. There
is no guaranteed response or resolution deadline. If you have not received a
reply, follow up on the same email thread.

Fixes are released through the normal release process. Check the latest release
when reporting, and include reports affecting older versions too; do not assume
an older version receives a backport.

## Scope

Donmai is a local agent runtime, CLI, and daemon. Reports may concern its command
and daemon inputs, agent execution, credentials, workarea isolation, provider
and kit handling, or release artifacts. Include the trust boundary crossed and
the access needed to reproduce the issue. No category is excluded solely
because the default deployment is local.

For non-security bugs, use the repository's public bug-report template.
