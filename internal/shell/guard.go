package shell

// The WebView2 binding reports every failed call to one error callback and
// then ends the process with os.Exit(1). A single refused call (moving focus
// into a view that cannot take it, for example) must not take the uploads
// down with it, so calls that can fail harmlessly run under a callGuard: the
// error callback panics with the error instead of returning, and run recovers
// it before the binding reaches its exit.

// bindingFailure carries an error out of the binding's error callback.
type bindingFailure struct{ err error }

// callGuard is used on the UI thread only.
type callGuard struct{ depth int }

// run calls fn and returns the error the binding reported while it ran.
func (g *callGuard) run(fn func()) (err error) {
	g.depth++
	defer func() {
		g.depth--
		recovered := recover()
		if recovered == nil {
			return
		}
		failure, ok := recovered.(bindingFailure)
		if !ok {
			panic(recovered)
		}
		err = failure.err
	}()
	fn()
	return nil
}

// fail is called from the binding's error callback. Inside run it does not
// return; outside it returns and the caller treats the error as fatal.
func (g *callGuard) fail(err error) {
	if g.depth > 0 {
		panic(bindingFailure{err})
	}
}
