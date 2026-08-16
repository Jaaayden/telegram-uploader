//go:build windows

package mover

import "golang.org/x/sys/windows"

func renameNoReplace(source, destination string) error {
	from, err := windows.UTF16PtrFromString(source)
	if err != nil {
		return err
	}
	to, err := windows.UTF16PtrFromString(destination)
	if err != nil {
		return err
	}
	// MoveFileW fails when destination already exists; unlike os.Rename on
	// Unix it never silently replaces the destination.
	return windows.MoveFile(from, to)
}
