// Package landing is the Go-native landing serializer (FD-4): it serializes the
// landing of proposed changes (pull requests / proposals) onto a trunk so
// concurrent landings never corrupt the target branch, while letting
// non-conflicting changes land in parallel.
//
// It is the execution-layer port of the legacy TypeScript merge-queue + VCS
// provider subsystem. "Merge queue" is git-specific framing; this package uses
// provider-neutral names (landing serializer, Worker, Pool, proposal, landing
// strategy) so it generalizes over commutative VCS where there is no queue.
//
// Tenant isolation (FD-4): every Redis structure is keyed by the composite
// (orgId, repoId) via Key, not repoId alone as in the legacy keyspace. Key.String
// centralizes prefix construction so the isolation property has a single audit
// point.
//
// See landing/DESIGN.md for the full TS->Go file map and the keying rationale.
package landing
