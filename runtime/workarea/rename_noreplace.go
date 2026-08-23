package workarea

// RenameNoReplace atomically moves one validated path only when the destination
// is absent. Supported release platforms provide a kernel no-replace primitive.
func RenameNoReplace(source, destination string) error {
	return renameNoReplace(source, destination)
}
