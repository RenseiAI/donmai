// Package depprep prepares a plain checkout's dependencies through the kit
// dependency-store entries that select it, with no agent session required.
//
// It is the session-independent entry point of
// ADR-2026-11-11-session-independent-dependency-preparation, extending
// ADR-2026-10-08-kit-dependency-stores. An agent runner is one caller; a
// non-agent executor in another module is another. Both get the same fallback
// order, the same environment and confinement rules, the same time bounds and
// the same typed result. The package exposes only its own types, so a program
// outside this module can call it.
//
// # What this build does
//
// This is the keeper-absent path (ADR D10). With Config.Keeper nil, every
// subject gets session-only exposure: each selected entry fetches online into
// a per-run cache under Request.Scratch, then installs offline from it. The
// cache is discarded with the scratch directory, and nothing is shared
// between runs.
//
//   - Fetch phase: the entry's fetch command with the entry's no_code
//     settings (no lifecycle scripts, no repository-supplied manager hooks).
//     It is the only phase that may ever receive registry credentials.
//   - Install phase: the entry's offline install command, with no
//     credentials. Without a Confiner it also runs with the entry's no_code
//     settings, so package code never runs unconfined (ADR D4 rule 4). An
//     entry that declares no no_code block cannot be used unconfined and is
//     refused (miss reason confinement_unavailable).
//
// Every child process gets an environment constructed from nothing (ADR D4.1):
// PATH is Request.Toolchain then /usr/bin:/bin; HOME, TMPDIR and the XDG base
// directories are fresh directories under Request.Scratch; LANG and CI=1; the
// entry's store, import and no_code settings; and Request.ProxyEnv. Nothing is
// inherited from the calling process: no daemon token, no loader variable, no
// NODE_OPTIONS, no user manager configuration and no git credential helper.
//
// Kit commands run as argument vectors, never through a shell. Each child runs
// in its own process group, which is killed when the step's deadline passes.
//
// # What later work plugs in
//
//   - EntrySource: the composing binary's kit registry supplies entries.
//     StaticEntries serves tests and embedders with fixed entries.
//   - Config.Confiner: runs the offline install under the host's confinement
//     backend, so the repository's own manager allowlist decides which
//     lifecycle scripts run.
//   - Config.Credentials and Config.Scope: per-scope registry credentials for
//     the fetch phase. Not delivered by this build; New refuses a non-nil
//     callback rather than ignore it.
//   - Config.Keeper: the dependency keeper (snapshots, store views, background
//     fills). Not available in this build; New refuses a non-nil keeper.
//
// The keeper never changes Prepare's signature, error codes or fallback order.
package depprep
