// Package repokeeper keeps one private bare git mirror per (canonical
// remote, credential scope) under <state-dir>/repo-keeper/mirrors/.
//
// A mirror is keyed by the digest of its canonical remote — the same
// userinfo/query/fragment-stripping normalization the workarea declaration
// uses for its source digest — joined with an opaque credential-scope name,
// so two scopes for one remote never share a directory and no repository
// content ever crosses a credential scope. Each mirror records the remote
// and scope it was created for; both are re-verified against the live git
// origin before every use and any mismatch fails closed.
//
// The store never handles credential values: remote operations run with
// ambient authentication only, every credential travels per-invocation
// environment (never the mirror's config file), and the listing type is
// secret-free by construction. Later consumers resolve per-scope
// credentials above this package; this package only keeps the scopes
// separate.
package repokeeper
