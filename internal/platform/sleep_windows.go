//go:build windows

package platform

import (
	"errors"
	"runtime"
	"sync"
	"syscall"
)

const (
	esContinuous     = 0x80000000
	esSystemRequired = 0x00000001
)

type windowsSleepGuard struct {
	stop     chan struct{}
	finished chan struct{}
	once     sync.Once
	mu       sync.RWMutex
	err      error
}

func PreventSleep() (SleepGuard, error) {
	g := &windowsSleepGuard{stop: make(chan struct{}), finished: make(chan struct{})}
	ready := make(chan error, 1)
	go func() {
		runtime.LockOSThread()
		defer runtime.UnlockOSThread()
		kernel32 := syscall.NewLazyDLL("kernel32.dll")
		setExecutionState := kernel32.NewProc("SetThreadExecutionState")
		result, _, callErr := setExecutionState.Call(esContinuous | esSystemRequired)
		if result == 0 {
			ready <- callErr
			g.finish(callErr)
			return
		}
		ready <- nil
		<-g.stop
		result, _, callErr = setExecutionState.Call(esContinuous)
		if result == 0 {
			g.finish(callErr)
			return
		}
		g.finish(nil)
	}()
	if err := <-ready; err != nil {
		return nil, err
	}
	return g, nil
}

func (g *windowsSleepGuard) Stop() error {
	if g == nil {
		return nil
	}
	g.once.Do(func() { close(g.stop) })
	<-g.finished
	g.mu.RLock()
	err := g.err
	g.mu.RUnlock()
	if errors.Is(err, syscall.Errno(0)) {
		return nil
	}
	return err
}

func (g *windowsSleepGuard) finish(err error) {
	g.mu.Lock()
	g.err = err
	g.mu.Unlock()
	close(g.finished)
}
