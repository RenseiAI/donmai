//go:build linux

package sessionshim

// processGroupHasLiveMember resolves ProcessGroupGone's EPERM on Linux, where
// kill answers EPERM only when the group has a member this user may not
// signal, and an empty group is ESRCH. EPERM therefore already proves a live
// member, owned by another user.
func processGroupHasLiveMember(int) (bool, error) {
	return true, nil
}
