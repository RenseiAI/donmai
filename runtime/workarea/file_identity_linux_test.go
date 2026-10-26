//go:build linux

package workarea

import "testing"

// TestFileIdentityMatchesIgnoresBirthWithoutBirthTime documents the Linux
// platform asymmetry: Linux never records birth time (see
// file_identity_linux.go), so fileIdentityMatches decides on the inode alone
// and a same-inode cross-device pair is accepted even when one side carries
// a birth time. This pins the documented behavior so a future reader does
// not assume birth protection that Linux never provides.
func TestFileIdentityMatchesIgnoresBirthWithoutBirthTime(t *testing.T) {
	stored := FileIdentity{Device: 1, Inode: 100, BirthSec: 1000, BirthNsec: 1, HasBirth: true}
	current := FileIdentity{Device: 2, Inode: 100}
	if !fileIdentityMatches(stored, current) {
		t.Fatal("Linux identity without birth must match on inode alone")
	}
	if !fileIdentityMatches(current, stored) {
		t.Fatal("birthless stored identity must match on inode alone")
	}
	plainStored := FileIdentity{Device: 1, Inode: 100}
	plainCurrent := FileIdentity{Device: 2, Inode: 100}
	if !fileIdentityMatches(plainStored, plainCurrent) {
		t.Fatal("same inode on a different device must match on Linux")
	}
	otherInode := FileIdentity{Device: 2, Inode: 101}
	if fileIdentityMatches(plainStored, otherInode) {
		t.Fatal("different inode must not match even without birth time")
	}
}
