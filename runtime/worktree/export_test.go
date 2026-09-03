package worktree

// This file is compiled only into the package's test binary. It exposes the
// process-wide coordination registries so the external test package can assert
// on them directly — otherwise the lock registry's pruning and the base-fetch
// flight's cleanup are unobservable, and a leak there cannot be pinned.

// ParentLockRegistered reports whether the parent-lock registry still holds an
// entry for parent, under the same canonical key the production path derives.
func ParentLockRegistered(parent string) bool {
	key := canonicalParentPath(parent)
	parentWorktreeLocks.Lock()
	defer parentWorktreeLocks.Unlock()
	_, ok := parentWorktreeLocks.locks[key]
	return ok
}

// BaseFetchFlightRegistered reports whether a single-flight base fetch is
// currently registered for (parent, ref).
func BaseFetchFlightRegistered(parent, ref string) bool {
	key := canonicalParentPath(parent) + "\x00" + ref
	baseFetchFlights.Lock()
	defer baseFetchFlights.Unlock()
	_, ok := baseFetchFlights.flights[key]
	return ok
}

// AcquireParentLock exposes the parent lock to the external test package so the
// registry's acquire/release accounting can be exercised without a filesystem.
func AcquireParentLock(parent string) func() {
	return acquireParentWorktreeLock(parent)
}
