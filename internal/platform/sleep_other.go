//go:build !darwin && !windows

package platform

type noopSleepGuard struct{}

func PreventSleep() (SleepGuard, error) { return noopSleepGuard{}, nil }
func (noopSleepGuard) Stop() error      { return nil }
