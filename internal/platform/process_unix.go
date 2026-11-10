//go:build !windows

package platform

import (
	"errors"
	"syscall"
	"time"
)

// WaitProcessExit blocks until the process with the given id has exited or
// the timeout elapses. A process that cannot be signalled is treated as gone.
func WaitProcessExit(pid int, timeout time.Duration) {
	if pid <= 0 {
		return
	}
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		// Signal 0 only checks for existence. EPERM means the process exists
		// but belongs to someone else, which cannot be our previous instance.
		if err := syscall.Kill(pid, 0); err != nil && !errors.Is(err, syscall.EINTR) {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
}
