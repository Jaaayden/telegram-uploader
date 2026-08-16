//go:build darwin

package mover

import "golang.org/x/sys/unix"

// renameNoReplace atomically renames source while refusing to replace any
// existing destination entry, including a symlink.
func renameNoReplace(source, destination string) error {
	return unix.RenamexNp(source, destination, unix.RENAME_EXCL)
}
