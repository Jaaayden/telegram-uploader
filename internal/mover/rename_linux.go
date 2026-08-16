//go:build linux

package mover

import (
	"errors"
	"os"

	"golang.org/x/sys/unix"
)

func renameNoReplace(source, destination string) error {
	err := unix.Renameat2(unix.AT_FDCWD, source, unix.AT_FDCWD, destination, unix.RENAME_NOREPLACE)
	if !errors.Is(err, unix.ENOSYS) && !errors.Is(err, unix.EINVAL) && !errors.Is(err, unix.EOPNOTSUPP) {
		return err
	}
	// Older kernels/filesystems may not implement renameat2. A hard-link plus
	// unlink preserves the no-replace property for the regular files accepted
	// by this package.
	if err := os.Link(source, destination); err != nil {
		return err
	}
	if err := os.Remove(source); err != nil {
		_ = os.Remove(destination)
		return err
	}
	return nil
}
