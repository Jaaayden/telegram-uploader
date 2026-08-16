package platform

// SleepGuard keeps the machine awake while a long upload is active.
type SleepGuard interface {
	Stop() error
}
