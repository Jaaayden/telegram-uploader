//go:build windows

package platform

import (
	"time"

	"golang.org/x/sys/windows"
)

// WaitProcessExit blocks until the process with the given id has exited or
// the timeout elapses. A process that cannot be opened is treated as gone.
func WaitProcessExit(pid int, timeout time.Duration) {
	if pid <= 0 {
		return
	}
	handle, err := windows.OpenProcess(windows.SYNCHRONIZE, false, uint32(pid))
	if err != nil {
		return
	}
	defer windows.CloseHandle(handle)
	_, _ = windows.WaitForSingleObject(handle, uint32(timeout.Milliseconds()))
}
