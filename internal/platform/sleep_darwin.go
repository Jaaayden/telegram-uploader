//go:build darwin

package platform

import (
	"errors"
	"os"
	"os/exec"
	"strconv"
	"sync"
)

type caffeinateGuard struct {
	cmd  *exec.Cmd
	once sync.Once
	err  error
}

// PreventSleep uses the macOS system caffeinate utility. It watches the app
// process as an additional safety net and is explicitly stopped with Stop.
func PreventSleep() (SleepGuard, error) {
	cmd := exec.Command("/usr/bin/caffeinate", "-i", "-w", strconv.Itoa(os.Getpid()))
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	return &caffeinateGuard{cmd: cmd}, nil
}

func (g *caffeinateGuard) Stop() error {
	g.once.Do(func() {
		if g.cmd == nil || g.cmd.Process == nil {
			return
		}
		killErr := g.cmd.Process.Kill()
		waitErr := g.cmd.Wait()
		if killErr != nil && !errors.Is(killErr, os.ErrProcessDone) {
			g.err = killErr
			return
		}
		var exitErr *exec.ExitError
		if waitErr != nil && !errors.As(waitErr, &exitErr) {
			g.err = waitErr
		}
	})
	return g.err
}
