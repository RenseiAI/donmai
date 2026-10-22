// Package repokeeper keeps one private bare git mirror per (canonical
// remote, credential scope) under <state-dir>/repo-keeper/mirrors/.
//
// A mirror is keyed by the digest of its canonical remote — the remote
// with query and fragment stripped, joined with an opaque
// credential-scope name — so two scopes for one remote never share a
// directory and no repository content ever crosses a credential scope.
// Each mirror records the remote and scope it was created for inside the
// mirror directory; both are re-verified against the live git origin
// before every use and any mismatch fails closed. Use holds a lease on
// the mirror: the shared maintenance lock stays taken from verification
// until Release, so the verify-then-use window is closed for as long as
// the holder works.
//
// The store never handles credential values: a credential-bearing
// non-ssh source is refused before anything is written, and after a fresh
// clone the persisted origin is reset to the canonical remote (which
// carries no userinfo on those transports), so no credential reaches
// git's argv or the mirror's config file, and the listing type is
// secret-free by construction. ssh sources keep their login user in the
// canonical remote — there is no header mechanism to carry it, and a
// later fetch must connect as the recorded user. Later consumers resolve
// per-scope credentials above this package; this package only keeps the
// scopes separate.
package repokeeper
