# Verify runtime library changes

Use the public package boundary when a runtime package has no CLI or socket surface.

1. Create an isolated temporary Go module with `GOWORK=off` and a `replace github.com/RenseiAI/donmai => <this-worktree>` directive.
2. Write a small executable that imports only the public changed package(s) and drives the real filesystem lifecycle.
3. For workarea leases, observe acquire-before-teardown, retained leaf, explicit acknowledgement release, manager restart recovery, expiry reap, same-workarea exclusion, concurrent idempotence, and parallel independent workareas.
4. Include at least one negative probe, such as a non-semantic acknowledgement, and capture the executable's stdout.

Do not substitute unit tests or type-check commands for this runtime observation.
