//go:build !darwin && !linux

package pi

import "errors"

// processListingStrings is unavailable on this platform; the listing tests
// skip.
func processListingStrings(int) ([]string, error) {
	return nil, errors.ErrUnsupported
}

// sameUserPIDs is unavailable on this platform; the listing tests skip.
func sameUserPIDs() ([]int, error) {
	return nil, errors.ErrUnsupported
}
